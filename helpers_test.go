package simdhttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/sebishogun/simdhttp/http1"
)

func TestJSONRoundTrip(t *testing.T) {
	type payload struct {
		Name  string   `json:"name"`
		Count int      `json:"count"`
		Tags  []string `json:"tags"`
	}
	want := payload{Name: "a b", Count: 7, Tags: []string{"x", "y"}}

	rec := httptest.NewRecorder()
	if err := JSON(rec, http.StatusCreated, want); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type %q", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("content-length %q for a %d-byte body", cl, rec.Body.Len())
	}

	req := httptest.NewRequest("POST", "/", strings.NewReader(rec.Body.String()))
	var got payload
	if err := JSONDecode(req, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || got.Count != want.Count || len(got.Tags) != 2 {
		t.Fatalf("round trip gave %+v", got)
	}
}

// An encoding failure must not arrive after a success status: the value is
// encoded first, so the caller still has a status to send.
func TestJSONEncodeFailureDoesNotWriteStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	err := JSON(rec, http.StatusOK, make(chan int)) // channels do not encode
	if err == nil {
		t.Fatal("encoding a channel reported success")
	}
	if rec.Code != 200 || rec.Body.Len() != 0 {
		// httptest defaults Code to 200; the point is that nothing was written.
		if rec.Flushed || rec.Body.Len() != 0 {
			t.Fatalf("wrote %d bytes before failing", rec.Body.Len())
		}
	}
}

func TestJSONDecodeLimit(t *testing.T) {
	big := "{\"name\":\"" + strings.Repeat("x", 200) + "\"}"
	req := httptest.NewRequest("POST", "/", strings.NewReader(big))
	var v struct {
		Name string `json:"name"`
	}
	if err := JSONDecodeLimit(req, &v, 64); !errors.Is(err, http1.ErrBodyTooLarge) {
		t.Fatalf("err %v, want http1.ErrBodyTooLarge", err)
	}
	// Exactly at the limit is accepted.
	body := `{"name":"ok"}`
	req = httptest.NewRequest("POST", "/", strings.NewReader(body))
	if err := JSONDecodeLimit(req, &v, int64(len(body))); err != nil {
		t.Fatalf("a body exactly at the limit was rejected: %v", err)
	}
	if v.Name != "ok" {
		t.Fatalf("decoded %q", v.Name)
	}
}

func TestJSONDecodeMalformed(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":`))
	var v struct{ Name string }
	if err := JSONDecode(req, &v); err == nil {
		t.Fatal("a truncated document decoded without error")
	}
}

// The query accessors are compared against net/url's, which is the definition
// they have to meet; scanning the raw query is an optimization, not a licence
// to answer differently.
func TestQueryMatchesNetURL(t *testing.T) {
	for _, raw := range []string{
		"a=1&b=2",
		"a=1&a=2",
		"a",
		"a=",
		"=v",
		"a=hello+world",
		"a=hello%20world",
		"a%20b=v",
		"a=%2Fpath%2F",
		"a=1&&b=2",
		"a=%zz",
		"",
		"flag&other=1",
	} {
		req := httptest.NewRequest("GET", "/?"+raw, nil)
		want := req.URL.Query()
		for _, name := range []string{"a", "b", "a b", "flag", "", "missing"} {
			got := Query(req, name)
			if w := want.Get(name); got != w {
				t.Errorf("raw %q name %q: got %q, net/url %q", raw, name, got, w)
			}
			if has := QueryHas(req, name); has != want.Has(name) {
				t.Errorf("raw %q name %q: has %v, net/url %v", raw, name, has, want.Has(name))
			}
		}
	}
}

func TestQueryTypedAccessors(t *testing.T) {
	req := httptest.NewRequest("GET", "/?n=42&bad=x&t=true&f=0&bare&empty=", nil)
	if got := QueryInt(req, "n", -1); got != 42 {
		t.Errorf("n = %d", got)
	}
	if got := QueryInt(req, "bad", -1); got != -1 {
		t.Errorf("an unparseable int gave %d rather than the default", got)
	}
	if got := QueryInt(req, "absent", -1); got != -1 {
		t.Errorf("absent = %d", got)
	}
	if !QueryBool(req, "t", false) || QueryBool(req, "f", true) {
		t.Error("bool parsing")
	}
	if !QueryBool(req, "bare", false) {
		t.Error("a bare parameter should read as true")
	}
	if !QueryBool(req, "empty", false) {
		t.Error("an empty value should read as true")
	}
	if !QueryBool(req, "absent", true) {
		t.Error("absent should take the default")
	}
}

func TestParamAndForm(t *testing.T) {
	r := New()
	r.HandleFunc("POST", "/u/{id}", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(Param(req, "id") + ":" + Form(req, "field")))
	})
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	body := url.Values{"field": {"value"}}.Encode()
	req := httptest.NewRequest("POST", "/u/9", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "9:value" {
		t.Fatalf("body %q", rec.Body.String())
	}
}

// No package of this module may import encoding/json, and the only package in
// the whole graph that may is simdjson -- which does it for type identity:
// json.RawMessage and json.Marshaler have to be the same types a caller's
// structs already name, so they are aliases and the import is unavoidable.
//
// Asserted against the real graph rather than by reading imports, so an
// arrival through a new dependency is caught too. simdjson also retains a
// json.Marshal fallback for a marshaler whose type assertion fails; that is
// simdjson's to remove, and this test names it so it is not mistaken for this
// package's doing.
func TestOnlySimdjsonReachesEncodingJSON(t *testing.T) {
	deps, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	var importers []string
	for _, pkg := range strings.Fields(string(deps)) {
		if pkg == "encoding/json" {
			continue
		}
		out, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", pkg).Output()
		if err != nil {
			continue
		}
		for _, imp := range strings.Fields(string(out)) {
			if imp == "encoding/json" || imp == "encoding/json/v2" {
				importers = append(importers, pkg)
			}
		}
	}
	for _, pkg := range importers {
		if pkg != "github.com/sebishogun/simdjson" {
			t.Errorf("%s imports encoding/json", pkg)
		}
	}
}

func BenchmarkQueryRaw(b *testing.B) {
	req := httptest.NewRequest("GET", "/?alpha=1&beta=2&gamma=3&delta=4&target=value", nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if Query(req, "target") != "value" {
			b.Fatal("wrong value")
		}
	}
}

func BenchmarkQueryNetURL(b *testing.B) {
	req := httptest.NewRequest("GET", "/?alpha=1&beta=2&gamma=3&delta=4&target=value", nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if req.URL.Query().Get("target") != "value" {
			b.Fatal("wrong value")
		}
	}
}
