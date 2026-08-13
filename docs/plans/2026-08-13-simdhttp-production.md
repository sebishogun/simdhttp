# simdhttp Production Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.
>
> **For the executor:** this plan is future work. It is NOT to be executed
> on the `docs/v120-documentation` branch (docs-only). Execute it in a
> code-enabled worktree of the main repository, and check each task's
> gates in `docs/verification.md` before its commit.

**Goal:** Ship the approved production library: a hardened `simdhttp/http1`
head parser and streaming body reader, then a root net/http-native
low-allocation router with helpers, middleware, and an error adapter —
with the safety work (the current gaps G1–G6 of `docs/architecture.md`
§2, whose records and the benchmark/gate findings live in
`docs/wrong.md`) staged first.

**Architecture:** Phase 0 forks the shipped parser into `simdhttp/http1`
with profiles, limits, and the verified safety fixes (tests written
first; the G1 regression test fails on the copied parser). Phase 1 adds
the framing decision table and streaming `BodyReader`. Phase 2 adds the
root `Router` with immutable `Build`, params via `SetPathValue`, 405 /
HEAD / OPTIONS / trailing-slash policy, and a differential gate against
`net/http.ServeMux`. Phase 3 adds the error adapter, simdjson JSON
helpers, query/form helpers, and middleware. Phase 4 audits seams,
reworks the Makefile gates (including the `bench-check` pipefail flaw),
and updates docs to shipped reality. Hot loops stay concrete: no
interfaces, no indirect calls; seams only for codec/error/observability/
future server, each passing disassembly + `perf stat` gates.

**Tech Stack:** Go 1.26.5 toolchain (the oracle for every verdict in
this plan; go.mod keeps its `go 1.26.2` floor), `github.com/sebishogun/simd`
v1.20.0, `github.com/sebishogun/simdjson`, net/http (`Handler`,
`ResponseWriter`, `PathValue`/`SetPathValue`), `httptest`, `perf stat`,
`go tool objdump`.

---

## Phase 0 — `simdhttp/http1`: hardened head parser

### Task 1: Create the `http1` package with the parser copied verbatim

**Files:**
- Create: `http1/parser.go` (copy of root `parser.go` with the package
  renamed to `http1` and `Profile` added to the signature)
- Create: `http1/parser_test.go`
- Create: `http1/parser_g1_test.go` (the failing regression test)

**Step 1: Write the failing G1 regression test**

```go
// http1/parser_g1_test.go
package http1

import "testing"

// G1 (docs/wrong.md §2): the kernel control scan stops at a tab, so a
// control byte after the first HTAB in a value >= ctlScanThreshold is
// never seen. This must be rejected. Fails on the copied parser.
func TestLongValueControlAfterTab(t *testing.T) {
	cases := []int{64, 65, 1024} // value lengths around the threshold
	for _, n := range cases {
		head := "GET / HTTP/1.1\r\nX-Long: " + repeat('v', n) + "\t" +
			repeat('w', 8) + "\x00" + repeat('x', 8) + "\r\n\r\n"
		var req Request
		if _, err := Parse(&req, []byte(head), Compatible); err == nil {
			t.Fatalf("len %d: control byte after HTAB accepted", n)
		}
	}
}
```

**Step 2: Run it to verify it fails**

Run: `go test ./http1/ -run TestLongValueControlAfterTab`
Expected: FAIL (accepted — this is the point of the task)

**Step 3: Create the package skeleton**

Copy `parser.go` to `http1/parser.go`, renaming the package, and add:

```go
type Profile uint8

const (
	Compatible Profile = iota // net/http-compatible verdicts
	Strict                    // documented superset
)

func Parse(req *Request, b []byte, profile Profile) (consumed int, err error)
```

Copy the parser **verbatim** — the shipped control scan (hit-and-stop
on the first `IndexAnyOrLess` hit) stays exactly as it is, so the
regression test exercises the real bug before Task 2 fixes it. No
behavioral edits in this task; `Profile` is threaded through but not
yet consulted.

**Step 4: Run it to verify it passes**

Run: `go test ./http1/`
Expected: all copied tests pass, `TestLongValueControlAfterTab` FAILs.

**Step 5: Commit**

```bash
git add http1/
git commit -m "http1: fork the head parser with a profile parameter"
```

### Task 2: Fix G1 — the control scan continues past HTAB

**Files:**
- Modify: `http1/parser.go` (control scan)

**Step 1: Write the fix**

