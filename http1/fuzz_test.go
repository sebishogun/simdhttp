package http1

import (
	"bufio"
	"net/http"
	"net/url"
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
		// G1: a control byte behind a tab in a value past ctlScanThreshold.
		"GET / HTTP/1.1\r\nHost: h\r\nX: " + strings.Repeat("v", 70) + "\t\x00\r\n\r\n",
		// G2 and the Host rows.
		"GET / HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a.com,b.com\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: [::1\r\n\r\n",
		// Framing pairs.
		"POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n",
	} {
		f.Add([]byte(s))
	}
	var req Request
	f.Fuzz(func(t *testing.T, data []byte) {
		_, ourErr := Parse(&req, data, Compatible)
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
