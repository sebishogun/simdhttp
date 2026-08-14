# Working on this repository

This file is the **canonical** rule file. `CLAUDE.md` is the concise,
self-contained edition for Claude; when the two disagree, this file wins.

## Core tenets: performance-aware programming

**These are the core tenets of this codebase. Read them before writing a line.**

The stance is Casey Muratori's: *performance-aware programming*. Not
"optimization" as a phase that happens after the code works — knowing roughly
what the machine will do with what you write, while you write it. The
alternative is not "clean code that gets optimized later"; it is code whose
shape forecloses the fast version, and the rewrite costs more than thinking for
five minutes did.

Two ideas underneath everything below:

- **Know the order of magnitude before you type.** How many times does this run
  — once, per request, per row, per element? What does one iteration touch?
  Nobody needs a cycle count; everybody needs to know whether they just wrote
  something that runs 200,000 times and allocates.
- **The machine is not an abstract machine.** It has caches, a prefetcher, wide
  registers, and many cores. Code that pretends otherwise leaves 10-100x on the
  floor, and no amount of later profiling recovers a layout decision.

**How the tenets relate.** They are not a list of independent good ideas. The
data-layout ones exist to make the bulk operation POSSIBLE:

    struct-of-arrays + grouped lifetimes + zero per-element allocation
        -> contiguous, uniformly-typed arrays
            -> the kernel can run at all
                -> SIMD, and the parallel shard boundaries come free

You cannot vectorize an array-of-structs: the lanes are not adjacent. You
cannot vectorize a slice that is really a graph of separately-allocated
objects. You cannot keep a kernel fed if every element costs an allocation.
So struct-of-arrays, arenas and lifetime grouping are not housekeeping to do
after the fast path works — they are the precondition for the fast path
existing, and the reason a layout decision made carelessly cannot be recovered
by profiling later.

Read the sections in that order, and design in that order.

### 1. Zero allocations wherever it is possible at all

Not "few" — zero, on any path that runs per element, per record, per row or per
request.

The checklist, in the order it usually pays:

- **Nothing per-element or per-record that can be per-batch.** A `map` built
  per record, a `fmt.Sprintf` per line, a `[]byte`->`string` per field: at 200k
  records those are 200k allocations and 200k pieces of GC work. Reach for a
  byte scan over the fixed shape instead of a reflective decode into a map.
- **Size every slice and map you can size.** `make([]T, 0, n)` when n is known
  or estimable. Growing from nil reallocates and copies the whole thing at
  every doubling.
- **Reuse the caller's buffer.** Append into a supplied `[]byte`, compact in
  place when the write cursor provably trails the read cursor, take a `dst`
  parameter rather than returning a fresh allocation.
- **Do not scan twice.** If a later stage already parses the data, do not
  validate it fully first — do the O(1) structural check and let the one place
  that parses report the rest.
- **Escape analysis is part of the design.** A pointer stored in an interface,
  a closure capturing a local, a returned slice of a local array: each forces a
  heap allocation. `go build -gcflags=-m` says which.
- **Prefer a wider type to a pointer chase.** An index into a slab beats a
  pointer when the slab is contiguous — it is smaller, it does not escape, and
  it keeps the array vectorizable.

Verify with `-benchmem`. `0 allocs/op` is a target you can actually hit on a
hot path, and worth stating in the doc comment when you hit it.

### 2. Think about the data, then the code

Muratori's central point, and the one most often skipped. The layout of the
data decides the speed; the instructions are usually a detail.

**Struct-of-arrays over array-of-structs** for anything scanned columnwise. A
filter that reads one field should stream that field's array, not stride
through whole records pulling in fifteen fields it does not want. This is the
single highest-leverage decision in a columnar store, and it is made when the
type is declared, not later.

**Group lifetimes; allocate them together.** Objects born together and dying
together belong in one allocation. A per-request arena — one buffer that
everything for that request is carved out of, released in one move when the
request ends — replaces thousands of individual allocations and frees with a
single pointer reset. It also gives locality for free: everything the request
touches is contiguous. Where the lifetime is per-batch, per-group or
per-connection instead, the same applies at that scope. The rule is that the
allocation boundary should match the lifetime boundary; when it does not, you
get either leaks or a per-object free.

**Use the whole cache line.** Touch it once and consume all of it. Block a pass
to fit L1/L2 rather than striding across a large array repeatedly. Keep hot
fields adjacent and cold fields elsewhere so a line carries only what the loop
reads. Watch for false sharing when threads write adjacent words.

Locality is a hypothesis to check with perf counters, not a rule to apply
blindly: windowing won in simdcsv and did nothing in simdjson.