```go
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
```

**Step 2: Run tests**

Run: `go test ./http1/ -run TestLongValueControlAfterTab`
Expected: PASS

**Step 3: Verify the common case still costs one kernel call**

Run: `go test -c -o /tmp/http1.test ./http1/`, then `go tool objdump -s 'http1\.Parse' /tmp/http1.test > /tmp/http1.obj` and read the disassembly — the no-tab path must make one `IndexAnyOrLess` call; see `docs/verification.md` §6. (No gate is ever judged through a pipe.)

**Step 4: Run the full suite**

Run: `go test ./http1/`
Expected: PASS

**Step 5: Commit**

```bash
git add http1/
git commit -m "http1: close the tab-stops-the-control-scan hole"
```

### Task 3: Reject duplicate Host

**Files:**
- Modify: `http1/parser.go`
- Modify: `http1/parser_test.go`
- Add: `testdata/fuzz/FuzzParseAgainstNetHTTP/4cb4ee00bf74f878` (the
  duplicate-Host fuzz regression seed, committed in Step 3)

**Step 1: Write the failing test**

```go
func TestDuplicateHostRejected(t *testing.T) {
	for _, head := range []string{
		"GET / HTTP/1.1\r\nHost: a.com\r\nHost: a.com\r\n\r\n", // identical too
		"GET / HTTP/1.1\r\nHost: a.com\r\nHost: b.com\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a.com\r\nHOST:\r\n\r\n",
	} {
		var req Request
		if _, err := Parse(&req, []byte(head), Compatible); err == nil {
			t.Fatalf("%q: duplicate Host accepted", head)
		}
	}
}
```

**Step 2: Run it to verify it fails**

Run: `go test ./http1/ -run TestDuplicateHostRejected`
Expected: FAIL

**Step 3: Pin the fuzz-found input as a regression corpus seed**

Commit the input the differential fuzz already found — the documented
G2 case (`docs/wrong.md` §3): `0 * HTTP/1.0\r\nHost:\r\nHost:\r\n\r\n`,
accepted by simdhttp, rejected by net/http ("too many Host headers").
A fresh clone must pin the red case before and with the fix, not carry
it as a local artifact:

- Add the Go fuzz corpus file
  `testdata/fuzz/FuzzParseAgainstNetHTTP/4cb4ee00bf74f878` — the
  `go test fuzz v1` file whose name is the first 16 hex digits of the
  SHA-256 of its own content (Go 1.26.5 corpus naming) and whose
  payload is `[]byte("0 * HTTP/1.0\r\nHost:\r\nHost:\r\n\r\n")`. The
  file lands in the worktree when a fuzz run reaches the failure
  (`go test -fuzz=FuzzParseAgainstNetHTTP`); commit it as-is.
- Do **not** add an `f.Add` seed in `fuzz_test.go` source — the corpus
  file is the seed; source seeds are added in Task 8's corpus step.
- With the seed committed, `go test ./http1/` (and the fuzz smoke)
  fails on the un-fixed parser — the seed acts as a regression test
  once Go finds it in `testdata/fuzz/` — and passes after the fix.
  The fix in Step 4 turns it green; the seed and the fix ship in the
  same commit so the clone is never green before the fix.

**Step 4: Implement**

In the header loop, track the first Host occurrence with
`bytes.EqualFold` over a precomputed constant slice — no string
conversion, no allocation:

```go
var hostHeader = []byte("Host") // package-level; []byte of a constant
// inside the loop, after the name check:
var hostSeen bool
if bytes.EqualFold(name, hostHeader) {
	if hostSeen {
		return 0, ErrMalformed
	}
	hostSeen = true
}
```

**Step 5: Run tests**

Run: `go test ./http1/`
Expected: PASS (the seed in Step 3 was red before the fix, green now)

**Step 6: Commit**

```bash
git add http1/ testdata/fuzz/
git commit -m "http1: reject duplicate Host headers (parity); pin fuzz regression seed"
```

### Task 4: Host presence and format validation

**Files:**
- Modify: `http1/parser.go`
- Modify: `http1/parser_test.go`

**Step 1: Write the failing tests**

