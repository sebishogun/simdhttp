package http1

import "errors"

// The body-framing decision table of docs/lld/http1-body-framing.md section 4,
// in one place. Two implementations of this policy is exactly the condition a
// smuggled request needs -- one component frames the body one way, the next
// frames it another, and the bytes between the two readings are a second
// request nobody inspected. So this is the only function that decides, and the
// head parser shares its token check (ValidTransferEncoding) rather than
// keeping a second opinion.

// Kind is how a body is delimited.
type Kind uint8

const (
	KindNone    Kind = iota // no body
	KindFixed               // exactly Length bytes
	KindChunked             // chunked transfer coding
)

func (k Kind) String() string {
	switch k {
	case KindFixed:
		return "fixed"
	case KindChunked:
		return "chunked"
	}
	return "none"
}

// Framing describes how to read the body that follows a head.
type Framing struct {
	Kind   Kind
	Length int64 // valid when Kind == KindFixed
}

var (
	// ErrAmbiguousFraming means the head declares its body length more than
	// once, or in two ways at once: the request has no single reading.
	ErrAmbiguousFraming = errors.New("simdhttp: ambiguous body framing")
	// ErrBadContentLength means the Content-Length value is not a plain
	// non-negative int64: no sign, no space, digits only.
	ErrBadContentLength = errors.New("simdhttp: malformed Content-Length")
	// ErrBadTransferEncoding means the Transfer-Encoding is not a single
	// exact "chunked".
	ErrBadTransferEncoding = errors.New("simdhttp: unsupported transfer encoding")
)

// FramingOf decides how to read the body from a head's raw framing views --
// the occurrence lists the parser filled, never a re-parsed second opinion.
//
// The profile is accepted for symmetry with the parser and because the
// production limits differ by profile; the verdicts themselves are identical
// in both, which is deliberate: a request that frames one way for a lenient
// deployment and another way for a strict one is the ambiguity this table
// exists to remove.
func FramingOf(cl [][]byte, te [][]byte, profile Profile) (Framing, error) {
	_ = profile

	// Transfer-Encoding wins the shape of the body when present, so it is
	// judged first -- but its presence alongside Content-Length is the
	// ambiguity, not a precedence question (D7: Go deletes CL and frames
	// chunked; this rejects, because the deleted header is exactly what the
	// next hop may still read).
	switch {
	case len(te) > 1:
		return Framing{}, ErrBadTransferEncoding
	case len(te) == 1:
		if !ValidTransferEncoding(te[0]) {
			return Framing{}, ErrBadTransferEncoding
		}
		if len(cl) > 0 {
			return Framing{}, ErrAmbiguousFraming
		}
		return Framing{Kind: KindChunked}, nil
	}

	switch len(cl) {
	case 0:
		return Framing{Kind: KindNone}, nil
	case 1:
		n, ok := parseContentLength(cl[0])
		if !ok {
			return Framing{}, ErrBadContentLength
		}
		return Framing{Kind: KindFixed, Length: n}, nil
	default:
		// Any duplicate, equal or not (D6: Go dedupes equal values).
		return Framing{}, ErrAmbiguousFraming
	}
}

// parseContentLength accepts a plain non-negative decimal int64 and nothing
// else: no sign, no whitespace, no leading plus, and no value that overflows.
// Every one of those spellings is a place two parsers could disagree.
func parseContentLength(v []byte) (int64, bool) {
	if len(v) == 0 || len(v) > 19 { // 19 digits is the int64 ceiling's width
		return 0, false
	}
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int64(c - '0')
		if n > (1<<63-1-d)/10 {
			return 0, false // overflow is a framing error, never a wrap
		}
		n = n*10 + d
	}
	return n, true
}