### 3. Do the work in bulk — use SIMD

This family exists for it. Whole-slice work goes through the kernels, not a
hand-written scalar loop. Where no kernel exists for the shape, say so
explicitly rather than quietly writing the scalar loop and leaving it.

Check the dispatch actually reaches the kernel at runtime: every complex kernel
in `simd` was dead code from v1.14.0 to v1.20.0 because nothing walked the
tables the runtime indexes.

A per-element function call defeats vectorization outright — measured at 11
extra instructions per element, a 2.56x ratio. If the API shape forces one, the
API shape is the bug.

### 4. Don't do the work at all

The fastest code is the code that does not run. Prune before you decode: a
bloom filter that rejects a group, a time window that skips a block, a column
never materialized because nothing asked for it. simdlogs' rare-needle path
beats a full scan by rejecting groups without decoding them, not by decoding
faster.

Hoist invariants out of loops. Compute once what does not change. Do not scan
twice — if a later stage already parses the data, do the O(1) structural check
and let the one place that parses report the rest.

### 5. Multi-threaded where it is beneficial

And only there. Parallelism pays when the work per shard clears the
coordination cost; below that it is slower and less predictable.

Shard on a boundary the data already has (groups, blocks, row ranges), give
each worker its own output buffer, merge once. Never share a mutable buffer
between goroutines without saying so in the doc comment. `-race` is a gate.

### 6. `sync.Pool` is the last resort — and it has to be correct

Reach for it last. Most allocation wins are a size hint, an arena, or a
caller-supplied buffer: free at runtime, no correctness hazard. A pool costs
Get/Put, a miss allocates anyway, and it introduces a class of bug the others
cannot have.

When a pool IS the right answer, these are not optional:

- **The buffer must be fully overwritten before anything reads it.** A pooled
  buffer arrives holding a PREVIOUS request's data. If any path reads an
  element it did not write, that request's data is silently served to this one
  — a correctness bug, cross-request data leakage, not a performance one. Know
  the property holds and say WHY in the doc comment; do not assume it.
- **Prove it with a poisoning test.** Fill pooled buffers with a value that
  cannot occur, then assert the pooled result equals the unpooled result
  exactly. Write that test FIRST. This is the only thing that catches the bug,
  because the unpooled path zeroes and therefore hides it.
- **Ownership must be unambiguous.** A pooled buffer must not escape into a
  returned value, be captured by a goroutine that outlives the Put, or be
  aliased by a slice the caller keeps. Returning a slice of a pooled array is a
  use-after-free in all but name.
- **Put back exactly what you took**, reset to a known state, once. A double
  Put hands the same array to two callers at the same time.
- **Pool a pointer, not a slice.** A `[]T` placed in an `any` allocates on
  every Put, which is the cost the pool exists to remove.
- **Sizing is part of the contract.** A pool of mixed sizes either wastes the
  large buffers or reallocates on the small ones; decide which and say so.
- **Testing note:** `sync.Pool.Put` drops the value at random one time in four
  under `-race`, so any test asserting reuse across a single round trip is red
  a quarter of the time. Assert reuse within a few attempts, not on a
  particular one.

### Then measure

These tenets are where to start, not a substitute for the benchmark.
Fast-looking code that was never measured is a guess. The noise floor, the
interleaved A/B discipline, and "disassemble before you theorise" apply to code
written this way exactly as they apply to a tuning change — and a claim with no
number behind it does not go in a doc.

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
  the production plan's Tasks 1–8 own that fix. The red is local: it
  replays from the campaign cache and a run-written corpus file, and
  **no seed is committed** (`git ls-files` has no `testdata`), so a
  fresh clone's fuzz stays green until rediscovery; plan Task 3
  commits the seed `testdata/fuzz/FuzzParseAgainstNetHTTP/4cb4ee00bf74f878`
  with the fix. A red run is read, never piped; the finding is the
  deliverable.

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
   (currently **red by design, locally**: the fuzz reaches the
   documented duplicate-Host gap G2 — wrong.md §3, verification.md
   intro. A fresh clone has no committed seed and stays green until
   rediscovery; plan Task 3 pins the seed with the fix. A red run
   must be read, not piped; the finding is the deliverable.)
5. Markdown checks: links inside `docs/` resolve, no dead references, no
   trailing-whitespace drift in files touched.

Then `git diff --stat` and a full read of the diff before committing.
Commit messages follow the repository style (`docs: ...` for
documentation commits). A commit's contents follow the task scope: a
documentation-only task commits Markdown only — never Go, tests,
modules, the Makefile, workflows, assets, baselines, or release
records.
