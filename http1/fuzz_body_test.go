package http1

import (
	"bufio"
	"bytes"
	"io"
	"testing"
	"time"
)

// FuzzBodyReader asserts the three properties the reader owes a server loop on
// arbitrary bytes from the wire: it does not panic, it does not hang, and it
// never returns (0, nil) for a non-empty buffer. The last is the one a caller
// cannot defend against -- a loop that waits for n > 0 spins forever.
func FuzzBodyReader(f *testing.F) {
	for _, s := range []string{
		"hello",
		"5\r\nhello\r\n0\r\n\r\n",
		"5\r\nhello\r\n0\r\nX-T: v\r\n\r\n",
		"5;ext=1\r\nhello\r\n3\r\nabc\r\n0\r\n\r\n",
		"0\r\n\r\n",
		"zz\r\n",
		"7fffffffffffffff0\r\n",
		"5\r\nhello\n0\r\n\r\n",
		"ffffffff\r\n",
	} {
		f.Add([]byte(s), uint8(0))
		f.Add([]byte(s), uint8(1))
		f.Add([]byte(s), uint8(2))
	}
	f.Fuzz(func(t *testing.T, in []byte, mode uint8) {
		framing := Chunked
		switch mode % 3 {
		case 0:
			framing = FixedLength(int64(len(in)) / 2)
		case 1:
			framing = NoBody
		}
		lim := DefaultLimits(Compatible)
		// Small budgets so the limit paths are reached on short inputs.
		lim.MaxBodySize, lim.MaxDrainSize = 4096, 1024
		lim.MaxChunkSizeLine, lim.MaxTrailerCount = 256, 8

		done := make(chan struct{})
		go func() {
			defer close(done)
			b := NewBodyReader(bufio.NewReader(bytes.NewReader(in)), framing, lim)
			p := make([]byte, 64)
			for i := 0; i < 4096; i++ {
				n, err := b.Read(p)
				if n == 0 && err == nil {
					t.Errorf("(0, nil) on %q framing %v", in, framing.Kind)
					return
				}
				if n < 0 || n > len(p) {
					t.Errorf("read reported %d bytes into a %d buffer", n, len(p))
					return
				}
				if err != nil {
					break
				}
			}
			// Close after any outcome: a server loop closes in a defer.
			b.Close()
			b.Close()
			if b.Consumed() < 0 || b.Consumed() > int64(len(in)) {
				t.Errorf("consumed %d off a %d-byte stream", b.Consumed(), len(in))
			}
			if b.Drained() && b.Consumed() > int64(len(in)) {
				t.Errorf("drained past the input")
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("body reader hung on %q framing %v", in, framing.Kind)
		}
	})
}

// FuzzBodyDrainLeavesPipelined: whatever the first body does, a drained
// connection must be positioned so the following request parses. Anything less
// is a smuggling primitive -- the next head would start inside this body.
func FuzzBodyDrainLeavesPipelined(f *testing.F) {
	f.Add([]byte("hello"), uint8(5))
	f.Add([]byte("5\r\nhello\r\n0\r\n\r\n"), uint8(255))
	f.Fuzz(func(t *testing.T, body []byte, n uint8) {
		const next = "GET /next HTTP/1.1\r\nHost: h\r\n\r\n"
		framing := Chunked
		if n != 255 {
			if int(n) > len(body) {
				return
			}
			framing = FixedLength(int64(n))
		}
		br := bufio.NewReader(bytes.NewReader(append(append([]byte{}, body...), next...)))
		b := NewBodyReader(br, framing, DefaultLimits(Compatible))
		if err := b.Close(); err != nil || !b.Drained() {
			return // an undrainable body closes the connection; nothing to check
		}
		rest, _ := io.ReadAll(br)
		if framing.Kind == KindFixed && string(rest) != string(body[n:])+next {
			t.Fatalf("after draining %d of %q the wire holds %q", n, body, rest)
		}
	})
}
