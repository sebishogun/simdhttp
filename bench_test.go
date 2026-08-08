package simdhttp

import (
	"bufio"
	"bytes"
	"net/http"
	"testing"
)

var realHead = []byte("GET /api/v2/items?page=3&limit=50 HTTP/1.1\r\n" +
	"Host: api.example.com\r\n" +
	"User-Agent: Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101\r\n" +
	"Accept: text/html,application/xhtml+xml,application/xml;q=0.9\r\n" +
	"Accept-Encoding: gzip, deflate, br\r\n" +
	"Accept-Language: en-US,en;q=0.5\r\n" +
	"Cookie: session=abc123def456; theme=dark; lang=en\r\n" +
	"Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9\r\n" +
	"Connection: keep-alive\r\n\r\n")

func BenchmarkParse(b *testing.B) {
	var req Request
	req.Headers = make([]Header, 0, 16)
	b.SetBytes(int64(len(realHead)))
	b.Run("simdhttp", func(b *testing.B) {
		for b.Loop() {
			if _, err := Parse(&req, realHead); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("net-http", func(b *testing.B) {
		br := bufio.NewReader(nil)
		for b.Loop() {
			br.Reset(bytes.NewReader(realHead))
			if _, err := http.ReadRequest(br); err != nil {
				b.Fatal(err)
			}
		}
	})
}
