# Verification

The gates every simdhttp change and every claim must pass. Current
baseline verified 2026-08-13 on this machine (Go 1.26.5 toolchain,
amd64): `go test`, `go vet`, `gofmt`, `go test -race` green; 15 s
differential fuzz smoke green at ~3.9 M execs. The gates below cover
current code and the roadmap's phases; phase-specific gates are marked.
All oracle verdicts in this document refer to the Go 1.26.5 toolchain
actually run; go.mod's `go 1.26.2` directive is a module floor, not an
oracle date.

## 1. Unit and differential tests

- `TestParseAgainstNetHTTP`: corpus verdicts, field equality, consumed,
  prefix-incompleteness at every prefix, 5,000 single-byte mutations
  (no-panic smoke only — it never asserts a verdict; verdicts come
  from the corpus and fuzz).
- Extended corpus (Phase 0, LLD `http1-head-parser.md` §4): duplicate
  Host (identical and differing), missing/empty Host on 1.0 and 1.1,
  target controls and invalid escapes (`%zz`, `%2`), duplicate CL
  (identical and differing), CL+TE, TE `gzip, chunked`,
  tab-before-control long values at 63/64/65 B and 1 KiB, obs-fold,
  bare LF, space-in-name, comma and bracket Hosts.
- Assertion direction: simdhttp must never accept what net/http rejects
  (framing; net/url target semantics excepted per `fuzz_test.go`), and
  accepted fields must match byte-for-byte. The behavior-policy rows
  (architecture §2.1, D1–D10) are asserted one-directionally —
  simdhttp rejects, Go's verdict recorded from the probed oracle —
  with parity closures (D4, D8) asserted as the parity they are, never
  as deviations. The strict profile is a documented superset of
  rejections.

## 2. Gate hygiene (house rules)

- Gates run bare: no pipe without `set -o pipefail`. The current
  `Makefile` `bench-check` target pipes through `tee` **and ends in an
  unconditional `@echo`**, so it succeeds no matter what the bench did
  (wrong.md §8); `bench-check` is advisory until the Phase 4 gates
  rework fixes it, and is never relied on in CI form.
- Bench runs: one process, `-shuffle=on`, `-count=6`, compared on the
  **minimum**, A/B builds **interleaved in one session**, machine quiet
  (load average < 1). Never compare across sessions.
- The **8.3% code-layout noise floor** is the simd-family policy,
  inherited from the measured record in the simd repository's
  `CLAUDE.md` — it has not been re-measured in this repository, and
  that re-measurement is future work. A wall-clock delta below it is
  not evidence either way; fall back to `perf stat -e
  instructions:u,cycles:u` and disassembly. Note the distinction: the
  `Makefile`'s "8%" comment is this repo's chosen regression guard for
  `bench-check`; the 8.3% floor is the measurement-noise policy.

## 3. Request-smuggling corpora

Fixed corpora, committed with each phase's tests (LLD
`http1-body-framing.md` §4 is the oracle table):

- CL.TE / TE.CL / CL.CL (equal and differing) / TE.TE — both chunked and
  obfuscated encodings;
- bare LF, CR without LF, LF without CR, space-in-name, obs-fold;
- tab between method and target, multiple spaces, empty target;
- Host: duplicates, empty, comma-separated (with and without space),
  IPv6 brackets (balanced and unbalanced), controls — the comma and
  bracket rows are deviation D9 (Go accepts both);
- target: NUL/DEL/other controls, invalid escapes (`%zz`, `%2`),
  overlong escapes, `%2F`, absolute-form, asterisk-form;
- version lines: `HTTP/1.2`, `HTTP/2.0`, the `PRI * HTTP/2.0` preface,
  `HTTP/9.9` — Go accepts these (probed); simdhttp rejects (deviation
  D10);
- oversized: head past every limit, chunk-size line past its limit,
  trailer count past its limit;
- truncated at every byte position of a chunked body and its trailers.

Every input gets a single verdict from the framing table; the table has
one implementation, so the "two parsers disagree" attack class has
nothing to disagree with. Deviation rows carry their Go verdict from
the probed oracle so the corpus doubles as the deviation documentation.

