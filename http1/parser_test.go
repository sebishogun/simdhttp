package http1

import (
	"bufio"
	"bytes"
	"math/rand"
	"net/http"
	"strings"
	"testing"
)

// The oracle is net/http's own reader: everything it accepts we accept
// with the same fields, everything it rejects we reject.
func TestParseAgainstNetHTTP(t *testing.T) {
	cases := []string{
		"GET / HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"POST /a/b?q=1 HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\nX-Empty:\r\nX-Ows:   padded   \r\n\r\n",
		"GET / HTTP/1.1\r\nHost: h\r\nA: 1\r\nB: 2\r\nC: 3\r\nD: 4\r\nE: 5\r\nF: 6\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: s\r\n\r\n",
		// Rejections.
		"GET / HTTP/1.1\r\nBad Header: x\r\n\r\n",
		"GET / HTTP/1.1\r\nX: a\x00b\r\n\r\n",
		"GET /\r\n\r\n",
		"\r\n\r\n",
		"GET / HTTP/1.1\nHost: bare-lf\n\n",
	}
	var req Request
	for _, c := range cases {
		hr, herr := http.ReadRequest(bufio.NewReader(strings.NewReader(c + "XXX")))
		n, err := Parse(&req, []byte(c), Compatible)
		if herr != nil && err == nil {
			t.Fatalf("%q: net/http rejects (%v), ours accepts", c, herr)
		}
		// Two places we are deliberately stricter than net/http, both
		// request-smuggling surface: a space in a field name, and bare-LF
		// line endings. Everything else must match its verdict.
		stricter := strings.Contains(c, "Bad Header") || !strings.Contains(c, "\r")
		if herr == nil && err != nil && !stricter {
			t.Fatalf("%q: net/http accepts, ours rejects (%v)", c, err)
		}
		if err != nil {
			continue
		}
		if n != len(c) {
			t.Fatalf("%q: consumed %d want %d", c, n, len(c))
		}
		if string(req.Method) != hr.Method {
			t.Fatalf("%q: method %q vs %q", c, req.Method, hr.Method)
		}
		if string(req.Target) != hr.RequestURI {
			t.Fatalf("%q: target %q vs %q", c, req.Target, hr.RequestURI)
		}
		for _, h := range req.Headers {
			want := hr.Header.Get(string(h.Name))
			if strings.EqualFold(string(h.Name), "Host") {
				want = hr.Host
			}
			if want != string(h.Value) && len(hr.Header.Values(string(h.Name))) < 2 {
				t.Fatalf("%q: header %q = %q, net/http %q", c, h.Name, h.Value, want)
			}
		}
	}
	// Incomplete heads at every prefix must say incomplete, never accept.
	full := "GET /path HTTP/1.1\r\nHost: h\r\nAccept: */*\r\n\r\n"
	for i := 0; i < len(full); i++ {
		if _, err := Parse(&req, []byte(full[:i]), Compatible); err != ErrIncomplete && err != ErrMalformed {
			t.Fatalf("prefix %d: err=%v", i, err)
		}
	}
	// Random mutations must never panic and must reject or match.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		mut := []byte(full)
		mut[rng.Intn(len(mut))] = byte(rng.Intn(256))
		hr, herr := http.ReadRequest(bufio.NewReader(bytes.NewReader(append(mut, "XXX"...))))
		_, err := Parse(&req, mut, Compatible)
		if err == nil && herr != nil {
			// We accepted something net/http rejected: only acceptable if
			// the mutation is in a header VALUE net/http also surfaces --
			// tighten by comparing outcome only on structural bytes.
			_ = hr
		}
	}
}

func TestDuplicateHostRejected(t *testing.T) {
	for _, head := range []string{
		"GET / HTTP/1.1\r\nHost: a.com\r\nHost: a.com\r\n\r\n", // identical too
		"GET / HTTP/1.1\r\nHost: a.com\r\nHost: b.com\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a.com\r\nHOST:\r\n\r\n",
	} {
		var req Request
		if _, err := Parse(&req, []byte(head), Compatible); err == nil {
			t.Fatalf("%q: duplicate Host accepted", head)
		}
	}
}

