package simdhttp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebishogun/simdhttp/http1"
)

func TestWrapNilError(t *testing.T) {
	h := Wrap(func(w http.ResponseWriter, _ *http.Request) error {
		w.Write([]byte("ok"))
		return nil
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

// The parse and framing errors already carry a verdict; the adapter is what
// turns that verdict into a status instead of every handler restating it.
func TestWrapSentinelStatuses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{http1.ErrMissingHost, http.StatusBadRequest},
		{http1.ErrMalformed, http.StatusBadRequest},
		{http1.ErrIncomplete, http.StatusBadRequest},
		{http1.ErrAmbiguousFraming, http.StatusBadRequest},
		{http1.ErrBadContentLength, http.StatusBadRequest},
		{http1.ErrBadChunk, http.StatusBadRequest},
		{http1.ErrBadTransferEncoding, http.StatusNotImplemented},
		{http1.ErrBodyTooLarge, http.StatusRequestEntityTooLarge},
		{http1.ErrDrainTooLarge, http.StatusRequestEntityTooLarge},
		{http1.ErrChunkLineTooLong, http.StatusRequestEntityTooLarge},
		{http1.ErrChunkExtensionTooLong, http.StatusRequestEntityTooLarge},
		{http1.ErrTooManyTrailers, http.StatusRequestEntityTooLarge},
		{http1.ErrHeadTooLarge, http.StatusRequestHeaderFieldsTooLarge},
		{http1.ErrRequestLineTooLarge, http.StatusRequestURITooLong},
		{http1.ErrTooManyHeaders, http.StatusRequestHeaderFieldsTooLarge},
		{http1.ErrValueTooLarge, http.StatusRequestHeaderFieldsTooLarge},
		{errors.New("something else"), http.StatusInternalServerError},
	} {
		err := c.err
		h := Wrap(func(http.ResponseWriter, *http.Request) error { return err })
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != c.want {
			t.Errorf("%v -> %d, want %d", err, rec.Code, c.want)
		}
	}
}

// A wrapped sentinel keeps its status: handlers add context with %w and the
// verdict must survive it.
func TestWrapUnwrapsWrappedSentinels(t *testing.T) {
	h := Wrap(func(http.ResponseWriter, *http.Request) error {
		return fmt.Errorf("reading the body: %w", http1.ErrBodyTooLarge)
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d", rec.Code)
	}
}

// An unexpected error must not describe itself to the client: the text may
// name a file, a query, or an internal host.
func TestWrapDoesNotLeakUnknownErrorText(t *testing.T) {
	h := Wrap(func(http.ResponseWriter, *http.Request) error {
		return errors.New("dial tcp 10.0.0.7:5432: connection refused")
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Fatalf("body leaked the error text: %q", rec.Body.String())
	}
}

// Once the response has started there is no status left to send. Writing a
// second one would corrupt the stream, so the adapter logs and aborts the
// connection instead.
func TestWrapAfterResponseStarted(t *testing.T) {
	var logged error
	a := ErrorAdapter{Log: func(_ *http.Request, err error) { logged = err }}
	h := a.Wrap(func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		return errors.New("failed halfway")
	})
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != http.ErrAbortHandler {
				t.Fatalf("recovered %v, want http.ErrAbortHandler", r)
			}
		}()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	}()
	if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
		t.Fatalf("the started response changed: %d %q", rec.Code, rec.Body.String())
	}
	if logged == nil || !strings.Contains(logged.Error(), "failed halfway") {
		t.Fatalf("logged %v", logged)
	}
}

func TestWrapCustomMapper(t *testing.T) {
	sentinel := errors.New("teapot")
	a := ErrorAdapter{Map: func(err error) (int, string) {
		if errors.Is(err, sentinel) {
			return http.StatusTeapot, "short and stout"
		}
		return DefaultErrorMapper(err)
	}}
	h := a.Wrap(func(http.ResponseWriter, *http.Request) error { return sentinel })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusTeapot || !strings.Contains(rec.Body.String(), "short and stout") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

// The adapter wraps the ResponseWriter to know whether the response started.
// That must not cost the handler its Flusher or Hijacker: a streaming or
// WebSocket endpoint has to keep working through it.
func TestWrapPreservesFlusher(t *testing.T) {
	srv := httptest.NewServer(Wrap(func(w http.ResponseWriter, _ *http.Request) error {
		w.Write([]byte("first\n"))
		f, ok := w.(http.Flusher)
		if !ok {
			return errors.New("Flusher lost")
		}
		f.Flush()
		if err := http.NewResponseController(w).Flush(); err != nil {
			return fmt.Errorf("ResponseController flush: %w", err)
		}
		w.Write([]byte("second\n"))
		return nil
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "first\nsecond\n" {
		t.Fatalf("body %q", body)
	}
}

func TestWrapPreservesHijacker(t *testing.T) {
	srv := httptest.NewServer(Wrap(func(w http.ResponseWriter, _ *http.Request) error {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return errors.New("Hijacker lost")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return err
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: raw\r\n\r\nhijacked")
		return buf.Flush()
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	all, _ := io.ReadAll(bufio.NewReader(conn))
	if !strings.Contains(string(all), "101 Switching Protocols") || !strings.HasSuffix(string(all), "hijacked") {
		t.Fatalf("response %q", all)
	}
}

// The wrapper must not claim an interface the underlying writer lacks: a
// handler that feature-detects would take a path the server cannot serve.
func TestWrapDoesNotInventCapabilities(t *testing.T) {
	h := Wrap(func(w http.ResponseWriter, _ *http.Request) error {
		if _, ok := w.(http.Hijacker); ok {
			return errors.New("claimed Hijacker over a writer that has none")
		}
		w.Write([]byte("ok"))
		return nil
	})
	rec := httptest.NewRecorder() // implements Flusher, not Hijacker
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body.String())
	}
}

// Wrap composes with the router the same as any http.Handler.
func TestWrapThroughRouter(t *testing.T) {
	r := New()
	r.Handle("GET", "/x/{id}", Wrap(func(w http.ResponseWriter, req *http.Request) error {
		if req.PathValue("id") == "bad" {
			return http1.ErrMalformed
		}
		w.Write([]byte("id=" + req.PathValue("id")))
		return nil
	}))
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	if got := serve(t, r, "GET", "/x/7").Body.String(); got != "id=7" {
		t.Fatalf("body %q", got)
	}
	if code := serve(t, r, "GET", "/x/bad").Code; code != http.StatusBadRequest {
		t.Fatalf("code %d", code)
	}
}
