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
		for _, f := range []Framing{NoBody, FixedLength(0), FixedLength(5), Chunked} {
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

func TestBodyChunkedTrailers(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(
		"5\r\nhello\r\n0\r\nX-Trailer: v\r\n\r\n"))
	b := NewBodyReader(br, Chunked, DefaultLimits(Compatible))
	all, err := io.ReadAll(b)
	if err != nil || string(all) != "hello" {
		t.Fatalf("body %q err %v", all, err)
	}
	if b.Trailers().Get("X-Trailer") != "v" {
		t.Fatalf("trailers %v", b.Trailers())
	}
}

func TestBodyChunkedShapes(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string
		rest string
	}{
		{"single", "5\r\nhello\r\n0\r\n\r\n", "hello", ""},
		{"multi", "3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n", "abcde", ""},
		{"empty body", "0\r\n\r\n", "", ""},
		{"uppercase hex", "A\r\n0123456789\r\n0\r\n\r\n", "0123456789", ""},
		{"lowercase hex", "a\r\n0123456789\r\n0\r\n\r\n", "0123456789", ""},
		{"extension", "5;name=value\r\nhello\r\n0\r\n\r\n", "hello", ""},
		{"extension on last", "0;done\r\n\r\n", "", ""},
		{"pipelined after", "5\r\nhello\r\n0\r\n\r\nGET / HTTP/1.1\r\n\r\n", "hello", "GET / HTTP/1.1\r\n\r\n"},
		{"trailers then next", "0\r\nX-A: 1\r\n\r\nNEXT", "", "NEXT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in))
			b := NewBodyReader(br, Chunked, DefaultLimits(Compatible))
			all, err := io.ReadAll(b)
			if err != nil {
				t.Fatalf("err %v (read %q)", err, all)
			}
			if string(all) != c.want {
				t.Fatalf("body %q, want %q", all, c.want)
			}
			if rest, _ := io.ReadAll(br); string(rest) != c.rest {
				t.Fatalf("remainder %q, want %q", rest, c.rest)
			}
			if b.Consumed() != int64(len(c.in)-len(c.rest)) {
				t.Fatalf("consumed %d, want %d", b.Consumed(), len(c.in)-len(c.rest))
			}
		})
	}
}

// Everything a hostile peer can put in a chunked stream. Each must be a typed
// error, never a panic, never a hang, and never a silently shorter body.
func TestBodyChunkedRejects(t *testing.T) {
	big := strings.Repeat("f", 9000)
	for _, c := range []struct {
		name string
		in   string
		want error
	}{
		{"truncated size line", "5", io.ErrUnexpectedEOF},
		{"truncated data", "5\r\nhel", io.ErrUnexpectedEOF},
		{"missing final crlf", "5\r\nhello\r\n0\r\n", io.ErrUnexpectedEOF},
		{"no terminator", "5\r\nhello\r\n", io.ErrUnexpectedEOF},
		{"bad size", "zz\r\n", ErrBadChunk},
		{"empty size", "\r\n", ErrBadChunk},
		{"negative size", "-1\r\n", ErrBadChunk},
		{"size overflow", "7fffffffffffffff0\r\n", ErrBadChunk},
		{"data not followed by crlf", "5\r\nhelloXX", ErrBadChunk},
		{"lone lf after data", "5\r\nhello\n0\r\n\r\n", ErrBadChunk},
		{"space in size line", "5 5\r\nhello\r\n", ErrBadChunk},
		{"size line too long", big + "\r\n", ErrChunkLineTooLong},
		{"control byte in trailer", "0\r\nX-A: \x01\r\n\r\n", ErrMalformed},
		{"bare lf ending trailer", "0\r\nX-A: 1\n\r\n", ErrMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in))
			b := NewBodyReader(br, Chunked, DefaultLimits(Compatible))
			_, err := io.ReadAll(b)
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			// Sticky: a caller that ignores the error gets it again, not bytes.
			if _, again := b.Read(make([]byte, 4)); !errors.Is(again, c.want) {
				t.Fatalf("second read %v, want the same error", again)
			}
		})
	}
}

