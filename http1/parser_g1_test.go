package http1

import (
	"strings"
	"testing"
)

// G1 (docs/wrong.md section 2): the kernel control scan stops at a tab, so a
// control byte after the first HTAB in a value >= ctlScanThreshold is
// never seen. This must be rejected. Fails on the copied parser.
func TestLongValueControlAfterTab(t *testing.T) {
	cases := []int{64, 65, 1024} // value lengths around the threshold
	for _, n := range cases {
		head := "GET / HTTP/1.1\r\nX-Long: " + strings.Repeat("v", n) + "\t" +
			strings.Repeat("w", 8) + "\x00" + strings.Repeat("x", 8) + "\r\n\r\n"
		var req Request
		if _, err := Parse(&req, []byte(head), Compatible); err == nil {
			t.Fatalf("len %d: control byte after HTAB accepted", n)
		}
	}
}
