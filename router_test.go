package simdhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func noop(http.ResponseWriter, *http.Request) {}

// echo writes the named path values so a test can assert what the walk bound.
func echo(names ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = n + "=" + req.PathValue(n)
		}
		w.Write([]byte(strings.Join(parts, " ")))
	}
}

func serve(t *testing.T, r *Router, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestRouteMatchParams(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("id=" + req.PathValue("id")))
	})
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	if got := serve(t, r, "GET", "/users/42").Body.String(); got != "id=42" {
		t.Fatalf("body %q", got)
	}
}

func TestBuildConflict(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a/{x}", noop)
	r.HandleFunc("GET", "/a/{y}", noop)
	if err := r.Build(); err == nil {
		t.Fatal("conflict not detected")
	}
}

// Two patterns that differ only in specificity are not a conflict: the more
// specific one wins at the position where they differ.
func TestPrecedence(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a/b", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("static")) })
	r.HandleFunc("GET", "/a/{x}", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("param")) })
	r.HandleFunc("GET", "/a/*", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("wild")) })
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{"/a/b", "static"},
		{"/a/c", "param"},
		{"/a/c/d", "wild"},
		{"/a/b/c", "wild"},
	} {
		if got := serve(t, r, "GET", c.path).Body.String(); got != c.want {
			t.Errorf("%s matched %q, want %q", c.path, got, c.want)
		}
	}
}

func TestWildcardRemainder(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/files/*", echo("*"))
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{"/files/a", "*=a"},
		{"/files/a/b/c", "*=a/b/c"},
		{"/files/", "*="},
	} {
		if got := serve(t, r, "GET", c.path).Body.String(); got != c.want {
			t.Errorf("%s bound %q, want %q", c.path, got, c.want)
		}
	}
}

func TestMultipleParams(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/{a}/x/{b}/{c}", echo("a", "b", "c"))
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	if got := serve(t, r, "GET", "/1/x/2/3").Body.String(); got != "a=1 b=2 c=3" {
		t.Fatalf("bound %q", got)
	}
}

// Each segment is decoded once. %2F decodes to a literal slash inside the
// segment rather than a separator, which is what ServeMux documents; the
// differential pins it, and this asserts the router's own reading.
func TestSegmentDecoding(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/p/{v}", echo("v"))
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ target, want string }{
		{"/p/a%20b", "v=a b"},
		{"/p/a%2Fb", "v=a/b"},
		{"/p/a%252Fb", "v=a%2Fb"},
		{"/p/plain", "v=plain"},
	} {
		if got := serve(t, r, "GET", c.target).Body.String(); got != c.want {
			t.Errorf("%s bound %q, want %q", c.target, got, c.want)
		}
	}
}

func TestStaticLiteralDecoded(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a b", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hit")) })
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	if got := serve(t, r, "GET", "/a%20b").Body.String(); got != "hit" {
		t.Fatalf("escaped literal did not match: %q", got)
	}
}

// Registration never panics; every bad table surfaces at Build.
func TestRegistrationErrorsSurfaceAtBuild(t *testing.T) {
	for _, c := range []struct {
		name    string
		method  string
		pattern string
	}{
		{"empty method", "", "/a"},
		{"non-token method", "GE T", "/a"},
		{"empty pattern", "GET", ""},
		{"no path at all", "GET", "ab"},
		{"no leading slash after host", "GET", "example.com"},
		{"empty param name", "GET", "/a/{}"},
		{"duplicate param name", "GET", "/{x}/{x}"},
		{"param with literal suffix", "GET", "/{id}.json"},
		{"param with literal prefix", "GET", "/v{id}"},
		{"unterminated param", "GET", "/{id"},
		{"wildcard not final", "GET", "/a/*/b"},
		{"wildcard with literal", "GET", "/a/*x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := New()
			r.HandleFunc(c.method, c.pattern, noop) // must not panic
			if err := r.Build(); err == nil {
				t.Fatalf("Build accepted %q %q", c.method, c.pattern)
			}
		})
	}
}

func TestMustBuildPanicsOnBadTable(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustBuild did not panic on a conflicting table")
		}
	}()
	r := New()
	r.HandleFunc("GET", "/a/{x}", noop)
	r.HandleFunc("GET", "/a/{y}", noop)
	r.MustBuild()
}

func TestMustBuildReturnsRouter(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/ok", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	built := r.MustBuild()
	if got := serve(t, built, "GET", "/ok").Body.String(); got != "ok" {
		t.Fatalf("body %q", got)
	}
}

// Serving an uncompiled table is a server fault, not a 404: reporting "no such
// route" would hide the missing Build behind a plausible answer.
func TestServeBeforeBuild(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a", noop)
	if code := serve(t, r, "GET", "/a").Code; code != http.StatusInternalServerError {
		t.Fatalf("code %d, want 500", code)
	}
}