// Truncating a valid chunked stream at every byte must never panic or hang;
// every prefix short of the whole thing must fail rather than report a
// complete body.
func TestBodyChunkedTruncatedAtEveryByte(t *testing.T) {
	const full = "5;ext=1\r\nhello\r\n3\r\nabc\r\n0\r\nX-T: v\r\n\r\n"
	for i := 0; i < len(full); i++ {
		br := bufio.NewReader(strings.NewReader(full[:i]))
		b := NewBodyReader(br, Chunked, DefaultLimits(Compatible))
		if _, err := io.ReadAll(b); err == nil {
			t.Fatalf("prefix %d (%q) reported a complete body", i, full[:i])
		}
	}
	br := bufio.NewReader(strings.NewReader(full))
	b := NewBodyReader(br, Chunked, DefaultLimits(Compatible))
	if all, err := io.ReadAll(b); err != nil || string(all) != "helloabc" {
		t.Fatalf("whole stream: %q %v", all, err)
	}
}

func TestBodyChunkedLimits(t *testing.T) {
	t.Run("body size", func(t *testing.T) {
		lim := DefaultLimits(Compatible)
		lim.MaxBodySize = 4
		br := bufio.NewReader(strings.NewReader("5\r\nhello\r\n0\r\n\r\n"))
		b := NewBodyReader(br, Chunked, lim)
		if _, err := io.ReadAll(b); !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("err %v, want ErrBodyTooLarge", err)
		}
	})
	t.Run("extension length", func(t *testing.T) {
		lim := DefaultLimits(Compatible)
		lim.MaxChunkExtensionLen = 8
		br := bufio.NewReader(strings.NewReader("5;" + strings.Repeat("x", 40) + "\r\nhello\r\n0\r\n\r\n"))
		b := NewBodyReader(br, Chunked, lim)
		if _, err := io.ReadAll(b); !errors.Is(err, ErrChunkExtensionTooLong) {
			t.Fatalf("err %v, want ErrChunkExtensionTooLong", err)
		}
	})
	t.Run("trailer count", func(t *testing.T) {
		lim := DefaultLimits(Compatible)
		lim.MaxTrailerCount = 2
		var sb strings.Builder
		sb.WriteString("0\r\n")
		for i := 0; i < 5; i++ {
			sb.WriteString("X-A: 1\r\n")
		}
		sb.WriteString("\r\n")
		br := bufio.NewReader(strings.NewReader(sb.String()))
		b := NewBodyReader(br, Chunked, lim)
		if _, err := io.ReadAll(b); !errors.Is(err, ErrTooManyTrailers) {
			t.Fatalf("err %v, want ErrTooManyTrailers", err)
		}
	})
	t.Run("trailer value length", func(t *testing.T) {
		lim := DefaultLimits(Compatible)
		lim.MaxTrailerValueLen = 8
		br := bufio.NewReader(strings.NewReader("0\r\nX-A: " + strings.Repeat("v", 40) + "\r\n\r\n"))
		b := NewBodyReader(br, Chunked, lim)
		if _, err := io.ReadAll(b); !errors.Is(err, ErrValueTooLarge) {
			t.Fatalf("err %v, want ErrValueTooLarge", err)
		}
	})
}

// Trailers are only meaningful once the stream ended; before EOF the map must
// not promise fields that have not arrived.
func TestBodyTrailersOnlyAfterEOF(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("5\r\nhello\r\n0\r\nX-T: v\r\n\r\n"))
	b := NewBodyReader(br, Chunked, DefaultLimits(Compatible))
	if len(b.Trailers()) != 0 {
		t.Fatalf("trailers before any read: %v", b.Trailers())
	}
	if _, err := b.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	if len(b.Trailers()) != 0 {
		t.Fatalf("trailers mid-body: %v", b.Trailers())
	}
	io.ReadAll(b)
	if b.Trailers().Get("X-T") != "v" {
		t.Fatalf("trailers after EOF: %v", b.Trailers())
	}
}

// Close before EOF must leave the connection positioned exactly at the next
// request, or the next head parse reads a body's tail as a request line.
func TestBodyCloseDrainsFixed(t *testing.T) {
	const next = "GET /next HTTP/1.1\r\nHost: h\r\n\r\n"
	br := bufio.NewReader(strings.NewReader("hello" + next))
	b := NewBodyReader(br, FixedLength(5), DefaultLimits(Compatible))
	if _, err := b.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !b.Drained() {
		t.Fatal("Drained reports false after a completed drain")
	}
	if rest, _ := io.ReadAll(br); string(rest) != next {
		t.Fatalf("remainder %q, want %q", rest, next)
	}
	if b.Consumed() != 5 {
		t.Fatalf("consumed %d, want 5", b.Consumed())
	}
}

