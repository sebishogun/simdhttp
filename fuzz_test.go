package simdhttp

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// FuzzParseAgainstNetHTTP asserts the one direction that is a security
// contract: simdhttp must never accept a head that net/http rejects. A
// parser in front of an origin server that is MORE permissive than the
// origin is how a smuggled request slips through -- so the reverse,
// simdhttp rejecting what net/http accepts, is a feature (it is stricter
// on CRLF, token names and the 1.x version line) and not checked here.
// When both accept, the request line must still agree.
func FuzzParseAgainstNetHTTP(f *testing.F) {
	for _, s := range []string{
		"GET / HTTP/1.1\r\nHost: x\r\n\r\n",
		"POST /a HTTP/1.1\r\nH: v\r\nH2:  w \r\n\r\n",
		"bad", "GET\r\n\r\n", "\r\n",
		"0 0 0\r\n\r\n", "0 * HTTP/0.0\r\n\n",
	} {
		f.Add([]byte(s))
	}
	var req Request
	f.Fuzz(func(t *testing.T, data []byte) {
		_, ourErr := Parse(&req, data)
		hr, hErr := http.ReadRequest(bufio.NewReader(strings.NewReader(string(data))))
		if hErr != nil && ourErr == nil {
			// simdhttp validates message framing (RFC 9112), not URI
			// semantics -- interpreting the target is the caller's job with
			// net/url. When net/http's rejection is only that the target is
			// not a parseable request-URI, that is out of simdhttp's
			// contract, not a framing disagreement. Everything else is.
			if u := requestTarget(req); u != "" {
				if _, e := url.ParseRequestURI(u); e != nil {
					return
				}
			}
			t.Fatalf("net/http rejects (%v), simdhttp accepts: %q", hErr, data)
		}
		if hErr == nil && ourErr == nil && string(req.Method) != hr.Method {
			t.Fatalf("both accept but method %q vs %q: %q", req.Method, hr.Method, data)
		}
	})
}

func requestTarget(r Request) string {
	if r.Target == nil {
		return ""
	}
	return string(r.Target)
}

// FuzzRouterMatch: random paths and methods against a built table.
//
// docs/verification.md has named this target since the v1.2.0 documentation
// commit and no test of that name has ever existed in this repository — the
// guarantee was described, in the document that lists what the suite checks,
// and never written. Swept 2026-08-16 across the whole family; this was the
// only one.
//
// The two properties the document claims, and what each is worth:
//
//   - NO PANIC. The trie walk indexes a fixed-size inline array of parameter
//     values and slices path segments by offset, so a path shape the builder
//     did not anticipate is an index bug rather than a wrong answer.
//   - DETERMINISTIC PARAMS. Two identical requests must select the same route
//     and bind the same values. A router that is right on average is a router
//     whose handler sees a different id on a retry.
//
// The table is built ONCE, outside f.Fuzz: it is the subject, and rebuilding
// it per input would fuzz the builder instead of the match.
func FuzzRouterMatch(f *testing.F) {
	r := New()
	seen := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Route", name)
			for _, p := range []string{"id", "*", "a", "b"} {
				if v := req.PathValue(p); v != "" {
					w.Header().Add("X-Param", p+"="+v)
				}
			}
		}
	}
	for _, rt := range []struct{ method, pattern, name string }{
		{"GET", "/", "root"},
		{"GET", "/users", "users"},
		{"GET", "/users/{id}", "user"},
		{"POST", "/users/{id}", "user-post"},
		{"GET", "/users/{id}/posts/{b}", "user-post-b"},
		{"GET", "/static/*", "static"},
		{"GET", "/a/{a}/b/{b}", "ab"},
		{"DELETE", "/users/{id}", "user-delete"},
	} {
		r.Handle(rt.method, rt.pattern, seen(rt.name))
	}
	if err := r.Build(); err != nil {
		f.Fatalf("building the table: %v", err)
	}

	for _, s := range []string{
		"GET /", "GET /users", "GET /users/7", "POST /users/7",
		"GET /users/7/posts/9", "GET /static/a/b/c", "GET /a/1/b/2",
		"DELETE /users/7", "GET /users/", "GET //users//7",
		"PUT /users/7", "GET /nope", "GET /users/7/posts",
		"GET /static/", "GET /%2e%2e/etc", "GET /users/{id}",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, line string) {
		method, target, ok := strings.Cut(line, " ")
		if !ok || method == "" || !strings.HasPrefix(target, "/") {
			t.Skip()
		}
		// A target the URL parser rejects never reaches a router in a server,
		// so it is not this target's subject.
		if _, err := url.ParseRequestURI(target); err != nil {
			t.Skip()
		}

		first := routeOnce(t, r, method, target)
		again := routeOnce(t, r, method, target)
		if first != again {
			t.Fatalf("%s %s matched differently on two identical requests:\n"+
				"  first: %s\n  again: %s", method, target, first, again)
		}
	})
}

