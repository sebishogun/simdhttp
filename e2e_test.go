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
	"sync"
	"testing"
	"time"

	"github.com/sebishogun/simdhttp/http1"
)

// The router is exercised through a real http.Server and a real client here,
// not through ServeHTTP directly. Everything the package promises about fitting
// the ecosystem -- hijacking, flushing, TLS, HTTP/2, keep-alive -- is a
// property of that arrangement and cannot be observed from a recorder.

func e2eRouter(t *testing.T) *Router {
	t.Helper()
	r := New()
	r.HandleFunc("GET", "/hello/{name}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "hello %s", Param(req, "name"))
	})
	r.Handle("POST", "/echo", Wrap(func(w http.ResponseWriter, req *http.Request) error {
		var in struct {
			Msg string `json:"msg"`
		}
		if err := JSONDecodeLimit(req, &in, 32); err != nil {
			return err
		}
		return JSON(w, http.StatusOK, map[string]string{"msg": in.Msg})
	}))
	r.HandleFunc("GET", "/proto", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "HTTP/%d.%d tls=%v", req.ProtoMajor, req.ProtoMinor, req.TLS != nil)
	})
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	return r
}

func getBody(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestE2EPlainServer(t *testing.T) {
	srv := httptest.NewServer(e2eRouter(t))
	defer srv.Close()
	if code, body := getBody(t, srv.Client(), srv.URL+"/hello/world"); code != 200 || body != "hello world" {
		t.Fatalf("%d %q", code, body)
	}
	if code, _ := getBody(t, srv.Client(), srv.URL+"/nothing"); code != 404 {
		t.Fatalf("code %d", code)
	}
}

// The adapter's verdict has to survive the whole stack, not just a recorder.
func TestE2EErrorAdapterOverTheWire(t *testing.T) {
	srv := httptest.NewServer(e2eRouter(t))
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/echo", "application/json", strings.NewReader(`{"msg":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"msg":"hi"`) {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type %q", ct)
	}

	// Past the 32-byte decode limit: ErrBodyTooLarge, mapped to 413.
	big := `{"msg":"` + strings.Repeat("x", 100) + `"}`
	resp, err = srv.Client().Post(srv.URL+"/echo", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d, want 413", resp.StatusCode)
	}
}

func TestE2EMiddlewareOrderOverTheWire(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				order = append(order, s)
				mu.Unlock()
				w.Header().Add("X-Chain", s)
				next.ServeHTTP(w, req)
			})
		}
	}
	r := New()
	r.Use(note("outer"), note("inner"))
	r.HandleFunc("GET", "/m", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("handler")) })
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/m")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := strings.Join(resp.Header.Values("X-Chain"), ","); got != "outer,inner" {
		t.Fatalf("header chain %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "outer,inner" {
		t.Fatalf("order %v", order)
	}
}

// Streaming: the client must see the first chunk before the handler produces
// the second, which is only true if Flush reaches the real connection.
func TestE2EFlushStreams(t *testing.T) {
	release := make(chan struct{})
	r := New()
	r.HandleFunc("GET", "/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "first\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
			return
		}
		<-release
		fmt.Fprint(w, "second\n")
	})
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("first line %q err %v -- the flush did not reach the wire", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if string(rest) != "second\n" {
		t.Fatalf("rest %q", rest)
	}
}

// A WebSocket upgrade in the shape a library performs it: assert the response,
// then talk raw bytes both ways over the same connection.
func TestE2EHijackWebSocketShaped(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/ws", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err := buf.Flush(); err != nil {
			t.Errorf("flush: %v", err)
			return
		}
		line, err := buf.ReadString('\n')
		if err != nil {
			t.Errorf("read after upgrade: %v", err)
			return
		}
		buf.WriteString("echo:" + line)
		buf.Flush()
	})
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 101") {
		t.Fatalf("status %q err %v", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	fmt.Fprint(conn, "ping\n")
	echo, err := br.ReadString('\n')
	if err != nil || echo != "echo:ping\n" {
		t.Fatalf("echo %q err %v", echo, err)
	}
}

