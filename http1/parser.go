// Package simdhttp parses HTTP/1.1 request heads with vector scans.
//
// The shape is the one simdjson proved on JSON: classify bytes a register
// at a time, then walk the classification instead of the bytes. An HTTP
// head is even simpler than JSON -- lines end at CRLF, a header name ends
// at the colon, and the whole head ends at CRLFCRLF -- so the scan is
// IndexByte and IndexAnyOrLess over the shipped kernels, and the walk
// touches each boundary once.
//
// The contract: anything net/http's reader rejects, this rejects, and
// accepted requests carry byte-identical fields with OWS trimmed. In one
// place this is deliberately stricter than net/textproto: a field name
// must be an RFC 9110 token, so a space inside a name -- which the
// standard reader tolerates and request-smuggling attacks lean on -- is
// malformed here, and so is a bare LF line ending, which the standard
// reader also accepts and which is the other half of the same class of
// attack. Both require CRLF.
package http1

import (
	"bytes"
	"errors"

	"github.com/sebishogun/simd"
)

// ctlScanThreshold is the value length above which scanning for control
// bytes goes through the simd kernel rather than an inline loop. Below it
// the kernel's call boundary costs more than the scan, as the shape sweep
// showed on many-short-header heads.
const ctlScanThreshold = 64

// Request is a parsed request head. All fields alias the input buffer;
// the caller owns the bytes and their lifetime.
type Request struct {
	Method  []byte
	Target  []byte
	Proto   []byte
	Headers []Header

	// ContentLengthLines and TransferEncodingLines are the raw occurrences
	// of those framing headers, borrowed from the input during the walk --
	// the surface the framing decision table (Phase 1) reads. Parse rejects
	// a second Content-Length, so ContentLengthLines holds at most one entry
	// and the framing layer keeps the single parsing opinion.
	ContentLengthLines    [][]byte
	TransferEncodingLines [][]byte

	// lineEnds is scratch for the one-pass boundary scan, reused across
	// Parse calls on the same Request.
	lineEnds []int32
}

// Header is one name: value pair, aliasing the input.
type Header struct {
	Name  []byte // as written, not canonicalized; see CanonicalName
	Value []byte // OWS-trimmed
}

var (
	// ErrIncomplete means the head has no terminating blank line yet:
	// read more and call again.
	ErrIncomplete = errors.New("simdhttp: incomplete head")
	// ErrMalformed means the bytes cannot be an HTTP/1.x head.
	ErrMalformed = errors.New("simdhttp: malformed head")
	// ErrMissingHost means an HTTP/1.1 head had no Host, or an empty one.
	ErrMissingHost = errors.New("simdhttp: missing or empty Host header")
)

// Profile selects the verdict set. Compatible mirrors net/http's reader;
// Strict is the documented superset of rejections.
type Profile uint8

const (
	Compatible Profile = iota // net/http-compatible verdicts
	Strict                    // documented superset
)

