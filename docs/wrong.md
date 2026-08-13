# Where the obvious answer was wrong

Entries are written as *what was believed / what was true / how it
surfaced*, with the source that established it, because that is the only
form in which this knowledge is useful. This repository records only two
classes of finding so far: the measured performance reversal in the
shipped parser's history, and sourced safety findings in the shipped
parser. A finding that cost a measurement belongs here whether or not
any code changed.

---

## 1. The typical-shape advantage reversed 1.22x to 1.05x

**Believed.** The one-pass boundary scan and inline short-line path that
fixed the 100-header shape (1.3x loss -> 4.7x win) could be judged by
that row alone; the typical nine-header shape was already a win and
would stay one.

**Actually.** The rework that fixed the many-header shape cost the
typical shape. Commit `a60a44b` (initial parser) measured 1,097 ns vs
net/http's 1,341 on the nine-header browser request — 1.22x, "minimum
of three". Commit `530ac05` (one-pass scan, inline short path, kernel
threshold) shipped a README quoting the typical row at ~1,400 vs ~1,470
— 1.05x. The 100-header row went from a 1.3x loss to 2,219 vs 10,491
(4.7x), and the giant-value row stayed 3.5x on aliasing, but the
typical shape's margin nearly halved.

**How it surfaced.** Not by any new measurement of the typical shape —
the number was already in the 530ac05 README diff, next to the win it
was expected to justify. The reversal is only visible by comparing the
two commits' READMEs (and the chart, `docs/bench.svg`, which draws the
post-rework 1.05x).

**Source.** `git log` — commit messages and README diffs of `a60a44b`
and `530ac05`; README.md as of `5c2bee2`.

**Consequence.** The README now carries the chart and table as
historical amd64/AVX-512 data, not as a current or portable promise,
and any future change to the header walk re-measures the typical shape
against the 8.3% floor plus `perf stat` and disassembly.

---

## 2. The long-value control scan stops at a tab

**Believed.** The value control scan is a single
`simd.IndexAnyOrLess(val, "\x7f", 0x20)` call with an HTAB allowance;
`\t` is below 0x20, so a value containing a tab is checked, the tab
excused, and the scan done.

**Actually.** The call returns the *first* byte in `\x7f ∪ < 0x20`. If
that byte is `\t`, the guard `val[i] != '\t'` passes and the scan
stops — a control byte after the first HTAB in a value at or above
`ctlScanThreshold` (64) bytes is never examined. The short-value inline
loop has no such hole (it scans every byte). net/http rejects the same
value ("malformed MIME header line").

**How it surfaced.** Source reading of `parser.go`'s threshold branch
during the 2026-08-13 documentation audit, then a live differential
run: `X-Long: <70×v>\t<10×w>\x00<20×x>` is accepted by simdhttp and
rejected by net/http; the identical value with the NUL before the tab
is rejected by both; the identical value under 64 bytes is rejected by
both. The differential fuzz cannot find it either: its seeds are short
and a 15 s run (~3.9 M execs) never grows a ≥ 64-byte value, so the
oracle is never asked.

**Source.** `parser.go:148-158` (threshold branch), `simd` v1.20.0
`IndexAnyOrLess` contract (`text.go`), live differential, Go 1.26.2.

**Consequence.** G1 in `docs/architecture.md`; fix designed in
`docs/lld/http1-head-parser.md` §3.3 with a regression test that fails
on today's parser; fuzz seeds extended in Phase 0.

---

## 3. Duplicate Host is accepted, even when identical

**Believed.** Headers are an unordered list; two `Host` lines with the
same value are equivalent to one.

**Actually.** net/http rejects *any* second `Host` line at read time —
"too many Host headers" — including two identical ones
(`request.go:1139`). simdhttp accepts them.

**How it surfaced.** Differential run, 2026-08-13:
`Host: a.com\r\nHost: a.com` — net/http errors, simdhttp returns both.

**Source.** Live differential; Go 1.26.5 `net/http/request.go:1139`.

---

## 4. Request-target control bytes are accepted