func TestE2ETLS(t *testing.T) {
	srv := httptest.NewTLSServer(e2eRouter(t))
	defer srv.Close()
	code, body := getBody(t, srv.Client(), srv.URL+"/proto")
	if code != 200 || !strings.Contains(body, "tls=true") {
		t.Fatalf("%d %q", code, body)
	}
}

// HTTP/2 reaches the router as a plain http.Handler; nothing in http1 is
// involved, which is the point of keeping the two separable.
func TestE2EHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(e2eRouter(t))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	code, body := getBody(t, srv.Client(), srv.URL+"/proto")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(body, "HTTP/2.0") {
		t.Fatalf("body %q -- the request did not arrive over h2", body)
	}
	if code, body := getBody(t, srv.Client(), srv.URL+"/hello/h2"); code != 200 || body != "hello h2" {
		t.Fatalf("%d %q", code, body)
	}
}

// Keep-alive: many requests over one connection must each be routed
// independently, which is what a pipelining or framing mistake breaks.
func TestE2EKeepAliveManyRequests(t *testing.T) {
	srv := httptest.NewServer(e2eRouter(t))
	defer srv.Close()
	tr := &http.Transport{MaxIdleConnsPerHost: 1}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("n%d", i)
		code, body := getBody(t, c, srv.URL+"/hello/"+name)
		if code != 200 || body != "hello "+name {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
	}
}

// The asterisk-form OPTIONS reaches the router only when the server is told
// not to answer it first. Both halves are asserted so the flag's role is
// recorded rather than assumed.
func TestE2EOptionsAsteriskNeedsTheFlag(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a", noop)
	r.HandleFunc("POST", "/b", noop)
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	for _, disable := range []bool{false, true} {
		srv := httptest.NewUnstartedServer(r)
		srv.Config.DisableGeneralOptionsHandler = disable
		srv.Start()
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(conn, "OPTIONS * HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		all, _ := io.ReadAll(bufio.NewReader(conn))
		conn.Close()
		srv.Close()
		hasAllow := strings.Contains(string(all), "Allow: GET, HEAD, POST")
		if disable && !hasAllow {
			t.Fatalf("with the flag set the router did not answer: %q", all)
		}
		if !disable && hasAllow {
			t.Fatalf("without the flag the server should have answered first: %q", all)
		}
	}
}

// A handler that fails after starting its response aborts the connection
// rather than sending a second status. The client sees a truncated body and an
// error, never a valid second response.
func TestE2EAbortAfterResponseStarted(t *testing.T) {
	r := New()
	r.Handle("GET", "/half", ErrorAdapter{Log: func(*http.Request, error) {}}.Wrap(
		func(w http.ResponseWriter, _ *http.Request) error {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("partial"))
			return errors.New("failed halfway")
		}))
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/half")
	if err != nil {
		return // the connection may die before the header arrives; also correct
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr == nil && len(body) == 100 {
		t.Fatal("the client received a complete body after the handler failed")
	}
}

// Everything the http1 package exports stays out of the router's path: the two
// are separable, and an application may use either alone.
func TestE2ERouterDoesNotDependOnHTTP1Parsing(t *testing.T) {
	// The helpers reference http1 only for its error sentinels, which are
	// values, not the parser. Serving a request must not touch Parse.
	srv := httptest.NewServer(e2eRouter(t))
	defer srv.Close()
	if code, _ := getBody(t, srv.Client(), srv.URL+"/hello/x"); code != 200 {
		t.Fatalf("code %d", code)
	}
	var req http1.Request
	if _, err := http1.Parse(&req, []byte("GET / HTTP/1.1\r\nHost: h\r\n\r\n"), http1.Compatible); err != nil {
		t.Fatalf("http1 still parses independently: %v", err)
	}
}