```go
func TestMissingHost(t *testing.T) {
	// HTTP/1.1 requires Host (net/http server: "missing required Host header").
	var req Request
	if _, err := Parse(&req, []byte("GET / HTTP/1.1\r\n\r\n"), Strict); err == nil {
		t.Fatal("strict: missing Host accepted")
	}
	if _, err := Parse(&req, []byte("GET / HTTP/1.1\r\nHost:\r\n\r\n"), Compatible); err != ErrMissingHost {
		t.Fatalf("compatible: want ErrMissingHost, got %v", err)
	}
	// HTTP/1.0 may omit Host.
	if _, err := Parse(&req, []byte("GET / HTTP/1.0\r\n\r\n"), Compatible); err != nil {
		t.Fatalf("1.0 without Host: %v", err)
	}
}

func TestMalformedHost(t *testing.T) {
	for _, h := range []string{"bad host", "a b", "a\x00b", "a,b"} {
		var req Request
		if _, err := Parse(&req, []byte("GET / HTTP/1.1\r\nHost: "+h+"\r\n\r\n"), Compatible); err == nil {
			t.Fatalf("Host %q accepted", h)
		}
	}
}
```

**Step 2: Run to verify they fail**

Run: `go test ./http1/ -run 'TestMissingHost|TestMalformedHost'`
Expected: FAIL

**Step 3: Implement**

Add `ErrMissingHost = errors.New("simdhttp: missing or empty Host header")`.
After the header loop, before the blank-line check: if the request line
is HTTP/1.1, apply the profile policy (strict: reject; compatible:
return `ErrMissingHost`; both count the empty value as missing —
deviation D5: Go's server accepts a present-but-empty `Host:`). Add
the simdhttp host-format check (inline scan, no `net/http` import):
no space, tab, control, or comma in the authority; `[` requires a
matching `]` and vice versa. This rule is deliberately stricter than
Go's `httpguts.ValidHostHeader` (a byte-table scan that allows comma
and has no bracket-balance logic — probed on Go 1.26.5: the server
accepts `Host: a.com,b.com` and unbalanced `Host: [::1`). The tests
assert the simdhttp rule's cases and pin the comma and bracket rows
as deviation D9 with the probed Go verdicts, never as
`ValidHostHeader` parity.

**Step 4: Run tests**

Run: `go test ./http1/`
Expected: PASS

**Step 5: Commit**

```bash
git add http1/
git commit -m "http1: validate Host presence and format per profile"
```

### Task 5: Reject control bytes and invalid escapes in the request-target

**Files:**
- Modify: `http1/parser.go`
- Modify: `http1/parser_test.go`

**Step 1: Write the failing test**

```go
func TestTargetControls(t *testing.T) {
	for _, tgt := range []string{"/a\x00b", "/a\x7fb", "/a\x1fb", "/%zz", "/a%2"} {
		var req Request
		if _, err := Parse(&req, []byte("GET "+tgt+" HTTP/1.1\r\nHost: x\r\n\r\n"), Compatible); err == nil {
			t.Fatalf("target %q accepted", tgt)
		}
	}
}
```

**Step 2: Run to verify it fails** — `go test ./http1/ -run TestTargetControls` FAIL.

