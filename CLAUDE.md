# Working on this project (Claude edition)

This file is the concise self-contained version. **AGENTS.md is the
canonical rule file** — when this file and AGENTS.md disagree, AGENTS.md
wins and this file is wrong.

## Status

simdhttp ships exactly the borrowed-buffer HTTP/1 request-head parser
`simdhttp.Parse` — no router, no body framing, no middleware, no
server — and nothing is released by tag (verified: no tags on the
repository). Ownership and concurrency are part of the contract: the
caller owns the bytes and their lifetime (fields alias the buffer); a
`Request` is not safe for concurrent use (`Parse` reuses its scratch);
the net/http contract is one-directional with deviations D1–D10
(`docs/architecture.md` §2.1). The roadmap, production design, and plan
are the approved target, not shipped. The differential fuzz smoke is
**red by design, locally** on the duplicate-Host gap G2 (`docs/wrong.md`
§3) until the roadmap's Phase 0 (production plan Tasks 1–8) fixes the
parser — the red replays from the local campaign cache and a
run-written corpus file, **no seed is committed**, so a fresh clone's
fuzz stays green until rediscovery; plan Task 3 pins the seed with the
fix. Read the red, never pipe it.

## Task scope

Task scope is per-task instruction: the branch and the files a task may
touch come from the task text. A documentation-only task is that task's
scope, not a standing rule about the repository. Push only on explicit
request; commit locally in house style when the task asks for it; never
amend a committed change without instruction.

## Read order (required; matches AGENTS.md)

1. `README.md` — front page, shipped surface, gaps, historical chart.
2. `docs/architecture.md` — gaps G1–G8, behavior policy D1–D10, target.
3. `docs/roadmap.md` — staged phases; nothing shipped.
4. `docs/plans/2026-08-13-simdhttp-production-design.md` — approved design.
5. `docs/lld/router.md` — router LLD (target).
6. `docs/lld/http1-head-parser.md` — head parser LLD.
7. `docs/lld/http1-body-framing.md` — body framing LLD (target).
8. `docs/lld/net-http-integration.md` — integration LLD (target).
9. `docs/verification.md` — every gate.
10. `docs/wrong.md` — findings; a new finding belongs there whether or
    not code changed.
11. `docs/plans/2026-08-13-simdhttp-production.md` — future TDD plan;
    execute only when a task says so.

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
- **Verification and release gates.** Every commit passes the gates in
  `docs/verification.md`; a release runs the full gated set and exists
  only as a tag. There is no release today (no tags).
- **The record:** findings that cost a measurement go into
  `docs/wrong.md`; the entry is the deliverable.

## Gates checklist (run bare, in order, before any commit)

1. `go test ./...`
2. `gofmt -l .` and `go vet ./...`
3. `go test -race ./...`
4. `go test -fuzz=FuzzParseAgainstNetHTTP -fuzztime=15s .` — currently
   **red by design, locally** (the fuzz reaches the documented
   duplicate-Host gap G2; wrong.md §3, verification.md intro; no seed
   is committed, so fresh clones stay green until rediscovery — plan
   Task 3 pins the seed with the fix). Read the red, never pipe it.
5. Markdown checks: links inside `docs/` resolve; no trailing
   whitespace in touched files.
6. `git diff --stat` and a full read of the diff; commit message in
   the repo style (`docs: ...`).

Deviations and parity closures live in `docs/architecture.md` §2.1
(D1–D10) — when writing about CL+TE, Host, versions, targets, or
framing, check the policy list first and keep the oracle verdicts
probed, not assumed.