// Parse parses one request head from b and returns it with the number of
// bytes consumed, including the terminating blank line. Headers is
// appended into req.Headers, which the caller may pre-size and reuse.
func Parse(req *Request, b []byte, profile Profile) (consumed int, err error) {
	_ = profile // threaded through; consulted from Task 4 on
	// The request line: METHOD SP TARGET SP PROTO CRLF.
	nl := simd.IndexByte(b, '\n')
	if nl < 0 {
		return 0, ErrIncomplete
	}
	line := b[:nl]
	if len(line) == 0 || line[len(line)-1] != '\r' {
		return 0, ErrMalformed
	}
	line = line[:len(line)-1]
	sp1 := simd.IndexByte(line, ' ')
	if sp1 <= 0 {
		return 0, ErrMalformed
	}
	sp2 := simd.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return 0, ErrMalformed
	}
	sp2 += sp1 + 1
	req.Method = line[:sp1]
	req.Target = line[sp1+1 : sp2]
	req.Proto = line[sp2+1:]
	if len(req.Target) == 0 {
		return 0, ErrMalformed
	}
	// The request-target carries no control bytes (a tab included -- unlike a
	// header value, a tab in a target is malformed) and every percent-escape
	// is two hex digits. net/url rejects both ("invalid control character in
	// URL", "invalid URL escape"), so this is parity, not added strictness.
	if !validTarget(req.Target) {
		return 0, ErrMalformed
	}
	// The version is HTTP/1.0 or HTTP/1.1 -- net/http rejects anything
	// else, and a request line whose third field is not a version is one
	// of the ways a smuggled request hides.
	if !isHTTPVersion(req.Proto) {
		return 0, ErrMalformed
	}
	// The method is a short token; validated inline, no dispatch.
	if !tokenOnly(req.Method) {
		return 0, ErrMalformed
	}

	req.Headers = req.Headers[:0]
	req.ContentLengthLines = req.ContentLengthLines[:0]
	req.TransferEncodingLines = req.TransferEncodingLines[:0]
	hostCount := 0
	hostNonEmpty := false
	clSeen := false
	block := b[nl+1:]
	// One pass finds every line end in the header block; the walk below
	// splits on the index instead of scanning for each '\n' separately,
	// which is what kept a hundred-header head linear in the scan rather
	// than one IndexByte call per line. lineEnds is sized to the block --
	// a head cannot have more line ends than bytes -- and reused across
	// calls through the request.
	if cap(req.lineEnds) < len(block)/2+1 {
		req.lineEnds = make([]int32, len(block)/2+1)
	}
	ends := req.lineEnds[:cap(req.lineEnds)]
	ne := simd.IndexAll(ends, block, '\n')
	start := 0
	for k := 0; k < ne; k++ {
		lineEnd := int(ends[k])
		line := block[start:lineEnd]
		if len(line) == 0 || line[len(line)-1] != '\r' {
			return 0, ErrMalformed
		}
		line = line[:len(line)-1]
		consumedTo := nl + 1 + lineEnd + 1
		start = lineEnd + 1
		if len(line) == 0 {
			// Head complete. HTTP/1.1 requires a non-empty, well-formed Host;
			// 1.0 may omit it. Strict rejects a missing/empty Host as
			// malformed; Compatible answers ErrMissingHost so a caller can
			// tell that framing case apart. Go's server accepts a
			// present-but-empty Host: (D5) -- both profiles here count empty
			// as missing.
			if req.Proto[7] == '1' && !hostNonEmpty {
				if profile == Strict {
					return 0, ErrMalformed
				}
				return 0, ErrMissingHost
			}
			return consumedTo, nil
		}
		// bytes.IndexByte is a compiler intrinsic -- inlined, no dispatch
		// -- which beats a kernel call on a short header line, the shape
		// the sweep showed dominating a hundred-header head. The big
		// linear scan (every line end) already happened in one IndexAll
		// pass above; here each line is tens of bytes.
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return 0, ErrMalformed
		}
		name := line[:colon]
		if !tokenOnly(name) {
			return 0, ErrMalformed
		}
		// One Host, exactly -- net/http's reader answers "too many Host
		// headers" to a second one, and a parser in front of an origin that
		// accepts two is the request-smuggling seam the differential fuzz
		// found (docs/wrong.md, the G2 record; the corpus seed pins it).
		isHost := bytes.EqualFold(name, hostHeader)
		if isHost {
			hostCount++
			if hostCount > 1 {
				return 0, ErrMalformed // duplicate Host, even identical/empty (parity)
			}
		}
		val := line[colon+1:]
		for len(val) > 0 && (val[0] == ' ' || val[0] == '\t') {
			val = val[1:]
		}
		for len(val) > 0 && (val[len(val)-1] == ' ' || val[len(val)-1] == '\t') {
			val = val[:len(val)-1]
		}
		// A long value is worth the kernel; a short one is scanned inline.
		// Both reject a control byte other than HTAB. The kernel reports
		// the FIRST hit, and a tab is a legal hit -- so the scan continues
		// past each tab rather than stopping at it, which is G1: a control
		// byte hiding behind the first tab of a long value was accepted.
		// The no-tab common case still costs exactly one kernel call.
		if len(val) >= ctlScanThreshold {
			pos := 0
			for {
				i := simd.IndexAnyOrLess(val[pos:], "\x7f", 0x20)
				if i < 0 {
					break
				}
				if val[pos+i] != '\t' {
					return 0, ErrMalformed
				}
				pos += i + 1 // a tab is legal; keep scanning past it
			}
		} else {
			for _, c := range val {
				if c < 0x20 && c != '\t' || c == 0x7f {
					return 0, ErrMalformed
				}
			}
		}
		if isHost && len(val) > 0 {
			if !validHost(val) {
				return 0, ErrMalformed // D9: stricter than httpguts
			}
			hostNonEmpty = true // present-but-empty counts as missing (D5)
		}
		if bytes.EqualFold(name, clHeader) {
			// A second Content-Length is rejected in both profiles. Go dedupes
			// identical values and rejects only differing ones (D6); this is
			// stricter on purpose -- two CL lines are a framing ambiguity.
			if clSeen {
				return 0, ErrMalformed
			}
			clSeen = true
			req.ContentLengthLines = append(req.ContentLengthLines, val)
		} else if bytes.EqualFold(name, teHeader) {
			req.TransferEncodingLines = append(req.TransferEncodingLines, val)
		}
		req.Headers = append(req.Headers, Header{Name: name, Value: val})
	}
	// Ran out of line ends before the blank line: the head is incomplete.
	return 0, ErrIncomplete
}