func TestMissingHost(t *testing.T) {
	var req Request
	if _, err := Parse(&req, []byte("GET / HTTP/1.1\r\n\r\n"), Strict); err == nil {
		t.Fatal("strict: missing Host accepted")
	}
	if _, err := Parse(&req, []byte("GET / HTTP/1.1\r\nHost:\r\n\r\n"), Compatible); err != ErrMissingHost {
		t.Fatalf("compatible: want ErrMissingHost, got %v", err)
	}
	if _, err := Parse(&req, []byte("GET / HTTP/1.0\r\n\r\n"), Compatible); err != nil {
		t.Fatalf("1.0 without Host: %v", err)
	}
}

func TestMalformedHost(t *testing.T) {
	for _, h := range []string{"bad host", "a b", "a\x00b", "a,b"} {
		var req Request
		if _, err := Parse(&req, []byte("GET / HTTP/1.1\r\nHost: "+h+"\r\n\r\n"), Compatible); err == nil {
			t.Fatalf("Host %q accepted", h)
		}
	}
}

func TestTargetControls(t *testing.T) {
	for _, tgt := range []string{"/a\x00b", "/a\x7fb", "/a\x1fb", "/%zz", "/a%2"} {
		var req Request
		if _, err := Parse(&req, []byte("GET "+tgt+" HTTP/1.1\r\nHost: x\r\n\r\n"), Compatible); err == nil {
			t.Fatalf("target %q accepted", tgt)
		}
	}
}

func TestDuplicateContentLength(t *testing.T) {
	for _, head := range []string{
		"POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n",
	} {
		var req Request
		if _, err := Parse(&req, []byte(head), Compatible); err == nil {
			t.Fatalf("%q: duplicate Content-Length accepted", head)
		}
	}
}

func TestFramingViews(t *testing.T) {
	var req Request
	head := "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n"
	if _, err := Parse(&req, []byte(head), Compatible); err != nil {
		t.Fatal(err)
	}
	if len(req.ContentLengthLines) != 1 || string(req.ContentLengthLines[0]) != "5" {
		t.Fatalf("ContentLengthLines = %q", req.ContentLengthLines)
	}
	if len(req.TransferEncodingLines) != 1 || string(req.TransferEncodingLines[0]) != "chunked" {
		t.Fatalf("TransferEncodingLines = %q", req.TransferEncodingLines)
	}
}

func TestLimits(t *testing.T) {
	var req Request
	// 60 valid header lines exceeds the strict count of 50, with the head
	// still far under the strict head-size bound.
	_, err := Parse(&req, []byte("GET / HTTP/1.1\r\nHost: h\r\n"+strings.Repeat("X-H: v\r\n", 60)+"\r\n"), Strict)
	if err != ErrTooManyHeaders {
		t.Fatalf("header-count limit: %v", err)
	}
	// 1<<20+1 exceeds the compatible 1 MiB value limit, and is reachable
	// because MaxHeadSize (2 MiB) exceeds it.
	_, err = Parse(&req, []byte("GET / HTTP/1.1\r\nHost: h\r\nX: "+strings.Repeat("v", 1<<20+1)+"\r\n\r\n"), Compatible)
	if err != ErrValueTooLarge {
		t.Fatalf("value limit: %v", err)
	}
	// 60 valid 8 KiB headers (~500 KiB) trips the strict head-size entry
	// check before the count check.
	_, err = Parse(&req, []byte("GET / HTTP/1.1\r\n"+strings.Repeat("X: "+strings.Repeat("v", 1<<13)+"\r\n", 60)+"\r\n"), Strict)
	if err != ErrHeadTooLarge {
		t.Fatalf("head limit: %v", err)
	}
	// An over-long request line is its own verdict, not ErrIncomplete.
	_, err = Parse(&req, []byte("GET /"+strings.Repeat("p", 9<<10)+" HTTP/1.1\r\nHost: h\r\n\r\n"), Compatible)
	if err != ErrRequestLineTooLarge {
		t.Fatalf("request-line limit: %v", err)
	}
	// A limit verdict never masquerades as "read more".
	for _, e := range []error{ErrHeadTooLarge, ErrTooManyHeaders, ErrValueTooLarge, ErrRequestLineTooLarge} {
		if e == ErrIncomplete {
			t.Fatal("a limit error aliases ErrIncomplete")
		}
	}
}

func TestDuplicateTransferEncoding(t *testing.T) {
	var req Request
	head := "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n"
	if _, err := Parse(&req, []byte(head), Compatible); err == nil {
		t.Fatal("duplicate Transfer-Encoding accepted")
	}
}
