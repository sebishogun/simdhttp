package http1

import (
	"bufio"
	"errors"
	"io"
	"net/http"
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

var (
	// ErrBodyTooLarge means the body exceeded Limits.MaxBodySize.
	ErrBodyTooLarge = errors.New("simdhttp: body too large")
	// ErrBadChunk means the chunked grammar was violated: a size that is not
	// hex, a chunk not closed by CRLF, or arithmetic that would overflow.
	ErrBadChunk = errors.New("simdhttp: malformed chunked encoding")
	// ErrChunkLineTooLong means a chunk-size line exceeded
	// Limits.MaxChunkSizeLine.
	ErrChunkLineTooLong = errors.New("simdhttp: chunk size line too long")
	// ErrChunkExtensionTooLong means a chunk extension exceeded
	// Limits.MaxChunkExtensionLen.
	ErrChunkExtensionTooLong = errors.New("simdhttp: chunk extension too long")
	// ErrTooManyTrailers means the trailer section exceeded
	// Limits.MaxTrailerCount.
	ErrTooManyTrailers = errors.New("simdhttp: too many trailers")
)

// errLineTooLong is internal: readLine reports it and each caller maps it to
// the limit that was actually exceeded.
var errLineTooLong = errors.New("simdhttp: line too long")

// maxTrailerNameLen bounds the name half of a trailer line so the line budget
// can be stated in terms of the value limit the caller set.
const maxTrailerNameLen = 256

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

	// chunked state
	state       chunkState
	chunkRemain int64       // bytes left in the chunk being served
	trailers    http.Header // filled only once the trailer section closes
	line        []byte      // scratch for a size or trailer line
}

// chunkState is where in the chunked grammar the next byte belongs. The
// reader is a state machine rather than nested loops so that a Read returning
// mid-chunk resumes at exactly the same place.
type chunkState uint8

const (
	csHeader   chunkState = iota // at a chunk-size line
	csData                       // inside chunk data
	csDataCRLF                   // at the CRLF that closes chunk data
	csTrailers                   // in the trailer section after the 0-chunk
	csDone                       // stream complete
)

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
		return b.readChunked(p)
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

// readChunked serves the next available body bytes, advancing the state
// machine as far as needed. It loops rather than returning (0, nil): a chunk
// header followed by data must not cost the caller an empty read.
func (b *BodyReader) readChunked(p []byte) (int, error) {
	for {
		switch b.state {
		case csHeader:
			size, err := b.readChunkHeader()
			if err != nil {
				return b.fail(err)
			}
			if size == 0 {
				b.state = csTrailers
				continue
			}
			b.chunkRemain = size
			b.state = csData

		case csData:
			q := p
			if int64(len(q)) > b.chunkRemain {
				q = q[:b.chunkRemain]
			}
			n, err := b.br.Read(q)
			b.served += int64(n)
			b.consumed += int64(n)
			b.chunkRemain -= int64(n)
			if b.chunkRemain == 0 {
				b.state = csDataCRLF
			}
			if err == io.EOF {
				// The peer closed inside a chunk: the body is short, and
				// saying EOF here would present it as whole.
				b.err = io.ErrUnexpectedEOF
				return n, b.err
			}
			if err != nil {
				b.err = err
				return n, err
			}
			if n > 0 {
				return n, nil
			}
			// bufio returned (0, nil); go round rather than pass it on.

		case csDataCRLF:
			if err := b.expectCRLF(); err != nil {
				return b.fail(err)
			}
			b.state = csHeader

		case csTrailers:
			if err := b.readTrailers(); err != nil {
				return b.fail(err)
			}
			b.state = csDone

		default:
			return b.fail(io.EOF)
		}
	}
}

// readChunkHeader reads one chunk-size line and returns the size.
//
// The parse order is Go's: trailing whitespace off the whole line first, then
// the extension, then hex. Splitting on the semicolon first would accept
// "5 ;ext" -- which Go rejects -- and being more permissive than the origin
// server in front of which this runs is the entire smuggling seam.
func (b *BodyReader) readChunkHeader() (int64, error) {
	line, crlf, err := b.readLine(b.lim.MaxChunkSizeLine)
	if err == errLineTooLong {
		return 0, ErrChunkLineTooLong
	}
	if err != nil {
		return 0, err
	}
	if !crlf {
		return 0, ErrBadChunk // a bare LF is not a chunk terminator
	}
	for len(line) > 0 && (line[len(line)-1] == ' ' || line[len(line)-1] == '\t') {
		line = line[:len(line)-1] // RFC 7230 BWS, which Go also accepts
	}
	if i := indexByteIn(line, ';'); i >= 0 {
		if len(line)-i-1 > b.lim.MaxChunkExtensionLen {
			return 0, ErrChunkExtensionTooLong
		}
		line = line[:i]
	}
	size, ok := parseHexInt64(line)
	if !ok {
		return 0, ErrBadChunk
	}
	// A chunk that would carry the body past its budget is refused before its
	// bytes are read, not after.
	if b.served+size > b.lim.MaxBodySize {
		return 0, ErrBodyTooLarge
	}
	return size, nil
}

