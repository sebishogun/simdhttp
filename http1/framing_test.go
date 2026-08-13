package http1

import "testing"

// One row per line of docs/lld/http1-body-framing.md section 4. The verdicts
// marked deviation carry the probed Go behaviour in the message, so a future
// reader can tell a deliberate difference from a bug.
func TestFramingTable(t *testing.T) {
	b := func(ss ...string) [][]byte {
		if len(ss) == 0 {
			return nil
		}
		out := make([][]byte, len(ss))
		for i, s := range ss {
			out[i] = []byte(s)
		}
		return out
	}
	for _, c := range []struct {
		name string
		cl   [][]byte
		te   [][]byte
		want error
		kind Kind
		n    int64
		note string
	}{
		{"no framing", nil, nil, nil, KindNone, 0, "no body"},
		{"plain CL", b("5"), nil, nil, KindFixed, 5, ""},
		{"zero CL", b("0"), nil, nil, KindFixed, 0, ""},
		{"CL+TE", b("5"), b("chunked"), ErrAmbiguousFraming, KindNone, 0,
			"D7: Go deletes CL and frames chunked (probed: server 200)"},
		{"CL.CL equal", b("5", "5"), nil, ErrAmbiguousFraming, KindNone, 0,
			"D6: Go dedupes identical values"},
		{"CL.CL differing", b("5", "6"), nil, ErrAmbiguousFraming, KindNone, 0, "parity"},
		{"empty CL", b(""), nil, ErrBadContentLength, KindNone, 0, "parity: invalid empty Content-Length"},
		{"CL with sign", b("+5"), nil, ErrBadContentLength, KindNone, 0, "digits only"},
		{"CL with space", b("5 "), nil, ErrBadContentLength, KindNone, 0, "digits only"},
		{"CL non-numeric", b("five"), nil, ErrBadContentLength, KindNone, 0, ""},
		{"CL negative", b("-1"), nil, ErrBadContentLength, KindNone, 0, ""},
		{"CL overflow", b("99999999999999999999"), nil, ErrBadContentLength, KindNone, 0, "int64 overflow is a framing error"},
		{"TE chunked", nil, b("chunked"), nil, KindChunked, 0, "parity"},
		{"TE chunked cased", nil, b("ChUnKeD"), nil, KindChunked, 0, "token is case-insensitive"},
		{"TE gzip", nil, b("gzip"), ErrBadTransferEncoding, KindNone, 0, "parity: unsupported"},
		{"TE identity", nil, b("identity"), ErrBadTransferEncoding, KindNone, 0, "parity: unsupported"},
		{"TE list", nil, b("gzip, chunked"), ErrBadTransferEncoding, KindNone, 0, "parity: one field, exactly chunked"},
		{"TE empty", nil, b(""), ErrBadTransferEncoding, KindNone, 0, "parity"},
		{"TE twice", nil, b("chunked", "chunked"), ErrBadTransferEncoding, KindNone, 0, "parity: too many transfer encodings"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range []Profile{Compatible, Strict} {
				f, err := FramingOf(c.cl, c.te, p)
				if err != c.want {
					t.Fatalf("profile %d: err %v, want %v [%s]", p, err, c.want, c.note)
				}
				if err != nil {
					continue
				}
				if f.Kind != c.kind || f.Length != c.n {
					t.Fatalf("profile %d: %+v, want kind %v length %d", p, f, c.kind, c.n)
				}
			}
		})
	}
}

// The head parser and the table must never disagree: whatever Parse accepts,
// Framing must be able to judge from the views Parse filled.
func TestFramingAgreesWithParse(t *testing.T) {
	for _, head := range []string{
		"POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: h\r\n\r\n",
	} {
		var req Request
		if _, err := Parse(&req, []byte(head), Compatible); err != nil {
			continue // rejected at head level; the table never sees it
		}
		if _, err := FramingOf(req.ContentLengthLines, req.TransferEncodingLines, Compatible); err != nil {
			// CL+TE is the one accepted head with a rejecting verdict (D7).
			if err != ErrAmbiguousFraming {
				t.Fatalf("%q: unexpected framing verdict %v", head, err)
			}
		}
	}
}