**Believed.** The fuzz contract says URI semantics are the caller's
(net/url) business, so the parser need not look at the target at all.

**Actually.** The exclusion is about *semantics* (what net/url accepts
as a URI). Control bytes — NUL, DEL — in the target are a framing-level
hygiene matter: net/http rejects them via `url.ParseRequestURI`
("invalid control character in URL") while simdhttp accepts them
unchanged. `%20`-style escapes are fine in both; that is the semantic
part. The parser's own strictness (tokens, CRLF, controls in values)
makes the target the one unchecked control surface.

**How it surfaced.** Differential run, 2026-08-13: `GET /a\x00b` and
`GET /a\x7fb` accepted by simdhttp, rejected by net/http.

**Source.** Live differential; `fuzz_test.go`'s scoping comment
(read with care: it excludes net/url verdicts, which is exactly the
layer that catches this).

---

## 5. Framing fields are opaque — CL+TE and duplicate CL pass

**Believed.** A head parser that stops at the blank line has no framing
responsibility; whatever a future body layer needs, it can re-derive.

**Actually.** `Content-Length` and `Transfer-Encoding` are smuggling
surface from the moment a server consumes the head. net/http's
`ReadRequest` rejects a second `Content-Length` ("message cannot
contain multiple Content-Length headers"); simdhttp returns both lines
opaquely, and CL+TE together pass both.

**How it surfaced.** Differential run, 2026-08-13: two differing CLs —
net/http errors, simdhttp accepts; CL+TE — both accept at read level
(Go's server rejects the combination later).

**Source.** Live differential; Go 1.26.5 `net/http` request reading.

---

## 6. HTTP/1.1 without a usable Host is accepted

**Believed.** Host is a header like any other; the parser's job stops
at the blank line.

**Actually.** Host is a framing-critical field. net/http's server
rejects HTTP/1.1 requests with no Host ("missing required Host
header", `server.go:1023`), rejects a malformed Host ("malformed Host
header", via `httpguts.ValidHostHeader`), and `ReadRequest` rejects
duplicates (finding 3). simdhttp accepts `GET / HTTP/1.1\r\n\r\n`,
`Host:`, and `Host: bad host` alike.

**How it surfaced.** Differential run, 2026-08-13; source check of the
Go server's read path.

**Source.** Live differential; Go 1.26.5 `net/http/server.go:1007-1034`.

---

## 7. The differential fuzz found two real gaps before passing

**Believed.** The initial parser's version check and request-line
splitting were complete.

**Actually.** Commit `530ac05`'s message records what the differential
fuzz found at ~35 M executions: an unvalidated HTTP version (any third
field passed) and the empty request-target case (`GET  HTTP/1.1`-style,
where the old `len(Proto)==0` guard did not cover the empty target).
Both became `isHTTPVersion` and the explicit empty-target rejection.

**How it surfaced.** Fuzz failure logs during the `530ac05` session,
then the two fixed branches.

**Source.** `git log` commit `530ac05` message and `parser.go` diff.

**Consequence.** The fuzz found what the corpus missed; the corpus is
extended in Phase 0 with the G1 long-value seed so the same class does
not hide again.

---

## 8. `bench-check` launders red gates (gate flaw, not a code flaw)

**Believed.** `make bench-check` reports failure when a row regresses
past the 8% floor.

**Actually.** The target pipes the bench through `tee` with no
`pipefail`, so the target's exit status is `tee`'s, and the house rule
("never pipe a gate without `pipefail`") is violated in this repo's own
Makefile. A red comparison is laundered green.

**How it surfaced.** Reading the `Makefile` against the house rule
during the 2026-08-13 audit. No regression has been laundered in this
repo's short history; the flaw is the exposure.

**Source.** `Makefile` `bench-check` target.

**Consequence.** `bench-check` is advisory until the Phase 4 gates
rework (it also references `testdata/bench.txt`, which no commit has
ever added); all benches are judged by bare, interleaved, minimum-of-six
runs per `docs/verification.md` §2.
