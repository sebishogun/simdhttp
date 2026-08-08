// Package simdhttp parses HTTP/1.1 request heads with vector scans.
//
// The shape is the one simdjson proved on JSON: classify bytes a register
// at a time, then walk the classification instead of the bytes. An HTTP
// head is even simpler than JSON -- lines end at CRLF, a header name ends
// at the colon, and the whole head ends at CRLFCRLF -- so the scan is
// IndexByte and IndexAnyOrLess over the shipped kernels, and the walk
// touches each boundary once.
//
// The contract: anything net/http's reader rejects, this rejects, and
// accepted requests carry byte-identical fields with OWS trimmed. In one
// place this is deliberately stricter than net/textproto: a field name
// must be an RFC 9110 token, so a space inside a name -- which the
// standard reader tolerates and request-smuggling attacks lean on -- is
// malformed here, and so is a bare LF line ending, which the standard
// reader also accepts and which is the other half of the same class of
// attack. Both require CRLF.
package simdhttp

import (
	"errors"

	"github.com/sebishogun/simd"
)

// Request is a parsed request head. All fields alias the input buffer;
// the caller owns the bytes and their lifetime.
type Request struct {
	Method  []byte
	Target  []byte
	Proto   []byte
	Headers []Header
}

// Header is one name: value pair, aliasing the input.
type Header struct {
	Name  []byte // as written, not canonicalized; see CanonicalName
	Value []byte // OWS-trimmed
}

var (
	// ErrIncomplete means the head has no terminating blank line yet:
	// read more and call again.
	ErrIncomplete = errors.New("simdhttp: incomplete head")
	// ErrMalformed means the bytes cannot be an HTTP/1.x head.
	ErrMalformed = errors.New("simdhttp: malformed head")
)

// Parse parses one request head from b and returns it with the number of
// bytes consumed, including the terminating blank line. Headers is
// appended into req.Headers, which the caller may pre-size and reuse.
func Parse(req *Request, b []byte) (consumed int, err error) {
	// The request line: METHOD SP TARGET SP PROTO CRLF.
	nl := simd.IndexByte(b, '\n')
	if nl < 0 {
		return 0, ErrIncomplete
	}
	line := b[:nl]
	if len(line) == 0 || line[len(line)-1] != '\r' {
		return 0, ErrMalformed
	}
	line = line[:len(line)-1]
	sp1 := simd.IndexByte(line, ' ')
	if sp1 <= 0 {
		return 0, ErrMalformed
	}
	sp2 := simd.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return 0, ErrMalformed
	}
	sp2 += sp1 + 1
	req.Method = line[:sp1]
	req.Target = line[sp1+1 : sp2]
	req.Proto = line[sp2+1:]
	if len(req.Target) == 0 || len(req.Proto) == 0 {
		return 0, ErrMalformed
	}
	for _, c := range req.Method {
		if !isTokenByte(c) {
			return 0, ErrMalformed
		}
	}

	req.Headers = req.Headers[:0]
	pos := nl + 1
	for {
		rest := b[pos:]
		nl := simd.IndexByte(rest, '\n')
		if nl < 0 {
			return 0, ErrIncomplete
		}
		line := rest[:nl]
		if len(line) == 0 || line[len(line)-1] != '\r' {
			return 0, ErrMalformed
		}
		line = line[:len(line)-1]
		pos += nl + 1
		if len(line) == 0 {
			return pos, nil // the blank line: head complete
		}
		colon := simd.IndexByte(line, ':')
		if colon <= 0 {
			return 0, ErrMalformed
		}
		name := line[:colon]
		// A field name is a token: one vector scan for anything outside
		// the token set replaces the byte loop.
		if simd.IndexNotAny(name, tokenSet) >= 0 {
			return 0, ErrMalformed
		}
		val := line[colon+1:]
		for len(val) > 0 && (val[0] == ' ' || val[0] == '\t') {
			val = val[1:]
		}
		for len(val) > 0 && (val[len(val)-1] == ' ' || val[len(val)-1] == '\t') {
			val = val[:len(val)-1]
		}
		// Field values may not contain bare CR or other controls except
		// HTAB: one scan for a byte below 0x20 that is not tab.
		if i := simd.IndexAnyOrLess(val, "\x7f", 0x20); i >= 0 && val[i] != '\t' {
			return 0, ErrMalformed
		}
		req.Headers = append(req.Headers, Header{Name: name, Value: val})
	}
}

// tokenSet is RFC 9110's token alphabet.
const tokenSet = "!#$%&'*+-.^_`|~0123456789" +
	"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func isTokenByte(c byte) bool {
	return c > 0x20 && c < 0x7f && simd.IndexByte(tokenSet, c) >= 0
}
