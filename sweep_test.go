package simdhttp

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func makeHead(nHeaders, valLen int) []byte {
	var b strings.Builder
	b.WriteString("GET /path HTTP/1.1\r\n")
	for i := 0; i < nHeaders; i++ {
		fmt.Fprintf(&b, "X-Header-%d: %s\r\n", i, strings.Repeat("v", valLen))
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

func BenchmarkSweep(b *testing.B) {
	shapes := []struct {
		name             string
		nHeaders, valLen int
	}{
		{"none", 0, 0},
		{"typical-9", 9, 30},
		{"many-100", 100, 20},
		{"giant-value", 4, 8000},
	}
	var req Request
	req.Headers = make([]Header, 0, 128)
	for _, s := range shapes {
		head := makeHead(s.nHeaders, s.valLen)
		b.Run("simdhttp/"+s.name, func(b *testing.B) {
			b.SetBytes(int64(len(head)))
			for b.Loop() {
				if _, err := Parse(&req, head); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("nethttp/"+s.name, func(b *testing.B) {
			b.SetBytes(int64(len(head)))
			br := bufio.NewReaderSize(nil, 1<<16)
			for b.Loop() {
				br.Reset(bytes.NewReader(head))
				if _, err := http.ReadRequest(br); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
