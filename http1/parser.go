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
			return consumedTo, nil // the blank line: head complete
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
		val := line[colon+1:]
		for len(val) > 0 && (val[0] == ' ' || val[0] == '\t') {
			val = val[1:]
		}
		for len(val) > 0 && (val[len(val)-1] == ' ' || val[len(val)-1] == '\t') {
			val = val[:len(val)-1]
		}
		// A long value is worth the kernel; a short one is scanned inline.
		// Both reject a control byte other than HTAB.
		if len(val) >= ctlScanThreshold {
			if i := simd.IndexAnyOrLess(val, "\x7f", 0x20); i >= 0 && val[i] != '\t' {
				return 0, ErrMalformed
			}
		} else {
			for _, c := range val {
				if c < 0x20 && c != '\t' || c == 0x7f {
					return 0, ErrMalformed
				}
			}
		}
		req.Headers = append(req.Headers, Header{Name: name, Value: val})
	}
	// Ran out of line ends before the blank line: the head is incomplete.
	return 0, ErrIncomplete
}

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
