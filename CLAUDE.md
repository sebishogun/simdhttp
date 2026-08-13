# Working on this project (Claude edition)

This file is the concise self-contained version. **AGENTS.md is the
canonical rule file** — when this file and AGENTS.md disagree, AGENTS.md
wins and this file is wrong.

## Status and boundary

simdhttp is tracked here on the `docs/v120-documentation` branch for
documentation work. Current state: the repo ships only the borrowed-buffer
HTTP/1 request-head parser (`simdhttp.Parse`); the production target
(router, `http1` framing, helpers) is designed but not built
(`README.md`, `docs/architecture.md`, `docs/roadmap.md`).

**Non-negotiable:** only `.md` files may change on this branch. Never
touch Go sources, tests, fuzz corpora, baselines, `go.mod`/`go.sum`, the
`Makefile`, workflows, `docs/bench.svg` or any asset, or release records.
Never push; commits stay local. If a doc claim cannot be made true, the
doc changes — the code never does.

## Read order

1. `AGENTS.md` — canonical rules.
2. `README.md` — front page, shipped surface, gaps, historical chart.
3. `docs/architecture.md` — gaps G1–G8, behavior policy D1–D10, target.
4. `docs/roadmap.md` — staged phases.
5. `docs/lld/` — router, head parser, body framing, integration LLDs.
6. `docs/verification.md` — every gate.
7. `docs/wrong.md` — findings; a new finding belongs there whether or
   not code changed.
8. `docs/plans/2026-08-13-simdhttp-production*.md` — future TDD plan
   (not to be executed on this branch).

## Non-negotiables

- **Claims must be sourced.** Every statement about the code is checked
  against `parser.go`, the tests, `Makefile`, `go.mod`/`go.sum`, and
  `git log` before it is written; verdicts are re-probed live against
  the Go 1.26.5 oracle. Unverifiable claims are either verified first
  (scratch program outside the repo) or stated as unverified. A doc
  that guesses is a bug.
- **Disassemble first, always.** Before proposing a cause for anything
  slow: `go test -c -o /tmp/x.test .` and
  `go tool objdump -s 'pkg\.fn' /tmp/x.test`. Disassembly is the only
  arbiter for register pressure, bounds-check elimination, inlining,
  layout, and — critically — whether a hot loop contains an indirect
  call (the production design forbids interfaces/indirect calls in
  route/parser hot loops; only codec/error/observability/future-server
  seams may dispatch).
- **8.3% noise floor** is the simd-family policy, inherited from the
  simd repository's measured record — not locally measured here
  (re-measurement is future work). Wall-clock deltas below it are not
  evidence either way; fall back to `perf stat -e
  instructions:u,cycles:u` (layout-independent) and disassembly. The
  Makefile's "8%" comment is the bench-check regression guard, a
  different number.
- **Bench discipline:** one process, `-shuffle=on`, `-count=6`,
  compared on the minimum, A/B builds interleaved in one session,
  machine quiet (load < 1). Never compare across sessions.
- **Bare gates:** never judge a gate through a pipe without
  `set -o pipefail`. The current `bench-check` target pipes through
  `tee` and ends in an unconditional `@echo`, so it always succeeds —
  a recorded gate flaw (`docs/wrong.md` §8, `docs/verification.md` §2),
  advisory until the gates rework.
- **The record:** findings that cost a measurement go into
  `docs/wrong.md`; the entry is the deliverable.

## Gates checklist (run bare, in order, before any commit)

1. `go test ./...`
2. `gofmt -l .` and `go vet ./...`
3. `go test -race ./...`
4. `go test -fuzz=FuzzParseAgainstNetHTTP -fuzztime=15s .` — currently
   **red by design** (the fuzz reaches the documented duplicate-Host
   gap G2; wrong.md §3, verification.md intro). Read the red, never
   pipe it.
5. Markdown checks: links inside `docs/` resolve; no trailing
   whitespace in touched files.
6. `git diff --stat` and a full read of the diff; commit message in
   the repo style (`docs: ...`).

Deviations and parity closures live in `docs/architecture.md` §2.1
(D1–D10) — when writing about CL+TE, Host, versions, targets, or
framing, check the policy list first and keep the oracle verdicts
probed, not assumed.
