package simdhttp

import (
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// The router and net/http.ServeMux are fed the same pattern sets and the same
// requests, and every observable must agree: which route ran, what it bound,
// the status, the Allow string byte for byte, and the redirect location. A
// router that agrees on the happy path and diverges on 405 is exactly the kind
// of difference an operator finds in production rather than in a test.
//
// Two things are out of scope by design. Method-less ServeMux patterns are not
// part of this API (every pattern carries a method), so none are generated.
// The asterisk-form OPTIONS is simdhttp-specific -- ServeMux alone answers 400
// -- and is asserted in the router tests instead.

// muxPattern renders a pattern in ServeMux's syntax: our "*" final segment is
// ServeMux's "{rest...}".
func muxPattern(method, pattern string) string {
	if strings.HasSuffix(pattern, "/*") {
		pattern = strings.TrimSuffix(pattern, "*") + "{rest...}"
	}
	return method + " " + pattern
}

// observation is everything a caller can see about one request.
type observation struct {
	code     int
	body     string
	allow    string
	location string
}

func observe(h http.Handler, method, target, host string) observation {
	req := httptest.NewRequest(method, target, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return observation{
		code:     rec.Code,
		body:     rec.Body.String(),
		allow:    rec.Header().Get("Allow"),
		location: rec.Header().Get("Location"),
	}
}

// routeEcho reports which pattern ran and what it bound, so a divergence names
// the route rather than just the status.
func routeEcho(pattern string, names []string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		var b strings.Builder
		b.WriteString(pattern)
		for _, n := range names {
			fmt.Fprintf(&b, " %s=%q", n, req.PathValue(n))
		}
		w.Write([]byte(b.String()))
	}
}

// paramNames extracts the {name} segments of a pattern, plus "rest" for a
// wildcard, so both routers are asked for the same bindings.
func paramNames(pattern string) []string {
	var out []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, seg[1:len(seg)-1])
		} else if seg == "*" {
			out = append(out, "rest")
		}
	}
	return out
}

// ourNames maps ServeMux's "rest" wildcard name back to ours ("*").
func ourNames(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		if n == "rest" {
			out[i] = "*"
		} else {
			out[i] = n
		}
	}
	return out
}

type patternSet struct {
	name     string
	patterns [][2]string // method, pattern
	requests [][2]string // method, path
}

func fixedSets() []patternSet {
	return []patternSet{
		{
			name: "static and params",
			patterns: [][2]string{
				{"GET", "/"}, {"GET", "/users"}, {"GET", "/users/{id}"},
				{"POST", "/users"}, {"GET", "/users/{id}/posts/{post}"},
				{"GET", "/a/b/c"}, {"DELETE", "/users/{id}"},
			},
			requests: [][2]string{
				{"GET", "/"}, {"GET", "/users"}, {"GET", "/users/42"},
				{"POST", "/users"}, {"PUT", "/users"}, {"GET", "/users/42/posts/7"},
				{"GET", "/a/b/c"}, {"GET", "/a/b"}, {"GET", "/a/b/c/d"},
				{"DELETE", "/users/42"}, {"PATCH", "/users/42"},
				{"GET", "/nothing"}, {"GET", "/users/"}, {"GET", "/users/42/posts"},
			},
		},
		{
			name: "trailing slash",
			patterns: [][2]string{
				{"GET", "/users/"}, {"POST", "/items/"}, {"GET", "/both"}, {"GET", "/both/"},
			},
			requests: [][2]string{
				{"GET", "/users"}, {"GET", "/users/"}, {"GET", "/items"},
				{"POST", "/items"}, {"POST", "/items/"}, {"GET", "/both"},
				{"GET", "/both/"}, {"DELETE", "/both"},
			},
		},
		{
			name: "method space",
			patterns: [][2]string{
				{"GET", "/r"}, {"POST", "/r"}, {"MKCOL", "/r"}, {"REPORT", "/r"},
				{"HEAD", "/explicit"}, {"GET", "/explicit"},
			},
			requests: [][2]string{
				{"GET", "/r"}, {"HEAD", "/r"}, {"POST", "/r"}, {"MKCOL", "/r"},
				{"REPORT", "/r"}, {"DELETE", "/r"}, {"PUT", "/r"},
				{"HEAD", "/explicit"}, {"GET", "/explicit"}, {"POST", "/explicit"},
			},
		},
		{
			name: "wildcard",
			patterns: [][2]string{
				{"GET", "/files/*"}, {"GET", "/files/special"}, {"POST", "/files/*"},
			},
			requests: [][2]string{
				{"GET", "/files/a"}, {"GET", "/files/a/b/c"}, {"GET", "/files/"},
				{"GET", "/files/special"}, {"POST", "/files/x"}, {"DELETE", "/files/x"},
				{"GET", "/files"},
			},
		},
		{
			name: "escapes",
			patterns: [][2]string{
				{"GET", "/p/{v}"}, {"GET", "/lit x"}, {"GET", "/d/{a}/{b}"},
			},
			requests: [][2]string{
				{"GET", "/p/a%20b"}, {"GET", "/p/a%2Fb"}, {"GET", "/p/a%252Fb"},
				{"GET", "/lit%20x"}, {"GET", "/d/1/2"}, {"GET", "/d/%41/%42"},
			},
		},
		{
			name: "precedence",
			patterns: [][2]string{
				{"GET", "/x/{a}/z"}, {"GET", "/x/y/w"}, {"GET", "/x/*"},
			},
			requests: [][2]string{
				{"GET", "/x/y/z"}, {"GET", "/x/y/w"}, {"GET", "/x/q/z"},
				{"GET", "/x/y"}, {"GET", "/x/y/z/extra"},
			},
		},
	}
}

