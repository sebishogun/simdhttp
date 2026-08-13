package http1

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
)

// The request-smuggling corpus of docs/verification.md section 3. Every input
// gets one verdict from one parser, so the "two parsers disagree" class has
// nothing to disagree with. Rows marked deviation carry the probed Go verdict
// so the corpus doubles as the deviation record.
func TestSmugglingCorpus(t *testing.T) {
	long := strings.Repeat("v", 70)
	cases := []struct {
		name   string
		head   string
		accept bool // simdhttp's verdict
		note   string
	}{
		// Framing pairs.
		{"CL.CL equal", "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\n", false, "D6: Go dedupes identical CL"},
		{"CL.CL differing", "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n", false, "parity"},
		{"CL+TE", "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n", true, "head parses; D7 verdict is the framing table's"},
		{"TE.TE", "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n", false, "parity: net/http answers too many transfer encodings"},
		// Line endings and name shapes.
		{"bare LF", "GET / HTTP/1.1\nHost: h\n\n", false, "stricter than net/textproto, by design"},
		{"CR without LF", "GET / HTTP/1.1\rHost: h\r\r", false, ""},
		{"space in name", "GET / HTTP/1.1\r\nHost: h\r\nBad Name: x\r\n\r\n", false, "stricter than net/textproto, by design"},
		{"obs-fold", "GET / HTTP/1.1\r\nHost: h\r\nX: a\r\n b\r\n\r\n", false, "continuation line is not a token: name"},
		// Request line shapes.
		{"tab between method and target", "GET\t/ HTTP/1.1\r\nHost: h\r\n\r\n", false, ""},
		{"multiple spaces", "GET  / HTTP/1.1\r\nHost: h\r\n\r\n", false, "empty target"},
		{"empty target", "GET  HTTP/1.1\r\nHost: h\r\n\r\n", false, ""},
		{"asterisk-form", "OPTIONS * HTTP/1.1\r\nHost: h\r\n\r\n", true, ""},
		{"absolute-form", "GET http://h/p HTTP/1.1\r\nHost: h\r\n\r\n", true, ""},
		// Host rows.
		{"Host duplicate", "GET / HTTP/1.1\r\nHost: a\r\nHost: a\r\n\r\n", false, "G2, pinned by the fuzz seed"},
		{"Host empty", "GET / HTTP/1.1\r\nHost:\r\n\r\n", false, "D5: Go accepts present-but-empty"},
		{"Host missing 1.1", "GET / HTTP/1.1\r\n\r\n", false, "parity with the Go server"},
		{"Host missing 1.0", "GET / HTTP/1.0\r\n\r\n", true, "1.0 may omit Host"},
		{"Host comma", "GET / HTTP/1.1\r\nHost: a.com,b.com\r\n\r\n", false, "D9: Go accepts"},
		{"Host comma space", "GET / HTTP/1.1\r\nHost: a.com, b.com\r\n\r\n", false, "D9"},
		{"Host IPv6 balanced", "GET / HTTP/1.1\r\nHost: [::1]:80\r\n\r\n", true, ""},
		{"Host IPv6 unbalanced", "GET / HTTP/1.1\r\nHost: [::1\r\n\r\n", false, "D9: Go accepts"},
		{"Host control", "GET / HTTP/1.1\r\nHost: a\x00b\r\n\r\n", false, ""},
		// Targets.
		{"target NUL", "GET /a\x00b HTTP/1.1\r\nHost: h\r\n\r\n", false, "D8 parity"},
		{"target DEL", "GET /a\x7fb HTTP/1.1\r\nHost: h\r\n\r\n", false, "D8 parity"},
		{"target bad escape", "GET /%zz HTTP/1.1\r\nHost: h\r\n\r\n", false, "D8 parity"},
		{"target short escape", "GET /a%2 HTTP/1.1\r\nHost: h\r\n\r\n", false, "D8 parity"},
		{"target %2F", "GET /a%2Fb HTTP/1.1\r\nHost: h\r\n\r\n", true, "valid escape, not decoded here"},
		// Versions -- D10: Go accepts these, simdhttp does not.
		{"HTTP/1.2", "GET / HTTP/1.2\r\nHost: h\r\n\r\n", false, "D10"},
		{"HTTP/2.0", "GET / HTTP/2.0\r\nHost: h\r\n\r\n", false, "D10"},
		{"PRI preface", "PRI * HTTP/2.0\r\n\r\n", false, "D10"},
		{"HTTP/9.9", "GET / HTTP/9.9\r\nHost: h\r\n\r\n", false, "D10"},
		{"TE empty", "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding:\r\n\r\n", false, "parity: unsupported transfer encoding"},
		{"TE gzip", "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: gzip\r\n\r\n", false, "parity"},
		{"TE list", "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: gzip, chunked\r\n\r\n", false, "parity"},
		{"TE identity", "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: identity\r\n\r\n", false, "parity"},
		{"TE chunked cased", "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: ChUnKeD\r\n\r\n", true, "case-insensitive token"},
		// The G1 shape: a control byte behind a tab in a long value.
		{"G1 control after tab", "GET / HTTP/1.1\r\nHost: h\r\nX: " + long + "\t\x00\r\n\r\n", false, "G1"},
		{"long value with tabs only", "GET / HTTP/1.1\r\nHost: h\r\nX: " + long + "\tmore\r\n\r\n", true, "tabs are legal in a value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req Request
			_, err := Parse(&req, []byte(c.head), Compatible)
			if c.accept && err != nil {
				t.Fatalf("rejected (%v); expected accept [%s]", err, c.note)
			}
			if !c.accept && err == nil {
				t.Fatalf("accepted; expected reject [%s]", c.note)
			}
			// The security direction: never accept what net/http rejects.
			if err == nil {
				if _, herr := http.ReadRequest(bufio.NewReader(strings.NewReader(c.head))); herr != nil {
					t.Fatalf("net/http rejects (%v), we accept [%s]", herr, c.note)
				}
			}
		})
	}
}

// Oversized rows: each limit has a verdict, and none of them is "read more".
func TestOversizedCorpus(t *testing.T) {
	var req Request
	for _, c := range []struct {
		name string
		head string
		want error
	}{
		{"head past limit", "GET / HTTP/1.1\r\n" + strings.Repeat("X: "+strings.Repeat("v", 1<<13)+"\r\n", 60) + "\r\n", ErrHeadTooLarge},
		{"count past limit", "GET / HTTP/1.1\r\nHost: h\r\n" + strings.Repeat("X-H: v\r\n", 60) + "\r\n", ErrTooManyHeaders},
		{"request line past limit", "GET /" + strings.Repeat("p", 5<<10) + " HTTP/1.1\r\nHost: h\r\n\r\n", ErrRequestLineTooLarge},
	} {
		if _, err := Parse(&req, []byte(c.head), Strict); err != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// Truncation at every byte: always incomplete or malformed, never accepted.
func TestTruncationSweep(t *testing.T) {
	var req Request
	for _, full := range []string{
		"GET /p HTTP/1.1\r\nHost: h\r\nAccept: */*\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: h\r\nX: " + strings.Repeat("v", 70) + "\t ok\r\n\r\n",
	} {
		for i := 0; i < len(full); i++ {
			if _, err := Parse(&req, []byte(full[:i]), Compatible); err == nil {
				t.Fatalf("prefix %d of %q accepted", i, full)
			}
		}
	}
}
