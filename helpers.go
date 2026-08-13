package simdhttp

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/sebishogun/simdhttp/http1"
	"github.com/sebishogun/simdjson"
)

// Concrete helpers over the standard request and response. Every one of them
// is a plain function on the standard types: nothing here wraps a request,
// introduces a context key, or hands back a type the ecosystem does not know.
//
// JSON goes through simdjson, never encoding/json -- a dependency test asserts
// the whole non-test graph is clean, so the choice cannot rot back.

// DefaultMaxJSONBody bounds JSONDecode's read. A body arriving from the wire
// has no length the server should trust, and decoding an unbounded one is the
// denial of service that costs nothing to send.
const DefaultMaxJSONBody = 1 << 20

var jsonBufs = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

// JSON encodes v and writes it with the given status.
//
// The value is encoded before the status is written, so an encoding failure is
// still a 500 the caller can send: writing the header first would commit to a
// success the body then contradicts.
func JSON(w http.ResponseWriter, status int, v any) error {
	bp := jsonBufs.Get().(*[]byte)
	defer func() {
		*bp = (*bp)[:0]
		jsonBufs.Put(bp)
	}()
	out, err := simdjson.MarshalTo((*bp)[:0], v)
	if err != nil {
		return fmt.Errorf("simdhttp: encoding the response: %w", err)
	}
	*bp = out
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(status)
	_, err = w.Write(out)
	return err
}

// JSONDecode reads the request body into v, bounded by DefaultMaxJSONBody.
func JSONDecode(req *http.Request, v any) error {
	return JSONDecodeLimit(req, v, DefaultMaxJSONBody)
}

// JSONDecodeLimit reads at most max bytes of the body into v. A body past the
// limit reports http1.ErrBodyTooLarge, which the error adapter maps to 413.
func JSONDecodeLimit(req *http.Request, v any, max int64) error {
	if req.Body == nil {
		return http1.ErrIncomplete
	}
	// One byte past the limit, so exceeding it is observable rather than a
	// silently truncated document that happens to parse.
	body, err := io.ReadAll(io.LimitReader(req.Body, max+1))
	if err != nil {
		return fmt.Errorf("simdhttp: reading the body: %w", err)
	}
	if int64(len(body)) > max {
		return http1.ErrBodyTooLarge
	}
	if err := simdjson.Unmarshal(body, v); err != nil {
		return fmt.Errorf("simdhttp: decoding the body: %w", err)
	}
	return nil
}

// Param returns a route parameter, sugar over the standard PathValue.
func Param(req *http.Request, name string) string { return req.PathValue(name) }

// Query returns the first value for a query parameter.
//
// It scans the raw query rather than calling req.URL.Query(), which builds a
// map of every parameter and unescapes every value to answer one question. A
// handler reading two parameters would pay for that twice.
func Query(req *http.Request, name string) string {
	if req.URL == nil {
		return ""
	}
	v, _ := queryLookup(req.URL.RawQuery, name)
	return v
}

// QueryHas reports whether a parameter is present, including when it is
// present with an empty value -- "?debug" and "?debug=" are both answers.
func QueryHas(req *http.Request, name string) bool {
	if req.URL == nil {
		return false
	}
	_, ok := queryLookup(req.URL.RawQuery, name)
	return ok
}

// QueryInt returns a parameter parsed as an int, or def when it is absent or
// unparseable. A malformed number is not silently zero.
func QueryInt(req *http.Request, name string, def int) int {
	v, ok := queryLookup(req.URL.RawQuery, name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// QueryBool returns a parameter parsed as a bool, or def. It accepts what
// strconv.ParseBool accepts, plus a bare parameter with no value ("?debug"),
// which reads as true.
func QueryBool(req *http.Request, name string, def bool) bool {
	v, ok := queryLookup(req.URL.RawQuery, name)
	if !ok {
		return def
	}
	if v == "" {
		return true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// queryLookup finds the first value for name in a raw query string, reporting
// whether the key was present at all. Only the matching value is unescaped, so
// a query with many parameters costs one decode rather than all of them.
func queryLookup(raw, name string) (string, bool) {
	for raw != "" {
		var pair string
		if i := strings.IndexAny(raw, "&;"); i >= 0 {
			pair, raw = raw[:i], raw[i+1:]
		} else {
			pair, raw = raw, ""
		}
		if pair == "" {
			continue
		}
		key, val := pair, ""
		if i := strings.IndexByte(pair, '='); i >= 0 {
			key, val = pair[:i], pair[i+1:]
		}
		// Keys can be escaped too; compare cheaply first and only decode when
		// the escaped form could still match.
		if key != name {
			if strings.IndexByte(key, '%') < 0 && strings.IndexByte(key, '+') < 0 {
				continue
			}
			dk, ok := unescapeQuery(key)
			if !ok || dk != name {
				continue
			}
		}
		dv, ok := unescapeQuery(val)
		if !ok {
			// A malformed escape drops the pair and the scan continues, which
			// is what net/url's ParseQuery does; the accessors are pinned
			// against it.
			continue
		}
		return dv, true
	}
	return "", false
}

// unescapeQuery resolves percent-escapes and '+' as a space, which is the
// query-string rule -- a path keeps its plus (see decodeSegment). It reports
// false for a malformed escape, which is how the caller knows to drop the pair
// the way net/url does.
func unescapeQuery(s string) (string, bool) {
	if strings.IndexByte(s, '%') < 0 && strings.IndexByte(s, '+') < 0 {
		return s, true
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '+':
			out = append(out, ' ')
		case s[i] == '%':
			if i+2 >= len(s) || !ishex(s[i+1]) || !ishex(s[i+2]) {
				return "", false
			}
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
		default:
			out = append(out, s[i])
		}
	}
	return string(out), true
}

// Form returns a form value, parsing the body once. It is the standard
// ParseForm underneath, because a form body is not a hot path and the standard
// parser is what applications already expect.
func Form(req *http.Request, name string) string {
	if req.Form == nil {
		if err := req.ParseForm(); err != nil {
			return ""
		}
	}
	return req.Form.Get(name)
}
