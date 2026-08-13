package simdhttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The match path is what every request pays, so it is measured with
// allocations reported: one allocation here is one per request for the life of
// the process.

func buildLarge(b *testing.B, n int) *Router {
	b.Helper()
	r := New()
	for i := 0; i < n; i++ {
		r.HandleFunc("GET", fmt.Sprintf("/c%d/{a}/{b}/tail", i), noop)
	}
	if err := r.Build(); err != nil {
		b.Fatal(err)
	}
	return r
}

func BenchmarkMatch4Segment(b *testing.B) {
	r := buildLarge(b, 100_000)
	req := httptest.NewRequest("GET", "/c99999/x/y/tail", nil)
	w := nilWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(w, req)
	}
}

func BenchmarkMatchStatic(b *testing.B) {
	r := New()
	for i := 0; i < 1000; i++ {
		r.HandleFunc("GET", fmt.Sprintf("/api/v1/resource%d/list", i), noop)
	}
	if err := r.Build(); err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/resource999/list", nil)
	w := nilWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(w, req)
	}
}

// The same shape through net/http.ServeMux, so the comparison is against the
// thing this replaces rather than against nothing.
func BenchmarkMatch4SegmentServeMux(b *testing.B) {
	mux := http.NewServeMux()
	for i := 0; i < 100_000; i++ {
		mux.HandleFunc(fmt.Sprintf("GET /c%d/{a}/{b}/tail", i), noop)
	}
	req := httptest.NewRequest("GET", "/c99999/x/y/tail", nil)
	w := nilWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(w, req)
	}
}

func BenchmarkMatchStaticServeMux(b *testing.B) {
	mux := http.NewServeMux()
	for i := 0; i < 1000; i++ {
		mux.HandleFunc(fmt.Sprintf("GET /api/v1/resource%d/list", i), noop)
	}
	req := httptest.NewRequest("GET", "/api/v1/resource999/list", nil)
	w := nilWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(w, req)
	}
}

// Build is not on the request path, but a hundred-thousand-route table must
// still compile in a startup-shaped amount of time -- conflict detection is
// pairwise in principle and this is what proves the bucketing works.
func BenchmarkBuild100k(b *testing.B) {
	for i := 0; i < b.N; i++ {
		r := New()
		for j := 0; j < 100_000; j++ {
			r.HandleFunc("GET", fmt.Sprintf("/c%d/{a}/{b}/tail", j), noop)
		}
		if err := r.Build(); err != nil {
			b.Fatal(err)
		}
	}
}
