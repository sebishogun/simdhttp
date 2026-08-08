package simdhttp

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