**Step 3: Implement** — one control scan over `Target` (same kernel,
no tab allowance: a tab in a target is rejected) plus a percent-escape
check: every `%` must be followed by two hex digits. Both profiles
reject: net/http rejects these targets too (net/url: "invalid control
character in URL", "invalid URL escape"), so this is parity, not
strictness (deviation D8 records it as target parity/hardening).

**Step 4: Run tests** — `go test ./http1/` PASS.

**Step 5: Commit**

```bash
git add http1/
git commit -m "http1: reject control bytes and invalid escapes in the request-target"
```

### Task 6: Reject duplicate Content-Length; expose framing fields

**Files:**
- Modify: `http1/parser.go`
- Modify: `http1/parser_test.go`

**Step 1: Write the failing test**

```go
func TestDuplicateContentLength(t *testing.T) {
	for _, head := range []string{
		"POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n",
	} {
		var req Request
		if _, err := Parse(&req, []byte(head), Compatible); err == nil {
			t.Fatalf("%q: duplicate Content-Length accepted", head)
		}
	}
}
```

**Step 2: Run to verify it fails** — FAIL (accepted).

**Step 3: Implement**

- Reject a *second* `Content-Length` line in both profiles. This is an
  enumerated deviation (D6), not parity: Go's `ReadRequest` dedupes
  identical `Content-Length` values and only rejects differing ones
  (verified on Go 1.26.5). The identical-duplicate test above is the
  deviation's regression test.
- Add the raw occurrence views for the framing layer:
  `ContentLengthLines [][]byte` and `TransferEncodingLines [][]byte`
  (borrowed, filled during the walk; same names as the LLD's §3.1
  surface). No parsed `ContentLength int64` on `Request` — the
  parse-time duplicate rejection keeps `ContentLengthLines` at one
  entry, so the view is safe to expose and the framing table holds
  the single parsing opinion. The CL+TE verdict itself is the framing
  table's job (Task 10, deviation D7), per the LLD.

**Step 4: Run tests** — `go test ./http1/` PASS.

**Step 5: Commit**

```bash
git add http1/
git commit -m "http1: reject duplicate Content-Length, expose framing fields"
```

### Task 7: Limits with typed errors

**Files:**
- Modify: `http1/parser.go`
- Modify: `http1/parser_test.go`

**Step 1: Write the failing tests**

```go
func TestLimits(t *testing.T) {
	var req Request
	// One request line, then 60 valid header lines (> the strict 50):
	// the count check must fire with the head still small.
	_, err := Parse(&req, []byte("GET / HTTP/1.1\r\n"+strings.Repeat("X-H: v\r\n", 60)+"\r\n"), Strict)
	if err != ErrTooManyHeaders {
		t.Fatalf("header-count limit: %v", err)
	}
	// 1<<20+1 exceeds the compatible 1 MiB value limit. Reachable because
	// MaxHeadSize (2 MiB compatible) exceeds MaxHeaderValueLen, so the
	// head-size check does not pre-empt it.
	_, err = Parse(&req, []byte("GET / HTTP/1.1\r\nX: "+strings.Repeat("v", 1<<20+1)+"\r\n\r\n"), Compatible)
	if err != ErrValueTooLarge {
		t.Fatalf("value limit: %v", err)
	}
	// One request line, then 60 valid 8 KiB headers (~500 KiB head, every
	// value under the strict 64 KiB value limit): the entry check fires
	// ErrHeadTooLarge before the count check.
	_, err = Parse(&req, []byte("GET / HTTP/1.1\r\n"+strings.Repeat("X: "+strings.Repeat("v", 1<<13)+"\r\n", 60)+"\r\n"), Strict)
	if err != ErrHeadTooLarge {
		t.Fatalf("head limit: %v", err)
	}
}
```

**Step 2: Run to verify they fail** — FAIL (no limits exist).

**Step 3: Implement**

```go
var (
	ErrHeadTooLarge   = errors.New("simdhttp: head exceeds limit")
	ErrTooManyHeaders = errors.New("simdhttp: too many headers")
	ErrValueTooLarge  = errors.New("simdhttp: header value exceeds limit")
)

// per-profile bounds (http1-head-parser.md §3.2).
// MaxHeadSize deliberately exceeds MaxHeaderValueLen so the value-limit
// error is reachable and not pre-empted by the head-size check.
type limits struct {
	headSize, requestLine, headerCount, valueLen int
}
```

Check `len(b)` before the scan, request-line length before splitting,
header count and value length inside the walk — one pass, no extra
allocation. A head past a limit returns its typed error, never
`ErrIncomplete` (the boundary tests pin this).

**Step 4: Run tests** — `go test ./http1/` PASS.

**Step 5: Commit**

```bash
git add http1/
git commit -m "http1: bounded head parsing with typed limit errors"
```

### Task 8: Extend the corpus, seeds, and gates

**Files:**
- Modify: `http1/parser_test.go`
- Modify: `http1/fuzz_test.go` (new)
- Modify: `README.md` (Phase 0 status + `http1` pointer)

**Step 1: Extend the differential corpus** with every case from
`docs/verification.md` §3, and the G1 seed (a ≥ 64-byte value with
`\t` + `\x00`) in the fuzz seed corpus:

```go
f.Add([]byte("GET / HTTP/1.1\r\nX: " + strings.Repeat("v", 70) + "\t\x00" + "\r\n\r\n"))
```

**Step 2: Run** `go test ./http1/` and a 15 s fuzz smoke
(`go test -fuzz=FuzzParseAgainstNetHTTP -fuzztime=15s ./http1/`).
Both must pass before this task's commit.

**Step 3: Update the README** — Phase 0 shipped: `http1.Parse`
supersedes the root `Parse` for new work; root `Parse` unchanged for
compatibility; gaps table now closed for G1–G6 in `http1`, still open
in the root package.

**Step 4: Commit**

```bash
git add http1/ README.md
git commit -m "http1: phase-0 corpus, fuzz seeds, README status"
```

---

## Phase 1 — framing and body reader

### Task 9: The framing decision table

**Files:**
- Create: `http1/framing.go`
- Create: `http1/framing_test.go`

**Step 1: Write the failing tests** — one test per row of the policy
table (`docs/lld/http1-body-framing.md` §4), e.g.:

```go
func TestFramingCLPlusTE(t *testing.T) {
	h := []byte("Content-Length: 5")
	if v := Framing(h, [][]byte{[]byte("chunked")}, Compatible); v != ErrAmbiguousFraming {
		t.Fatalf("CL+TE: %v", v) // deviation D7: Go's ReadRequest and server both delete CL and frame chunked (probed: 200)
	}
}

func TestFramingTEPolicy(t *testing.T) {
	// Exactly one TE field with exactly "chunked" — parity with Go's
	// single-encoding rule (verified: Go rejects "gzip, chunked" and
	// two chunked fields alike).
	if v := Framing(nil, [][]byte{[]byte("chunked")}, Compatible); v != nil {
		t.Fatalf("plain chunked: %v", v)
	}
	for _, te := range [][]byte{
		[]byte("gzip"),
		[]byte("gzip, chunked"),
		[]byte("identity"),
	} {
		if v := Framing(nil, [][]byte{te}, Compatible); v != ErrBadTransferEncoding {
			t.Fatalf("TE %q: %v", te, v)
		}
	}
	if v := Framing(nil, [][]byte{[]byte("chunked"), []byte("chunked")}, Compatible); v != ErrBadTransferEncoding {
		t.Fatalf("two TE fields: %v", v)
	}
}

func TestFramingDuplicateCL(t *testing.T) {
	// Deviation D6: Go dedupes identical values; simdhttp rejects any duplicate.
	if v := Framing([][]byte{[]byte("5"), []byte("5")}, nil, Compatible); v != ErrAmbiguousFraming {
		t.Fatalf("identical duplicate CL: %v", v)
	}
}
```

**Step 2: Run to verify they fail** — `go test ./http1/ -run TestFraming` FAIL (no function).

**Step 3: Implement** — `func Framing(cl [][]byte, te [][]byte, p Profile) error`:
single table-driven implementation of `docs/lld/http1-body-framing.md`
§4; `nil` means "no body framing error"; `ErrAmbiguousFraming`,
`ErrBadTransferEncoding`, `ErrBadContentLength` typed errors. Parse CL
to int64 (digits only, no `+`, no space).

**Step 4: Run tests** — `go test ./http1/` PASS.

**Step 5: Commit**

```bash
git add http1/framing.go http1/framing_test.go
git commit -m "http1: framing decision table (CL/TE ambiguity policy)"
```

### Task 10: `BodyReader` — fixed length

**Files:**
- Create: `http1/body.go`
- Create: `http1/body_test.go`

**Step 1: Write the failing test**

```go
func TestBodyFixedLength(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("helloNEXT"))
	b := NewBodyReader(br, FixedLength(5), DefaultLimits())
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
```

**Step 2: Run to verify it fails** — FAIL (no package symbol).

**Step 3: Implement** — `NewBodyReader(br *bufio.Reader, framing Framing, limits Limits) *BodyReader`; `Read` serves exactly `Content-Length` bytes; `io.EOF` at the declared length; bytes past the length stay on the connection; `Consumed()` counts from the reader. `Framing` is a small concrete struct (`FixedLength(n int64)` / `Chunked`), not an interface.

**Step 4: Run tests** — PASS.

**Step 5: Commit**

```bash
git add http1/body.go http1/body_test.go
git commit -m "http1: fixed-length body reader"
```

### Task 11: `BodyReader` — chunked and trailers

**Files:**
- Modify: `http1/body.go`
- Modify: `http1/body_test.go`

**Step 1: Write the failing test**

```go
func TestBodyChunkedTrailers(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(
		"5\r\nhello\r\n0\r\nX-Trailer: v\r\n\r\n"))
	b := NewBodyReader(br, Chunked, DefaultLimits())
	all, _ := io.ReadAll(b)
	if string(all) != "hello" {
		t.Fatalf("body %q", all)
	}
	if b.Trailers().Get("X-Trailer") != "v" {
		t.Fatalf("trailers %v", b.Trailers())
	}
}
```

**Step 2: Run to verify it fails** — FAIL.

**Step 3: Implement** — chunk-size line (hex, optional extension, CRLF),
chunk data + CRLF, zero chunk, trailer section (reuse the head parser's
line discipline and limits), final CRLF. Reject: control bytes in
trailers, chunk-size line past `MaxChunkSizeLine`, arithmetic overflow,
truncation (`io.ErrUnexpectedEOF`).

**Step 4: Run tests** — PASS, including the truncated-at-every-byte
corpus from `docs/verification.md` §3.

**Step 5: Commit**

```bash
git add http1/body.go http1/body_test.go
git commit -m "http1: chunked body reader with trailers"
```

### Task 12: Drain, limits, and the pipelining contract

**Files:**
- Modify: `http1/body.go`
- Modify: `http1/body_test.go`

**Step 1: Write the failing tests** — `Close` before EOF drains up to
`MaxDrainSize` and reports whether the drain completed; a body larger
than the drain budget aborts without hanging; pipelined bytes after a
drained body are intact for the next head parse.

**Step 2: Run to verify they fail** — FAIL.

**Step 3: Implement** — drain loop with the limit budget; `Consumed()`
delimits the request on the wire; `MaxBodySize` enforced as served
bytes; every error path leaves the reader drainable.

**Step 4: Run tests** — PASS, plus the Phase-1 fuzz targets
(`FuzzBodyReader`: no panic, no hang, no `(0, nil)`) over a 15 s
smoke.

**Step 5: Commit**

```bash
git add http1/body.go http1/body_test.go
git commit -m "http1: drain, limits, pipelining contract"
```

---

## Phase 2 — root router

### Task 13: `Router` with immutable build and segment trie

**Files:**
- Create: `router.go`
- Create: `router_test.go`

**Step 1: Write the failing tests**

```go
func TestRouteMatchParams(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("id=" + req.PathValue("id")))
	})
	if err := r.Build(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/users/42", nil))
	if rec.Body.String() != "id=42" {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestBuildConflict(t *testing.T) {
	r := New()
	r.HandleFunc("GET", "/a/{x}", h)
	r.HandleFunc("GET", "/a/{y}", h)
	if err := r.Build(); err == nil {
		t.Fatal("conflict not detected")
	}
}
```

**Step 2: Run to verify they fail** — FAIL (no symbols).

**Step 3: Implement** — registration copies patterns and never panics;
`Build` compiles the segment trie (`docs/lld/router.md` §6), detects
precedence ties as errors, freezes the router; `ServeHTTP` walks static
→ param → wildcard, resolves the method, allocates nothing on the
matched path, writes params via `SetPathValue`. Method resolution per
`docs/lld/router.md` §6: the nine common methods (`GET, HEAD, POST,
PUT, PATCH, DELETE, OPTIONS, CONNECT, TRACE`) get dense fixed IDs 0–8
resolved by an inline length-and-bytes compare chain; custom token
methods registered via `Handle` get IDs 9+ through an immutable
open-addressing table built at `Build` (bucket by first byte and
length, then full compare) — exact, case-sensitive, no interface
dispatch, no per-request allocation; the node `handlers` array is
sized `9 + custom methods` at `Build`. Add the explicit
`MustBuild() *Router` helper that panics on a `Build` error — the only
panic path for a bad table, for use in tests and top-level setup. No
interfaces inside.

**Step 4: Run tests** — PASS.

**Step 5: Commit**

```bash
git add router.go router_test.go
git commit -m "router: immutable build and segment-trie matching"
```

### Task 14: 405, HEAD, OPTIONS, trailing slash

**Files:**
- Modify: `router.go`
- Modify: `router_test.go`

**Step 1: Write the failing tests** — 405 with an `Allow` header built
ServeMux-exactly: the methods registered for the path **sorted
lexicographically**, implicit `HEAD` whenever `GET` is registered
(even with no explicit `HEAD` pattern), **no implicit `OPTIONS`**,
and the **trailing-slash variant unioned in when the request path
lacks a slash** (e.g. `POST` registered only on `/users/`, request
`GET /users` → `Allow` includes `POST`); exact strings asserted (e.g.
`GET, HEAD, POST`); a custom method (`MKCOL`) matches exactly, is
case-sensitive (`mkcol` does not match), and sorts into `Allow` with
the rest; an empty-method `Handle("", …)` registration surfaces as a
`Build` error (registration returns nothing and never panics);
`HEAD` served by the `GET` handler unless an explicit `HEAD` exists;
`OPTIONS *` answered with `Allow` — the test calls `ServeHTTP`
directly (or runs a server with `DisableGeneralOptionsHandler: true`),
because a standard `http.Server` intercepts `OPTIONS *` before the
handler (`globalOptionsHandler`: 200, no `Allow`; probed on Go
1.26.5), and ServeMux alone answers 400; trailing slash:
`RedirectTrailingSlash` on → **307** to the slash form **when the
slash variant has a pattern matching the request method** — parity
with ServeMux, which also redirects with 307 (probed), and **405
with `Allow` unioning the slash variant's methods when no method
matches** (probed) — off → 404; explicit
`/users` + `/users/` both match their exact forms.

**Step 2: Run to verify they fail** — FAIL.

**Step 3: Implement** per `docs/lld/router.md` §3, §5.

**Step 4: Run tests** — PASS.

**Step 5: Commit**

```bash
git add router.go router_test.go
git commit -m "router: 405/HEAD/OPTIONS and trailing-slash policy"
```

### Task 15: Host patterns

**Files:**
- Modify: `router.go`
- Modify: `router_test.go`

**Step 1: Write the failing test** — `Handle("GET", "api.example.com/v1/{id}", h)` matches `Host: api.example.com:443` and rejects other hosts; hostless patterns match any host; a Host-less HTTP/1.0 request matches hostless patterns only.

**Step 2: Run to verify it fails** — FAIL.

**Step 3: Implement** — host stored per node, compared case-insensitively minus port (`docs/lld/router.md` §4).

**Step 4: Run tests** — PASS.

**Step 5: Commit**

```bash
git add router.go router_test.go
git commit -m "router: host-pattern matching"
```

### Task 16: Route differential vs ServeMux + match bench

**Files:**
- Create: `router_diff_test.go`
- Create: `router_bench_test.go`

**Step 1: Write the differential harness** — generated pattern sets
and request corpora (`docs/verification.md` §5): same route, same
`PathValue`, same 405/404, **byte-identical `Allow` strings**
(ServeMux-exact: lexicographic, implicit `HEAD` with `GET`, no
implicit `OPTIONS`, trailing-slash variant unioned in for slash-less
request paths), and trailing-slash redirect parity (status and
location — both 307, probed). Pattern sets use explicit token methods
only — method-less ServeMux patterns are out of scope (router LLD
§3) and are never generated or compared. `OPTIONS *` is excluded from
the differential (simdhttp-specific; ServeMux alone 400s, the standard
server intercepts it unless `DisableGeneralOptionsHandler` is set) and
asserted directly in Task 14's router tests. And the match bench:

```go
func BenchmarkMatch4Segment(b *testing.B) {
	r := New()
	for i := 0; i < 100_000; i++ {
		r.HandleFunc("GET", fmt.Sprintf("/c%d/{a}/{b}/tail", i), h)
	}
	r.Build()
	req := httptest.NewRequest("GET", "/c99999/x/y/tail", nil)
	b.ReportAllocs()
	for b.Loop() {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}
```

**Step 2: Run** — differential must pass; the bench must show zero
allocations per op; the walk's disassembly must show no indirect call.

**Step 3: Commit**

```bash
git add router_diff_test.go router_bench_test.go
git commit -m "router: ServeMux differential and match bench"
```

---

## Phase 3 — helpers, middleware, error adapter

### Task 17: Error adapter `Wrap`

**Files:**
- Create: `wrap.go`
- Create: `wrap_test.go`

**Step 1: Write the failing test** — sentinel errors map to statuses
(`ErrMissingHost`→400, `ErrBodyTooLarge`→413); headers already written
→ error logged, connection closed, no second status.

**Step 2: Run to verify it fails** — FAIL.

**Step 3: Implement** — `Wrap(h func(http.ResponseWriter, *http.Request) error) http.Handler` with an `ErrorMapper` (error-seam default), per `docs/lld/net-http-integration.md` §3.

**Step 4: Run tests** — PASS.

**Step 5: Commit**

```bash
git add wrap.go wrap_test.go
git commit -m "router: optional error endpoint adapter"
```

### Task 18: JSON, query/form helpers, `Param`

**Files:**
- Create: `helpers.go`
- Create: `helpers_test.go`

**Step 1: Write the failing tests** — `JSON(w, status, v)` and
`JSONDecode(req, &v)` round-trip through simdjson; query/form accessors
borrow, no `encoding/json` in the dependency graph (assert via
`go list -deps` in a test).

**Step 2: Run to verify they fail** — FAIL.

**Step 3: Implement** — concrete functions on simdjson; `Param(r, name)`
over `PathValue`; add `github.com/sebishogun/simdjson` to `go.mod`.

**Step 4: Run tests** — PASS.

**Step 5: Commit**

```bash
git add helpers.go helpers_test.go go.mod go.sum
git commit -m "router: simdjson JSON, query/form helpers, Param"
```

### Task 19: Middleware stack and end-to-end suite

**Files:**
- Modify: `router.go`
- Modify: `wrap.go`
- Create: `e2e_test.go`

**Step 1: Write the failing tests** — `Use` wraps all routes; the
e2e suite runs the router + middleware + adapter on a real
`http.Server` with a real client: hijack preserved (WebSocket-shaped),
`Flusher` works, h2 and TLS smoke tests pass without `http1`
involvement.

**Step 2: Run to verify they fail** — FAIL (no `Use`).

**Step 3: Implement** — `Use(mws ...func(http.Handler) http.Handler)`
compiles the chain at `Build`; the e2e suite per
`docs/lld/net-http-integration.md` §7.

**Step 4: Run tests** — PASS.

**Step 5: Commit**

```bash
git add router.go e2e_test.go
git commit -m "router: middleware stack and httptest end-to-end suite"
```

---

## Phase 4 — seams, gates, docs

### Task 20: Seam audit and hot-loop gates

**Files:**
- Modify: `router.go`, `http1/parser.go`, `http1/body.go`
- Create: `seams_test.go` (disassembly/`perf stat` assertions live in the Makefile gates, not tests)

**Step 1: Write the failing check** — a Makefile target
`hot-loops-check` fails if `go tool objdump` shows an indirect call in
`Router.ServeHTTP`, `http1.Parse`, or `BodyReader.Read`. The target
also records `perf stat -e instructions:u,cycles:u` against the
committed baseline, with this rule: instructions-retired is
layout-independent evidence, so there is **no fixed 8.3% (or any
other) percentage threshold on instructions**. An instruction-count
increase must instead be explained from the disassembly and is
accepted only when it buys a cycle or wall-clock win; a decrease is
recorded but not required. The 8.3% floor applies to wall-clock bench
comparisons only (interleaved, minima, load < 1), per
`docs/verification.md` §2.

**Step 2: Run to verify it fails** — it must fail on any seam added
without the audit (that is the point).

**Step 3: Implement** — move any dispatch found behind the four allowed
seams (codec/error/observability/future server); verify the no-seam
paths disassemble with no indirect call; commit the instruction/cycle
baseline alongside the disassembly evidence.

**Step 4: Run** — `make hot-loops-check`, full suite, race, fuzz
smokes. PASS.

**Step 5: Commit**

```bash
git add Makefile
git commit -m "gates: hot-loop disassembly and perf-stat checks"
```

### Task 21: Makefile gates rework

**Files:**
- Modify: `Makefile`

**Step 1: Write the failing check** — a CI-style run of `make bench-check`
must fail when a row regresses past 8% (today it launders the red via
`tee` without `pipefail`, `docs/wrong.md` §8).

**Step 2: Run to verify it fails** — verify the pipefail flaw is
reproduced, then fixed: `set -o pipefail` (or no pipe at all), commit
`testdata/bench.txt` baseline.

**Step 3: Implement** — `verify: test vet race fuzz-smoke cross-arch
tier hot-loops-check bench-check`; add the cross-arch and `GOSIMD`
tier lanes (`docs/verification.md` §8).

**Step 4: Run** — `make verify` PASS.

**Step 5: Commit**

```bash
git add Makefile testdata/bench.txt
git commit -m "gates: pipefail-safe bench-check, cross-arch and tier lanes"
```

### Task 22: Docs to shipped reality

**Files:**
- Modify: `README.md`, `docs/architecture.md`, `docs/lld/*.md`,
  `docs/roadmap.md`, `docs/verification.md`, `docs/wrong.md`,
  `docs/plans/2026-08-13-simdhttp-production*.md`

**Step 1: Update every current-state section** — architecture §1–2
becomes history; LLDs become implementation notes where shipped;
roadmap phases checked off with measured numbers; wrong.md gains any
new findings from the phases (entry is the deliverable); the plan files
get an executed-notes header.

**Step 2: Verify** — every new claim traced to source per
`AGENTS.md`; links resolve; gates green.

**Step 3: Commit**

```bash
git add README.md docs/
git commit -m "docs: production phases shipped, records updated"
```