## 4. Fuzz (no panic, no hang)

- `FuzzParseAgainstNetHTTP` — differential, one-direction contract;
  seeds extended in Phase 0 with a ≥ 64-byte value containing `\t` +
  `\x00` so the G1 path is reached.
- `FuzzBodyReader` (Phase 1) — arbitrary byte streams into
  `BodyReader` with random limits: no panic, no hang (per-read timeout),
  `(0, nil)` never returned. Pipelining property: appending bytes that
  belong to a following pipelined request must not change the first
  request's verdict or body boundary — the reader consumes exactly the
  framed body, and trailing bytes are reported via `Consumed()`, never
  interpreted as body or verdict input.
- `FuzzRouterMatch` (Phase 2) — random paths and methods against a
  built table: no panic, deterministic params.
- Smoke policy: every commit runs `-fuzztime=15s` on each target; the
  roadmap phases run overnight budgets before merge.

## 5. Route differential vs net/http

Generated pattern sets (params, wildcards, hosts, trailing slashes,
method mixes) and generated request corpora; for each request, compare
simdhttp vs `net/http.ServeMux` on: matched route, `PathValue` contents,
405 vs 404, `Allow` header, trailing-slash redirect (status and
location — parity: both redirect with 307, probed on Go 1.26.5). The
`Allow` comparison is exact: both sides must emit the same string —
lexicographic over the methods registered for the path, implicit
`HEAD` when `GET` is registered, no implicit `OPTIONS` (ServeMux's
`matchingMethods` + `slices.Sorted`; pinned in router unit tests too).
Agreement is required in compatible mode; strict mode's documented
supersets are listed explicitly per case, not discovered at test time.
`OPTIONS *` is excluded from the differential — ServeMux alone answers
400, the standard server intercepts it (`globalOptionsHandler`, 200, no
`Allow`) unless `DisableGeneralOptionsHandler` is set, and simdhttp's
`Allow` behavior is asserted directly in router tests. `httptest` is the
harness; no sockets.

## 6. Disassembly

Per hot loop (`Parse` request-line + header walk, `BodyReader.Read`,
router match walk), after every change touching it:

```
go test -c -o /tmp/x.test .
go tool objdump -s 'pkg\.fn' /tmp/x.test
```

Checked for: **no indirect call** (no `CALL` through a register) except
at the codec/error/observability/future-server seams; bounds checks
eliminated where the compiler can prove them; index multiplies as
shifts; `append`-of-slice as inline stores; no per-iteration spill of
the loop counter. The disassembly is the arbiter for any claim that a
seam or a "no-dispatch" property holds — a profile saying otherwise is
wrong.

## 7. Performance gates

- Head parser: `BenchmarkSweep` shapes (none, typical-9, many-100,
  giant-value) + `BenchmarkParse` head-to-head. Phase 0 must hold the
  historical typical-9 row (README table, historical amd64 data) within
  the 8.3% floor; the G1 fix's cost is measured the same way.
- Router (Phase 2): match of a 4-segment/2-param route at 100k patterns;
  zero allocation on the matched path (`-benchmem`).
- Body (Phase 1): fixed-length and chunked copy at 1 MiB, plus a
  64 B-per-read shape.
- Sub-floor deltas: `perf stat -e instructions:u,cycles:u`, interleaved,
  minima, load < 1.

## 8. Cross-arch and tiers

- `GOOS/GOARCH` compile + test for amd64, arm64, riscv64, loong64
  (cross-compile at minimum; emulated execution where the simd
  toolchain supports it).
- Tier runs: the simd dependency dispatches per instruction set; the
  simdhttp suite runs once per `GOSIMD` tier the host supports, so a
  kernel-path difference cannot hide a verdict difference.
- The README's historical chart stays labeled amd64/AVX-512; no other
  architecture inherits its numbers.

## 9. Race and hygiene

- `go test -race ./...` on every phase; fuzz targets run under `-race`
  in the phase budgets.
- `gofmt -l .` empty, `go vet ./...` clean, links in `docs/` resolve
  (Markdown check runs before commit).
- Every doc claim re-verified against source before commit
  (`AGENTS.md`/`CLAUDE.md` "claims must be sourced").