func TestBodyCloseDrainsChunked(t *testing.T) {
	const next = "GET /next HTTP/1.1\r\nHost: h\r\n\r\n"
	br := bufio.NewReader(strings.NewReader("5\r\nhello\r\n3\r\nabc\r\n0\r\nX-T: v\r\n\r\n" + next))
	b := NewBodyReader(br, Chunked, DefaultLimits(Compatible))
	if _, err := b.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !b.Drained() {
		t.Fatal("Drained reports false after a completed drain")
	}
	if rest, _ := io.ReadAll(br); string(rest) != next {
		t.Fatalf("remainder %q, want %q", rest, next)
	}
}

// A body past the drain budget must abort rather than read it: draining an
// unbounded body on the peer's schedule is the denial of service the budget
// exists to prevent. The caller closes such a connection.
func TestBodyCloseAbortsPastDrainBudget(t *testing.T) {
	lim := DefaultLimits(Compatible)
	lim.MaxDrainSize = 16
	br := bufio.NewReader(strings.NewReader(strings.Repeat("x", 4096)))
	b := NewBodyReader(br, FixedLength(4096), lim)
	err := b.Close()
	if !errors.Is(err, ErrDrainTooLarge) {
		t.Fatalf("close: %v, want ErrDrainTooLarge", err)
	}
	if b.Drained() {
		t.Fatal("Drained reports true after an aborted drain")
	}
}

// Close is idempotent and always safe: a server loop closes in a defer whether
// or not the handler already read the body to EOF.
func TestBodyCloseIdempotent(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		f    Framing
	}{
		{"fixed", "hello", FixedLength(5)},
		{"chunked", "5\r\nhello\r\n0\r\n\r\n", Chunked},
		{"none", "", NoBody},
	} {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in))
			b := NewBodyReader(br, c.f, DefaultLimits(Compatible))
			io.ReadAll(b)
			for i := 0; i < 3; i++ {
				if err := b.Close(); err != nil {
					t.Fatalf("close %d: %v", i, err)
				}
			}
			if !b.Drained() {
				t.Fatal("Drained false after reading to EOF")
			}
		})
	}
}

// Closing a body whose framing is already broken must not hang or panic; the
// connection is not reusable and Drained says so.
func TestBodyCloseAfterError(t *testing.T) {
	for _, in := range []string{"zz\r\n", "5\r\nhelloXX", "hel"} {
		f := Chunked
		if in == "hel" {
			f = FixedLength(5)
		}
		br := bufio.NewReader(strings.NewReader(in))
		b := NewBodyReader(br, f, DefaultLimits(Compatible))
		io.ReadAll(b)
		if err := b.Close(); err == nil {
			t.Fatalf("%q: close reported success on a broken body", in)
		}
		if b.Drained() {
			t.Fatalf("%q: Drained true after a framing error", in)
		}
	}
}

// The pipelining contract: after a body is read or drained, the next head
// parses from the connection with no offset bookkeeping by the caller.
func TestBodyPipelinedHeadParses(t *testing.T) {
	const second = "GET /second HTTP/1.1\r\nHost: h\r\n\r\n"
	for _, c := range []struct {
		name  string
		first string
		f     Framing
		drain bool
	}{
		{"fixed read", "hello", FixedLength(5), false},
		{"fixed drained", "hello", FixedLength(5), true},
		{"chunked read", "5\r\nhello\r\n0\r\n\r\n", Chunked, false},
		{"chunked drained", "5\r\nhello\r\n0\r\nX-T: v\r\n\r\n", Chunked, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.first + second))
			b := NewBodyReader(br, c.f, DefaultLimits(Compatible))
			if c.drain {
				if err := b.Close(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := io.ReadAll(b); err != nil {
				t.Fatal(err)
			}
			head, err := io.ReadAll(br)
			if err != nil {
				t.Fatal(err)
			}
			var req Request
			n, err := Parse(&req, head, Compatible)
			if err != nil {
				t.Fatalf("second head: %v (bytes %q)", err, head)
			}
			if n != len(second) || string(req.Target) != "/second" {
				t.Fatalf("parsed %d bytes, target %q", n, req.Target)
			}
		})
	}
}
