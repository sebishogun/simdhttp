# Working on this repository

This file is the **canonical** rule file. `CLAUDE.md` is the concise,
self-contained edition for Claude; when the two disagree, this file wins.

## What this repository is

simdhttp is a Go library for HTTP/1 request-head parsing on the
[simd](https://github.com/sebishogun/simd) kernels. The shipped surface
is exactly the borrowed-buffer request-head parser `simdhttp.Parse`;
there is no router, no body framing, no middleware, and no server.
Ownership and concurrency are part of the contract: every `Request`
field aliases the caller's buffer, the caller owns the bytes and their
lifetime, and a `Request` is not safe for concurrent use — `Parse`
reuses its scratch. Compatibility with `net/http` is one-directional —
never accept what the standard reader rejects within the documented
scope — and every deviation is enumerated in `docs/architecture.md`
§2.1 (D1–D10).

## Task scope

Task scope is per-task instruction: the branch and the files a task may
touch come from the task text, not from this file. A documentation-only
task is that task's scope, not a standing rule about the repository. In
a documentation-only task, only Markdown documents change — Go sources,
tests, fuzz corpora, `go.mod`/`go.sum`, the `Makefile`, workflows,
assets, and release records stay untouched. Do not push without an
explicit request; commit locally in house style when the task asks for
it, and never amend a committed change without instruction.

## Shipped status (facts, not aspirations)

- **Parser-only.** The shipped surface is `simdhttp.Parse`; everything
  else in the docs is target.
- **No tagged release.** Verified: the repository has no tags; the code
  ships as the branch tip until a gated release exists.
- **Ownership / compat / concurrency.** The caller owns the bytes and
  their lifetime (fields alias the buffer); a `Request` is not safe for
  concurrent use; the net/http contract is one-directional with
  deviations D1–D10 (`docs/architecture.md` §2.1).
- **Roadmap-not-shipped.** The roadmap, production design, and plan are
  the approved target — nothing in them is shipped until it is in the
  code and the tests. Do not write prose that makes an open item sound
  built.
- **Verification and release gates.** Every commit passes the gates in
  `docs/verification.md`; a release runs the full gated set and exists
  only as a tag. There is no release today.
- **G2 red fuzz blocker.** The differential fuzz smoke is red by design
  on the duplicate-Host case (`0 * HTTP/1.0\r\nHost:\r\nHost:\r\n\r\n`,
  `docs/wrong.md` §3) until the roadmap's Phase 0 fixes the parser —
  the production plan's Tasks 1–8 own that fix. A red run is read,
  never piped; the finding is the deliverable.

## Read order (required)

1. `README.md` — front page, shipped surface, gaps, historical chart.
2. `docs/architecture.md` — shipped surface, gaps G1–G8, behavior
   policy D1–D10, target.
3. `docs/roadmap.md` — staged phases; nothing in it is shipped.
4. `docs/plans/2026-08-13-simdhttp-production-design.md` — the approved
   production design.
5. `docs/lld/router.md` — router LLD (target).
6. `docs/lld/http1-head-parser.md` — head parser LLD.
7. `docs/lld/http1-body-framing.md` — body framing LLD (target).
8. `docs/lld/net-http-integration.md` — integration LLD (target).
9. `docs/verification.md` — every gate.
10. `docs/wrong.md` — the record of findings; a new finding belongs
    there whether or not code changed.
11. `docs/plans/2026-08-13-simdhttp-production.md` — the future TDD
    plan; execute it only when a task says so.

## Claims must be sourced, and verified before they are written

Every statement about the code must be checked against the closest source
before it is written:

- exported API and behavior: `parser.go` and the module docs;
- verdicts and parity: `parser_test.go`, `fuzz_test.go`, and a live run
  against `net/http` — the oracle is the executable, not memory;
- versions and dependencies: `go.mod`, `go.sum`;
- benchmark numbers: the README, `docs/bench.svg`, `bench_test.go`,
  `sweep_test.go`, and the `Makefile` bench targets;
- history: `git log` commit messages and diffs.

When a claim is not verifiable from source, either verify it empirically
first (a scratch program outside the repo is allowed) or state it as
unverified. A doc that guesses is a bug. The record of findings that cost
measurement lives in `docs/wrong.md`; a finding belongs there whether or
not any code changed.

## Disassemble first, always

Before proposing a cause for anything slow, before writing a variant,
before reading a profile delta — **build it and read the instructions**.

```
go test -c -o /tmp/x.test .
go tool objdump -s 'pkg\.functionName' /tmp/x.test | less
```

Use gdb when a breakpoint or a live register is needed, and both together
when that helps. Go compiles in seconds; there is no cost to looking, and
every guess that skips it costs a build-measure-revert cycle and risks a
wrong conclusion landing in `docs/wrong.md` as fact.

What the disassembly says that nothing else does:

- **Register pressure.** A large stack frame with the loop counter or a
  flag spilled and reloaded per iteration. No performance counter reports
  this.
- **Whether a bounds check was eliminated**, and whether an index multiply
  is a shift or a multiply.
- **Whether a call was inlined**, and whether `append(b, s...)` became
  inline stores or a `memmove` call.
- **Which branch the compiler laid out as fallthrough.**
- **Whether a hot loop contains an indirect call.** The production design
  forbids interfaces and indirect calls in route and parser hot loops;
  only the codec, error, observability, and future-server seams may
  dispatch, and each must pass the disassembly and cycle checks.

## Benchmarks

The code-layout noise floor is the **8.3%** family policy, inherited
from the measured record in the simd repository's `CLAUDE.md` — it has
not been re-measured in this repository (future work), so treat it as
inherited policy, not a locally measured constant. Anything smaller
cannot be told from nothing by wall-clock, and more samples do not help
— layout noise is per-build, not per-run. When a change is expected to
be worth less than that:

- compare **instructions retired** and **cycles** with `perf stat -e
  instructions:u,cycles:u`, which are layout-independent;
- and read the disassembly, which is the only thing that explains *why*.

A/B builds must be **interleaved** in one session and compared on the
minimum, never across sessions. Run the machine quiet: wait for load
average under 1.

**Never pipe a gate through `tail`** (or anything else) without
`pipefail`: the pipe reports the last command's status and the failure
vanishes. Run gates bare, or `set -o pipefail` first. Note that the
current `Makefile` `bench-check` target pipes through `tee` without
`pipefail` **and ends in an unconditional `@echo`**, so it always
succeeds — a known gate flaw, recorded in `docs/wrong.md` and
`docs/verification.md`, scheduled for the gates rework.

## The record

`docs/wrong.md` holds measurements that argued against changes, including
changes that were then reverted, and sourced safety findings. A finding
that cost a measurement belongs there whether or not any code changed —
the entry is the deliverable.

## Verification before any commit

Run the gates bare, in this order, and read the output:

1. `go test ./...`
2. `gofmt -l .` and `go vet ./...`
3. `go test -race ./...`
4. a fuzz smoke: `go test -fuzz=FuzzParseAgainstNetHTTP -fuzztime=15s .`
   (currently **red by design**: the fuzz reaches the documented
   duplicate-Host gap G2 — wrong.md §3, verification.md intro. A red
   run must be read, not piped; the finding is the deliverable.)
5. Markdown checks: links inside `docs/` resolve, no dead references, no
   trailing-whitespace drift in files touched.

Then `git diff --stat` and a full read of the diff before committing.
Commit messages follow the repository style (`docs: ...` for
documentation commits). A commit's contents follow the task scope: a
documentation-only task commits Markdown only — never Go, tests,
modules, the Makefile, workflows, assets, baselines, or release
records.
