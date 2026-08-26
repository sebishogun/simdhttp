# Working on this project (Claude edition)

This file is the concise self-contained version. **AGENTS.md is the
canonical rule file** — when this file and AGENTS.md disagree, AGENTS.md
wins and this file is wrong.

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

## Status

simdhttp ships the root borrowed-buffer parser, the hardened `http1` parser
and body reader, an immutable-build `http.Handler` router, helpers,
middleware, and an error adapter. It does not ship a socket-accepting server
loop, and nothing is released by tag (verified: no tags). The caller owns root
parser bytes and their lifetime; a root `Request` is not safe for concurrent
use because `Parse` reuses its scratch. The net/http contract is
one-directional with deviations D1–D10 (`docs/architecture.md` §2.1).
Phases 0-4 are executed. Their `http1` fuzz seeds are committed; the root
compatibility parser still carries the duplicate-Host differential debt owned
by HTTP-V1-01 and may rediscover it locally. Read a red run, never pipe it.

## Task scope

Task scope is per-task instruction: the branch and the files a task may
touch come from the task text. A documentation-only task is that task's
scope, not a standing rule about the repository. Push only on explicit
request; commit locally in house style when the task asks for it; never
amend a committed change without instruction.

## Read order (required; matches AGENTS.md)

1. `README.md` — front page, shipped surface, gaps, historical chart.
2. `docs/architecture.md` — historical gaps G1–G8, behavior policy D1–D10,
   implemented architecture, deferred server.
3. `docs/roadmap.md` — executed phases 0-4 and current readiness gaps.
4. `docs/plans/2026-08-13-simdhttp-production-design.md` — approved design.
5. `docs/lld/router.md` — shipped router LLD.
6. `docs/lld/http1-head-parser.md` — head parser LLD.
7. `docs/lld/http1-body-framing.md` — shipped body-framing LLD.
8. `docs/lld/net-http-integration.md` — shipped integration LLD.
9. `docs/verification.md` — every gate.
10. `docs/wrong.md` — findings; a new finding belongs there whether or
    not code changed.
11. `docs/plans/2026-08-13-simdhttp-production.md` — executed historical
    plan plus the current follow-on ledger; do not re-execute old tasks.

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
  `set -o pipefail`. The current `bench-check` uses a pipe-safe script, but
  its committed baseline was captured above load 1; regenerate it on a quiet
  host before using wall-clock results as evidence.
- **Verification and release gates.** Every commit passes the gates in
  `docs/verification.md`; a release runs the full gated set and exists
  only as a tag. There is no release today (no tags).
- **The record:** findings that cost a measurement go into
  `docs/wrong.md`; the entry is the deliverable.

## Gates checklist (run bare, in order, before any commit)

1. `go test ./...`
2. `gofmt -l .` and `go vet ./...`
3. `go test -race ./...`
4. `go test -fuzz=FuzzParseAgainstNetHTTP -fuzztime=15s .` — the root
   compatibility lane may rediscover duplicate Host and is owned by
   HTTP-V1-01; `http1` has committed red-then-green seeds. Read the red,
   never pipe it.
5. Markdown checks: links inside `docs/` resolve; no trailing
   whitespace in touched files.
6. `git diff --stat` and a full read of the diff; commit message in
   the repo style (`docs: ...`).

Deviations and parity closures live in `docs/architecture.md` §2.1
(D1–D10) — when writing about CL+TE, Host, versions, targets, or
framing, check the policy list first and keep the oracle verdicts
probed, not assumed.

## Production task management

- **Local authority.** `docs/roadmap.md` is canonical; the follow-on ledger
  appended to `docs/plans/2026-08-13-simdhttp-production.md` is the only
  staging area for production-readiness tasks; `docs/wrong.md` is the only
  record of rejections. The family index at
  `github.com/sebishogun/simd`'s
  `docs/plans/2026-08-24-simd-family-production-readiness.md` is a link
  collection, noncanonical, and never overrides local truth; it never
  duplicates per-task status.
- **One task ID at a time.** Work executes one task at a time by its ID from
  the ledger (for example `HTTP-V1-01`). A session touching implementation
  work names its task ID in its first message; without one it touches no
  implementation files.
- **State transitions.** Seven states: `open`, `staged`, `in-progress`,
  `blocked`, `evidence-complete`, `shipped`, `rejected`. A transition is an
  edit in the ledger (plus the changelog created by HTTP-V1-05 or
  `docs/wrong.md` for
  `shipped`/`rejected`); `rejected` is terminal without a documented reopen
  condition; historical task text and IDs are never edited for status.
- **Gate rule.** Before any commit: the gate set from `docs/verification.md`,
  run bare (no `tail` without `pipefail`), with explicit timeouts; a hung
  test binary is a leak alarm, not a retry candidate.