// routeOnce serves one request and returns the route and parameters it bound,
// as a comparable string.
func routeOnce(t *testing.T, r *Router, method, target string) string {
	t.Helper()
	req, err := http.NewRequest(method, "http://x"+target, nil)
	if err != nil {
		t.Skip() // a method or target net/http itself refuses to build
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req) // must not panic: the first property
	params := append([]string(nil), rec.Header().Values("X-Param")...)
	sort.Strings(params)
	return fmt.Sprintf("%d %s [%s]", rec.Code, rec.Header().Get("X-Route"),
		strings.Join(params, " "))
}

// The fuzz table above actually matches things.
//
// A router fuzzer whose every input 404s is a panic check on the miss path and
// nothing else, and nothing in FuzzRouterMatch itself would say so: "no panic,
// same answer twice" is satisfied perfectly by a table that matches nothing.
// This pins the seeds to a spread of real routes, so a change to the patterns
// that stops them matching fails here rather than quietly emptying the fuzz.
func TestTheRouterFuzzSeedsReachRealRoutes(t *testing.T) {
	r := New()
	for _, rt := range []struct{ method, pattern, name string }{
		{"GET", "/", "root"},
		{"GET", "/users", "users"},
		{"GET", "/users/{id}", "user"},
		{"POST", "/users/{id}", "user-post"},
		{"GET", "/users/{id}/posts/{b}", "user-post-b"},
		{"GET", "/static/*", "static"},
		{"GET", "/a/{a}/b/{b}", "ab"},
		{"DELETE", "/users/{id}", "user-delete"},
	} {
		name := rt.name
		r.HandleFunc(rt.method, rt.pattern, func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Route", name)
			for _, p := range []string{"id", "*", "a", "b"} {
				if v := req.PathValue(p); v != "" {
					w.Header().Add("X-Param", p+"="+v)
				}
			}
		})
	}
	if err := r.Build(); err != nil {
		t.Fatalf("building the table: %v", err)
	}

	hit := map[string]bool{}
	params := 0
	for _, line := range []string{
		"GET /", "GET /users", "GET /users/7", "POST /users/7",
		"GET /users/7/posts/9", "GET /static/a/b/c", "GET /a/1/b/2",
		"DELETE /users/7",
	} {
		method, target, _ := strings.Cut(line, " ")
		req := httptest.NewRequest(method, target, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s answered %d; the fuzz seeds must reach real routes",
				line, rec.Code)
			continue
		}
		hit[rec.Header().Get("X-Route")] = true
		params += len(rec.Header().Values("X-Param"))
	}
	if len(hit) != 8 {
		t.Errorf("the seeds reached %d distinct routes of 8: %v", len(hit), hit)
	}
	if params == 0 {
		t.Error("no seed bound a single path parameter, so the determinism " +
			"property in FuzzRouterMatch is being checked over nothing")
	}
}