// expectCRLF consumes exactly the two bytes that must close chunk data.
func (b *BodyReader) expectCRLF() error {
	var buf [2]byte
	n, err := io.ReadFull(b.br, buf[:])
	b.consumed += int64(n)
	if err != nil {
		return io.ErrUnexpectedEOF
	}
	if buf[0] != '\r' || buf[1] != '\n' {
		return ErrBadChunk
	}
	return nil
}

// readTrailers reads the fields after the final chunk, up to the empty line.
func (b *BodyReader) readTrailers() error {
	limit := b.lim.MaxTrailerValueLen + maxTrailerNameLen
	var h http.Header
	for count := 0; ; count++ {
		line, crlf, err := b.readLine(limit)
		if err == errLineTooLong {
			return ErrValueTooLarge
		}
		if err != nil {
			return err
		}
		if !crlf {
			return ErrMalformed // a bare LF never terminates a field
		}
		if len(line) == 0 {
			b.trailers = h
			return nil
		}
		if count >= b.lim.MaxTrailerCount {
			return ErrTooManyTrailers
		}
		i := indexByteIn(line, ':')
		if i <= 0 {
			return ErrMalformed
		}
		name, val := line[:i], line[i+1:]
		if !validToken(name) {
			return ErrMalformed
		}
		for len(val) > 0 && (val[0] == ' ' || val[0] == '\t') {
			val = val[1:]
		}
		for len(val) > 0 && (val[len(val)-1] == ' ' || val[len(val)-1] == '\t') {
			val = val[:len(val)-1]
		}
		if len(val) > b.lim.MaxTrailerValueLen {
			return ErrValueTooLarge
		}
		if !validFieldValue(val) {
			return ErrMalformed
		}
		if h == nil {
			h = make(http.Header, 4)
		}
		h.Add(string(name), string(val))
	}
}

// readLine reads through the next LF, holding at most limit bytes. It returns
// the line without its terminator and whether that terminator was CRLF. A
// stream that ends without one is a truncation, never a line.
func (b *BodyReader) readLine(limit int) ([]byte, bool, error) {
	b.line = b.line[:0]
	for {
		frag, err := b.br.ReadSlice('\n')
		b.consumed += int64(len(frag))
		if len(b.line)+len(frag) > limit+2 { // +2 leaves room for the CRLF
			return nil, false, errLineTooLong
		}
		b.line = append(b.line, frag...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, false, io.ErrUnexpectedEOF
		}
		break
	}
	line := b.line[:len(b.line)-1] // drop the LF
	if len(line) > 0 && line[len(line)-1] == '\r' {
		return line[:len(line)-1], true, nil
	}
	return line, false, nil
}

// parseHexInt64 accepts one or more hex digits and nothing else. Leading zeros
// are fine (Go accepts "05"); a value that would overflow int64 is rejected
// rather than wrapped, because a wrapped length is a body boundary in the
// wrong place.
func parseHexInt64(v []byte) (int64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var n int64
	for _, c := range v {
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int64(c-'A') + 10
		default:
			return 0, false
		}
		if n > (1<<63-1-d)/16 {
			return 0, false
		}
		n = n*16 + d
	}
	return n, true
}

func indexByteIn(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// validToken reports whether b is an RFC 9110 token: the field-name grammar.
func validToken(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c >= 0x80 || !tokenChar[c] {
			return false
		}
	}
	return true
}

// validFieldValue rejects control bytes, which are how a trailer smuggles a
// header boundary into whatever reads the trailers next. HTAB is legal.
func validFieldValue(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

var tokenChar = func() (t [128]bool) {
	for _, c := range []byte("!#$%&'*+-.^_`|~") {
		t[c] = true
	}
	for c := byte('0'); c <= '9'; c++ {
		t[c] = true
	}
	for c := byte('a'); c <= 'z'; c++ {
		t[c] = true
	}
	for c := byte('A'); c <= 'Z'; c++ {
		t[c] = true
	}
	return t
}()

// Trailers returns the fields that followed the final chunk. It is meaningful
// only once Read has returned io.EOF; before that the section has not arrived
// and the map is empty.
func (b *BodyReader) Trailers() http.Header { return b.trailers }
