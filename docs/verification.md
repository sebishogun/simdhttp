# Verification

The gates every simdhttp change and every claim must pass. Current
baseline verified 2026-08-13 on this machine (go1.26.2, amd64): `go test`,
`go vet`, `gofmt`, `go test -race` green; 15 s differential fuzz smoke
green at ~3.9 M execs. The gates below cover current code and the
roadmap's phases; phase-specific gates are marked.

## 1. Unit and differential tests

- `TestParseAgainstNetHTTP`: corpus verdicts, field equality, consumed,
  prefix-incompleteness at every prefix, 5,000 single-byte mutations.
- Extended corpus (Phase 0, LLD `http1-head-parser.md` §4): duplicate
  Host (identical and differing), missing/empty Host on 1.0 and 1.1,
  target controls, double CL, CL+TE, tab-before-control long values at
  63/64/65 B and 1 KiB, obs-fold, bare LF, space-in-name.
- Assertion direction: simdhttp must never accept what net/http rejects
  (framing; net/url target semantics excepted per `fuzz_test.go`), and
  accepted fields must match byte-for-byte. The strict profile is a
  documented superset of rejections.

## 2. Gate hygiene (house rules)

- Gates run bare: no pipe without `set -o pipefail`. The current
  `Makefile` `bench-check` pipes through `tee` with no `pipefail` — a
  known flaw (wrong.md §8); `bench-check` is advisory until the Phase 4
  gates rework fixes it, and is never relied on in CI form.
- Bench runs: one process, `-shuffle=on`, `-count=6`, compared on the
  **minimum**, A/B builds **interleaved in one session**, machine quiet
  (load average < 1). Never compare across sessions.
- The **8.3% code-layout noise floor**: a wall-clock delta below it is
  not evidence either way; fall back to `perf stat -e
  instructions:u,cycles:u` and disassembly.

## 3. Request-smuggling corpora

Fixed corpora, committed with each phase's tests (verification LLD
`http1-body-framing.md` §4 is the oracle table):

- CL.TE / TE.CL / CL.CL (equal and differing) / TE.TE — both chunked and
  obfuscated encodings;
- bare LF, CR without LF, LF without CR, space-in-name, obs-fold;
- tab between method and target, multiple spaces, empty target;
- Host: duplicates, empty, comma-separated, IPv6 brackets, controls;
- target: NUL/DEL/other controls, overlong escapes, `%2F`, absolute-form,
  asterisk-form;
- oversized: head past every limit, chunk-size line past its limit,
  trailer count past its limit;
- truncated at every byte position of a chunked body and its trailers.

Every input gets a single verdict from the framing table; the table has
one implementation, so the "two parsers disagree" attack class has
nothing to disagree with.

## 4. Fuzz (no panic, no hang)

- `FuzzParseAgainstNetHTTP` — differential, one-direction contract;
  seeds extended in Phase 0 with a ≥ 64-byte value containing `\t` +
  `\x00` so the G1 path is reached.
- `FuzzBodyReader` (Phase 1) — arbitrary byte streams into
  `BodyReader` with random limits: no panic, no hang (per-read timeout),
  `(0, nil)` never returned, framing verdicts stable under input prefix
  extension (a valid framing must not turn invalid by appending bytes
  to a *different* request).
- `FuzzRouterMatch` (Phase 2) — random paths and methods against a
  built table: no panic, deterministic params.
- Smoke policy: every commit runs `-fuzztime=15s` on each target; the
  roadmap phases run overnight budgets before merge.

## 5. Route differential vs net/http

Generated pattern sets (params, wildcards, hosts, trailing slashes,
method mixes) and generated request corpora; for each request, compare
simdhttp vs `net/http.ServeMux` on: matched route, `PathValue` contents,
405 vs 404, `Allow` header, trailing-slash redirect (status + location).
Agreement is required in compatible mode; strict mode's documented
supersets are listed explicitly per case, not discovered at test time.
`httptest` is the harness; no sockets.

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