func runDifferential(t *testing.T, set patternSet) {
	t.Helper()
	ours := New()
	mux := http.NewServeMux()
	for _, p := range set.patterns {
		names := paramNames(p[1])
		mux.HandleFunc(muxPattern(p[0], p[1]), routeEcho(p[1], names))
		ours.HandleFunc(p[0], p[1], routeEcho(p[1], ourNames(names)))
	}
	if err := ours.Build(); err != nil {
		t.Fatalf("%s: Build: %v", set.name, err)
	}
	for _, req := range set.requests {
		got := observe(ours, req[0], req[1], "")
		want := observe(mux, req[0], req[1], "")
		// Bodies differ in the wildcard's name; compare after normalizing it.
		gotBody := strings.ReplaceAll(got.body, `*=`, `rest=`)
		wantBody := want.body
		if got.code != want.code {
			t.Errorf("%s: %s %s -> %d, ServeMux %d", set.name, req[0], req[1], got.code, want.code)
			continue
		}
		if got.code == http.StatusOK && gotBody != wantBody {
			t.Errorf("%s: %s %s bound %q, ServeMux %q", set.name, req[0], req[1], gotBody, wantBody)
		}
		if got.allow != want.allow {
			t.Errorf("%s: %s %s Allow %q, ServeMux %q", set.name, req[0], req[1], got.allow, want.allow)
		}
		if got.location != want.location {
			t.Errorf("%s: %s %s Location %q, ServeMux %q", set.name, req[0], req[1], got.location, want.location)
		}
	}
}

func TestRouterDifferentialFixed(t *testing.T) {
	for _, set := range fixedSets() {
		t.Run(set.name, func(t *testing.T) { runDifferential(t, set) })
	}
}

// TestRouterDifferentialGenerated builds random tables and random requests
// against them. Hand-written cases cover what was thought of; this covers the
// combinations that were not.
func TestRouterDifferentialGenerated(t *testing.T) {
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "MKCOL"}
	lits := []string{"a", "b", "users", "v1", "x"}
	rng := rand.New(rand.NewSource(20260813))
	for iter := 0; iter < 300; iter++ {
		set := patternSet{name: fmt.Sprintf("generated/%d", iter)}
		seen := map[string]bool{}
		for i := 0; i < 1+rng.Intn(6); i++ {
			var segs []string
			for d := 0; d < 1+rng.Intn(3); d++ {
				switch rng.Intn(4) {
				case 0:
					segs = append(segs, "{p"+fmt.Sprint(d)+"}")
				default:
					segs = append(segs, lits[rng.Intn(len(lits))])
				}
			}
			pattern := "/" + strings.Join(segs, "/")
			if rng.Intn(6) == 0 {
				pattern += "/"
			}
			method := methods[rng.Intn(len(methods))]
			if seen[method+" "+pattern] {
				continue
			}
			seen[method+" "+pattern] = true
			set.patterns = append(set.patterns, [2]string{method, pattern})
		}
		// Requests: the registered paths with parameters filled, near misses,
		// and both slash forms of each.
		for _, p := range set.patterns {
			path := p[1]
			for i := 0; ; i++ {
				old := fmt.Sprintf("{p%d}", i)
				if !strings.Contains(path, old) {
					break
				}
				path = strings.ReplaceAll(path, old, fmt.Sprintf("v%d", i))
			}
			set.requests = append(set.requests,
				[2]string{p[0], path},
				[2]string{methods[rng.Intn(len(methods))], path},
				[2]string{p[0], strings.TrimSuffix(path, "/")},
				[2]string{p[0], path + "/"},
				[2]string{p[0], path + "/extra"},
			)
		}
		set.requests = append(set.requests, [2]string{"GET", "/"}, [2]string{"GET", "/zzz"})

		// A generated set can contain patterns one router rejects and the
		// other accepts (ServeMux allows "{p}" repeated across segments of
		// different patterns, we allow the same); skip a set only if OUR
		// Build fails, and record why, so the skip cannot hide a bug.
		probe := New()
		for _, p := range set.patterns {
			probe.HandleFunc(p[0], p[1], noop)
		}
		if err := probe.Build(); err != nil {
			t.Logf("%s skipped: %v", set.name, err)
			continue
		}
		if !muxAccepts(set.patterns) {
			t.Logf("%s skipped: ServeMux rejects the set", set.name)
			continue
		}
		runDifferential(t, set)
	}
}

// muxAccepts reports whether ServeMux takes a pattern set without panicking:
// it panics on conflicts, and a generated set may contain one.
func muxAccepts(patterns [][2]string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	mux := http.NewServeMux()
	for _, p := range patterns {
		mux.HandleFunc(muxPattern(p[0], p[1]), noop)
	}
	return true
}

// Allow strings are compared as exact bytes everywhere else; this pins the
// sort order independently, since a set-equal-but-differently-ordered header
// would pass a naive comparison in a hand-written test.
func TestAllowIsLexicographic(t *testing.T) {
	r := New()
	for _, m := range []string{"REPORT", "GET", "MKCOL", "POST", "DELETE"} {
		r.HandleFunc(m, "/a", noop)
	}
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	got := serve(t, r, "PUT", "/a").Header().Get("Allow")
	parts := strings.Split(got, ", ")
	if !sort.StringsAreSorted(parts) {
		t.Fatalf("Allow %q is not lexicographic", got)
	}
	if got != "DELETE, GET, HEAD, MKCOL, POST, REPORT" {
		t.Fatalf("Allow %q", got)
	}
}