func TestRegistrationAfterBuildIsRejected(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a", noop)
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	r.HandleFunc("GET", "/b", noop) // must not panic and must not take effect
	if code := serve(t, r, "GET", "/b").Code; code != http.StatusNotFound {
		t.Fatalf("a post-Build registration took effect: %d", code)
	}
	if err := r.Build(); err == nil {
		t.Fatal("second Build accepted")
	}
}

func TestUnmatchedIs404(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a/{x}", noop)
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/b", "/a", "/a/x/y", "/"} {
		if code := serve(t, r, "GET", p).Code; code != http.StatusNotFound {
			t.Errorf("%s: code %d, want 404", p, code)
		}
	}
}

func TestMiddlewareWrapsAtBuild(t *testing.T) {
	r := New()
	var order []string
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			order = append(order, "outer")
			next.ServeHTTP(w, req)
		})
	})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			order = append(order, "inner")
			next.ServeHTTP(w, req)
		})
	})
	r.HandleFunc("GET", "/a", func(http.ResponseWriter, *http.Request) { order = append(order, "handler") })
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	serve(t, r, "GET", "/a")
	if strings.Join(order, ",") != "outer,inner,handler" {
		t.Fatalf("order %v", order)
	}
}

// The matched path allocates nothing. A per-request allocation here is paid by
// every request the process ever serves.
func TestMatchAllocatesNothing(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/api/v1/users/{id}/posts/{post}", noop)
	r.HandleFunc("GET", "/static/deep/path/here", noop)
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/users/42/posts/7", nil)
	w := nilWriter{}
	if n := testing.AllocsPerRun(200, func() { r.ServeHTTP(w, req) }); n != 0 {
		t.Fatalf("%v allocations on the matched path", n)
	}
	sreq := httptest.NewRequest("GET", "/static/deep/path/here", nil)
	if n := testing.AllocsPerRun(200, func() { r.ServeHTTP(w, sreq) }); n != 0 {
		t.Fatalf("%v allocations on a static match", n)
	}
}

// nilWriter is a ResponseWriter that allocates nothing, so the allocation test
// measures the router rather than the recorder.
type nilWriter struct{}

func (nilWriter) Header() http.Header         { return nil }
func (nilWriter) Write(b []byte) (int, error) { return len(b), nil }
func (nilWriter) WriteHeader(int)             {}

// A built router is read-only, so any number of goroutines may serve from it.
// Run with -race.
func TestConcurrentServe(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/u/{id}", echo("id"))
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, httptest.NewRequest("GET", "/u/7", nil))
				if rec.Body.String() != "id=7" {
					t.Errorf("body %q", rec.Body.String())
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Custom token methods get IDs beyond the common nine and must match exactly.
func TestCustomMethods(t *testing.T) {
	r := New()
	r.HandleFunc("MKCOL", "/c", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("mkcol")) })
	r.HandleFunc("REPORT", "/c", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("report")) })
	r.HandleFunc("GET", "/c", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("get")) })
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, want string }{
		{"MKCOL", "mkcol"},
		{"REPORT", "report"},
		{"GET", "get"},
	} {
		if got := serve(t, r, c.method, "/c").Body.String(); got != c.want {
			t.Errorf("%s -> %q, want %q", c.method, got, c.want)
		}
	}
}

func TestMethodMatchIsCaseSensitive(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/c", noop)
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	// httptest.NewRequest keeps the method verbatim.
	if code := serve(t, r, "get", "/c").Code; code == http.StatusOK {
		t.Fatal("lowercase get matched the GET pattern")
	}
}

// The same method twice on the same pattern is a conflict, not a silent
// last-registration-wins.
func TestDuplicateRegistrationIsConflict(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a", noop)
	r.HandleFunc("GET", "/a", noop)
	if err := r.Build(); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}

func TestDifferentMethodsSamePatternCoexist(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("get")) })
	r.HandleFunc("POST", "/a", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("post")) })
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	if got := serve(t, r, "GET", "/a").Body.String(); got != "get" {
		t.Fatalf("GET -> %q", got)
	}
	if got := serve(t, r, "POST", "/a").Body.String(); got != "post" {
		t.Fatalf("POST -> %q", got)
	}
}

// Patterns are copied at registration: the caller may reuse its buffers.
func TestPatternsCopiedAtRegistration(t *testing.T) {
	b := []byte("/users/{id}")
	r := New()
	r.HandleFunc("GET", string(b), echo("id"))
	copy(b, []byte("/XXXXX/{id}"))
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	if got := serve(t, r, "GET", "/users/9").Body.String(); got != "id=9" {
		t.Fatalf("body %q", got)
	}
}
