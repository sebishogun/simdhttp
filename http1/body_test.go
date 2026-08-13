package http1

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestBodyFixedLength(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("helloNEXT"))
	b := NewBodyReader(br, FixedLength(5), DefaultLimits(Compatible))
	p := make([]byte, 2)
	got := ""
	for {
		n, err := b.Read(p)
		got += string(p[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if got != "hello" || b.Consumed() != 5 {
		t.Fatalf("got %q consumed %d", got, b.Consumed())
	}
}

// The bytes after the declared length are the NEXT request on a pipelined
// connection. Reading one byte of them here is a request-smuggling primitive,
// so this asserts the reader stops exactly on the boundary and leaves the
// remainder for the next head parse.
func TestBodyFixedLeavesPipelinedBytes(t *testing.T) {
	const next = "GET /next HTTP/1.1\r\nHost: h\r\n\r\n"
	br := bufio.NewReader(strings.NewReader("hello" + next))
	b := NewBodyReader(br, FixedLength(5), DefaultLimits(Compatible))
	if all, err := io.ReadAll(b); err != nil || string(all) != "hello" {
		t.Fatalf("body %q err %v", all, err)
	}
	rest, _ := io.ReadAll(br)
	if string(rest) != next {
		t.Fatalf("pipelined remainder %q, want %q", rest, next)
	}
}

func TestBodyFixedZeroLength(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("NEXT"))
	b := NewBodyReader(br, FixedLength(0), DefaultLimits(Compatible))
	n, err := b.Read(make([]byte, 4))
	if n != 0 || err != io.EOF {
		t.Fatalf("read %d %v, want 0 io.EOF", n, err)
	}
	if rest, _ := io.ReadAll(br); string(rest) != "NEXT" {
		t.Fatalf("consumed the next request: %q", rest)
	}
}

func TestBodyNoneIsEmpty(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("NEXT"))
	b := NewBodyReader(br, NoBody, DefaultLimits(Compatible))
	if all, err := io.ReadAll(b); err != nil || len(all) != 0 {
		t.Fatalf("body %q err %v", all, err)
	}
	if b.Consumed() != 0 {
		t.Fatalf("consumed %d", b.Consumed())
	}
}

// A short body is a truncation, not an EOF: reporting io.EOF would hand the
// caller a body it never received as if it were complete.
func TestBodyFixedTruncated(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("hel"))
	b := NewBodyReader(br, FixedLength(5), DefaultLimits(Compatible))
	all, err := io.ReadAll(b)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err %v, want io.ErrUnexpectedEOF (read %q)", err, all)
	}
	if string(all) != "hel" {
		t.Fatalf("partial body %q", all)
	}
}

func TestBodyFixedOverLimit(t *testing.T) {
	lim := DefaultLimits(Compatible)
	lim.MaxBodySize = 4
	br := bufio.NewReader(strings.NewReader("hello"))
	b := NewBodyReader(br, FixedLength(5), lim)
	_, err := io.ReadAll(b)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err %v, want ErrBodyTooLarge", err)
	}
}

// Read must never return (0, nil): a caller looping on n==0 without checking
// err spins forever, which is a denial of service reachable from the wire.
func TestBodyReadNeverZeroNil(t *testing.T) {
	for _, in := range []string{"", "hello", "hellomore"} {
		// Chunked joins this table when the chunked reader lands.
		for _, f := range []Framing{NoBody, FixedLength(0), FixedLength(5)} {
			br := bufio.NewReader(strings.NewReader(in))
			b := NewBodyReader(br, f, DefaultLimits(Compatible))
			for i := 0; i < 64; i++ {
				n, err := b.Read(make([]byte, 8))
				if n == 0 && err == nil {
					t.Fatalf("(0, nil) from %q framing %v", in, f.Kind)
				}
				if err != nil {
					break
				}
			}
		}
	}
}

// A zero-length p is the one case where (0, nil) is correct per io.Reader.
func TestBodyEmptyBufferIsNoOp(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("hello"))
	b := NewBodyReader(br, FixedLength(5), DefaultLimits(Compatible))
	if n, err := b.Read(nil); n != 0 || err != nil {
		t.Fatalf("read(nil) = %d, %v", n, err)
	}
	if all, _ := io.ReadAll(b); string(all) != "hello" {
		t.Fatalf("body %q", all)
	}
}

func TestDefaultLimitsMatchLLD(t *testing.T) {
	c, s := DefaultLimits(Compatible), DefaultLimits(Strict)
	for _, tc := range []struct {
		name    string
		got     int64
		want    int64
		profile string
	}{
		{"MaxBodySize", c.MaxBodySize, 32 << 20, "compatible"},
		{"MaxChunkSizeLine", int64(c.MaxChunkSizeLine), 8 << 10, "compatible"},
		{"MaxChunkExtensionLen", int64(c.MaxChunkExtensionLen), 4 << 10, "compatible"},
		{"MaxTrailerCount", int64(c.MaxTrailerCount), 100, "compatible"},
		{"MaxTrailerValueLen", int64(c.MaxTrailerValueLen), 1 << 20, "compatible"},
		{"MaxDrainSize", c.MaxDrainSize, 1 << 20, "compatible"},
		{"MaxBodySize", s.MaxBodySize, 4 << 20, "strict"},
		{"MaxChunkSizeLine", int64(s.MaxChunkSizeLine), 4 << 10, "strict"},
		{"MaxChunkExtensionLen", int64(s.MaxChunkExtensionLen), 1 << 10, "strict"},
		{"MaxTrailerCount", int64(s.MaxTrailerCount), 50, "strict"},
		{"MaxTrailerValueLen", int64(s.MaxTrailerValueLen), 64 << 10, "strict"},
		{"MaxDrainSize", s.MaxDrainSize, 256 << 10, "strict"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s (%s) = %d, the LLD table says %d", tc.name, tc.profile, tc.got, tc.want)
		}
	}
}