// clHeader and teHeader name the framing headers, []byte of constants.
var (
	clHeader = []byte("Content-Length")
	teHeader = []byte("Transfer-Encoding")
)

// validTarget rejects a control byte (tab included) and a percent-escape
// that is not two hex digits.
func validTarget(t []byte) bool {
	if i := simd.IndexAnyOrLess(t, "\x7f", 0x20); i >= 0 {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] == '%' {
			if i+2 >= len(t) || !isHex(t[i+1]) || !isHex(t[i+2]) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// validHost rejects the authority forms net/http's httpguts misses: a
// space/tab/control/comma anywhere, and unbalanced brackets (D9). Stricter
// than ValidHostHeader on purpose -- probed on Go 1.26.5, its table accepts
// a.com,b.com and unbalanced [::1.
func validHost(h []byte) bool {
	depth := 0
	for _, c := range h {
		switch {
		case c <= 0x20 || c == 0x7f || c == ',':
			return false
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// hostHeader is the Host field name for the duplicate check; a package
// []byte of a constant, so EqualFold takes no per-call conversion.
var hostHeader = []byte("Host")

// tokenSet is RFC 9110's token alphabet.
const tokenSet = "!#$%&'*+-.^_`|~0123456789" +
	"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// isHTTPVersion reports whether p is exactly "HTTP/1.0" or "HTTP/1.1".
// This parser handles the /1.x line grammar; HTTP/2 and /3 are framed,
// not text, and belong to a different reader.
func isHTTPVersion(p []byte) bool {
	return len(p) == 8 && string(p[:7]) == "HTTP/1." && (p[7] == '0' || p[7] == '1')
}

// tokenBytes marks the RFC 9110 token alphabet for inline validation --
// a 256-entry lookup, no dispatch, which beats a kernel scan on the short
// names and methods that dominate a real request head.
var tokenBytes = func() [256]bool {
	var t [256]bool
	for _, c := range []byte(tokenSet) {
		t[c] = true
	}
	return t
}()

func tokenOnly(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !tokenBytes[c] {
			return false
		}
	}
	return true
}
