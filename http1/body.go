package http1

import (
	"bufio"
	"errors"
	"io"
)

// The body reader. It serves exactly the bytes the framing table said the body
// contains and not one more: the byte after the body is the next request on a
// pipelined connection, and a reader that swallows it has performed the second
// half of a smuggling attack on its own caller.
//
// Limits are enforced per docs/lld/http1-body-framing.md section 3, as bytes
// served rather than bytes allocated -- the reader streams, holding only the
// current chunk and the trailer section.

// Limits bounds everything a body can spend on the server's behalf. The
// defaults come from the LLD's table and differ by profile; a caller may lower
// any field, and TestDefaultLimitsMatchLLD pins the table itself.
type Limits struct {
	MaxBodySize          int64 // bytes served to the caller
	MaxChunkSizeLine     int   // a chunk-size line including any extension
	MaxChunkExtensionLen int   // the extension part of that line alone
	MaxTrailerCount      int   // trailer fields after the final chunk
	MaxTrailerValueLen   int   // one trailer field value
	MaxDrainSize         int64 // bytes Close will read to reuse the connection
}

// DefaultLimits returns the profile's limits from the LLD table.
func DefaultLimits(profile Profile) Limits {
	if profile == Strict {
		return Limits{
			MaxBodySize:          4 << 20,
			MaxChunkSizeLine:     4 << 10,
			MaxChunkExtensionLen: 1 << 10,
			MaxTrailerCount:      50,
			MaxTrailerValueLen:   64 << 10,
			MaxDrainSize:         256 << 10,
		}
	}
	return Limits{
		MaxBodySize:          32 << 20,
		MaxChunkSizeLine:     8 << 10,
		MaxChunkExtensionLen: 4 << 10,
		MaxTrailerCount:      100,
		MaxTrailerValueLen:   1 << 20,
		MaxDrainSize:         1 << 20,
	}
}

// NoBody and Chunked are the two framings that carry no length; FixedLength
// carries one. They are values rather than an interface so the hot Read loop
// switches on a byte instead of dispatching through a method table.
var (
	NoBody  = Framing{Kind: KindNone}
	Chunked = Framing{Kind: KindChunked}
)

// FixedLength frames a body of exactly n bytes.
func FixedLength(n int64) Framing { return Framing{Kind: KindFixed, Length: n} }

// ErrBodyTooLarge means the body exceeded Limits.MaxBodySize.
var ErrBodyTooLarge = errors.New("simdhttp: body too large")

// BodyReader serves one request's body from a connection's buffered reader.
// It is not safe for concurrent use; one request owns one reader.
type BodyReader struct {
	br   *bufio.Reader
	lim  Limits
	kind Kind

	remain   int64 // fixed framing: body bytes still owed
	served   int64 // body bytes handed to the caller
	consumed int64 // bytes taken off the connection for this body
	err      error // sticky: once set, every Read repeats it
}

// NewBodyReader returns a reader over the body that follows a head. The
// framing must come from FramingOf, so the length decision has exactly one
// author.
func NewBodyReader(br *bufio.Reader, framing Framing, limits Limits) *BodyReader {
	b := &BodyReader{br: br, lim: limits, kind: framing.Kind}
	if framing.Kind == KindFixed {
		b.remain = framing.Length
		// A declared length over the budget is refused before a byte moves.
		// The limit is about bytes served, and none will be; reading 32 MiB to
		// discover the caller declared 33 is the denial of service the limit
		// exists to prevent.
		if framing.Length > limits.MaxBodySize {
			b.err = ErrBodyTooLarge
		}
	}
	return b
}

// Read implements io.Reader. It never returns (0, nil) for a non-empty p: a
// caller looping until n > 0 without inspecting err would spin forever, and
// the input that triggers it arrives from the wire.
func (b *BodyReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	switch b.kind {
	case KindFixed:
		return b.readFixed(p)
	case KindChunked:
		// The chunked reader lands next; until it does this states so rather
		// than serving a body it cannot frame.
		return b.fail(errChunkedNotWired)
	default:
		return b.fail(io.EOF)
	}
}

func (b *BodyReader) readFixed(p []byte) (int, error) {
	if b.remain == 0 {
		return b.fail(io.EOF)
	}
	if b.served >= b.lim.MaxBodySize {
		return b.fail(ErrBodyTooLarge)
	}
	if room := b.lim.MaxBodySize - b.served; int64(len(p)) > room {
		p = p[:room]
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.br.Read(p)
	b.served += int64(n)
	b.consumed += int64(n)
	b.remain -= int64(n)
	switch {
	case b.remain == 0 && err == nil:
		// Do not report EOF yet: the caller gets the bytes now and EOF on the
		// next call, so a short final read is never mistaken for truncation.
		return n, nil
	case err == io.EOF && b.remain > 0:
		// The peer closed mid-body. Reporting io.EOF here would hand the
		// caller a truncated body as though it were whole.
		b.err = io.ErrUnexpectedEOF
		return n, b.err
	case err != nil:
		b.err = err
		return n, err
	case n == 0:
		// bufio can legally return (0, nil); loop rather than pass it on.
		return b.readFixed(p)
	}
	return n, nil
}

// fail records a terminal condition and reports it. Once set the reader stays
// in that state, so a caller that ignores the first error cannot walk past it
// into the next request's bytes.
func (b *BodyReader) fail(err error) (int, error) {
	b.err = err
	return 0, err
}

// Consumed reports the bytes this body took off the connection, including
// chunk framing. The next head parse starts there.
func (b *BodyReader) Consumed() int64 { return b.consumed }

var errChunkedNotWired = errors.New("simdhttp: chunked body reader not wired")
