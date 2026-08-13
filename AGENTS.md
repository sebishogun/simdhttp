# Working on this repository

This file is the **canonical** rule file. `CLAUDE.md` is the concise,
self-contained edition for Claude; when the two disagree, this file wins.

## Scope: documentation only

This repository tracks the simdhttp project on the `docs/v120-documentation`
branch. The only files that may change here are Markdown documents. The
following are **frozen** — never modified, added, or removed:

- `parser.go`, `*.go` sources and any future Go files
- `*_test.go`, including fuzz corpora and baselines
- `go.mod`, `go.sum`
- `Makefile` and any workflow files
- `docs/bench.svg` and any assets
- `testdata/`, release records, tags

Nothing is ever pushed from this checkout. Commits stay local until the
task owner moves them.

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
5. Markdown checks: links inside `docs/` resolve, no dead references, no
   trailing-whitespace drift in files touched.

Then `git diff --stat` and a full read of the diff before committing.
Commit messages follow the repository style (`docs: ...`). Never commit
Go, tests, modules, the Makefile, workflows, assets, baselines, or release
records on this branch.
