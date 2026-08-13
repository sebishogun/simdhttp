# Roadmap

Staged, safety first. Each phase ends in a commit with its gates green
(`docs/verification.md`).

**Phases 0-4 are executed.** What each one exited with is recorded under
its heading; the measured numbers below were taken at `34bfc83` on
amd64. What remains is a server loop of this repository's own -- until
then the router runs under `net/http`, which is the arrangement the
end-to-end suite exercises.

## Phase 0 — Harden the shipped parser (safety first) — **done**

Close G1–G6 on the current `Parse` in the `simdhttp` package or its
immediate successor:

- G1: control scan must not stop at an HTAB hit (LLD `http1-head-parser.md` §3.3).
- G2/G3: duplicate Host rejected; HTTP/1.1 Host presence and format
  validation with compatible/strict profiles (LLD §3.4; empty-as-missing
  is deviation D5).
- G4: control bytes and invalid percent-escapes in the request-target
  rejected (D8, LLD §3.5).
- G5: second `Content-Length` rejected at parse time (deviation D6);
  CL+TE verdict owned by the body layer's framing table (D7; LLD
  `http1-body-framing.md` §4).
- Limits: head size, header count, request-line length, value length —
  typed errors, enforced during the scan (LLD §3.2).
- Tests first (TDD, `docs/plans/2026-08-13-simdhttp-production.md`
  Tasks 1–8): the G1 regression test fails on today's parser.
- Fuzz seeds extended so the differential reaches the long-value path.

**Exit:** parser rejects every input in the verification smuggling
corpora that net/http rejects, plus the strict-profile superset; no
regression past the 8.3% floor on the typical shape (measured, not
inferred); `docs/wrong.md` updated.

## Phase 1 — `simdhttp/http1`: framing and body reader — **done**

- Package skeleton, profiles, and the framing decision table
  (body-framing LLD §1, §4).
- Streaming `BodyReader`: fixed-length, then chunked + trailers, then
  drain and limits (LLD §2–3, §5).
- Pipelining contract: `Consumed()` delimits the request; next head
  parse starts at the reported offset (LLD §5).
- Request-smuggling corpus tests (verification §3) become the gate.

**Exit:** framing verdicts match the table against the corpora under
`-race`; no panic/hang fuzz over the reader passes its time budget.

## Phase 2 — Root router — **done**

- Route syntax, precedence, conflicts, immutable `Build` (router LLD
  §2–3).
- 405/Allow, HEAD-from-GET, `OPTIONS *`, trailing-slash policy (§3, §5).
  `OPTIONS *` reaches the router only with
  `DisableGeneralOptionsHandler: true` (or in independent-reader mode):
  the standard server intercepts it; the router's `Allow` behavior is
  asserted by direct `ServeHTTP` tests.
- Params via `SetPathValue`/`PathValue`; host matching (§4, §6).
- Differential vs `net/http.ServeMux` (verification §5).

**Exit:** route differential green on the corpus; match-loop bench holds
the 8.3% floor with zero allocation on the matched path; disassembly
shows no indirect call in the walk.

## Phase 3 — Helpers, middleware, error adapter — **done**

- `Wrap` error adapter with `ErrorMapper` (integration LLD §3).
- simdjson JSON helpers, query/form accessors, `Param` (§4).
- Standard middleware set at the `Use` stack.

**Exit:** `httptest` end-to-end suite green, including hijack/Flusher
assertions and h2/TLS smoke (integration LLD §7).

## Phase 4 — Pipelining polish, seams, gates rework — **done**

- Drain and pipelining edge cases (body-framing LLD §5–6) under the
  smuggling corpora.
- Seam audit: codec/error/observability/future-server dispatch appears
  only where allowed (architecture §3.5); disassembly + `perf stat`
  gates for each seam's hot-path cost.
- Rework the `Makefile` gates: `bench-check` loses its
  `tee`-without-`pipefail` flaw (wrong.md entry, verification §2); add
  cross-arch and tier lanes (verification §8).
- Docs: architecture/LLD/verification updated to the shipped reality;
  plans marked executed.

## Deferred (needs its own design)

- **Custom connection server** — socket loop, keep-alive, timeouts,
  HTTP/2 frames, TLS. Explicitly out of scope (architecture §3.7); the
  future-server seam exists so this can be added without touching the
  router or parser.
- Any change to the aliasing/ownership contract of `Parse`.
- Interfaces or indirect calls in route/parser hot loops.

## Sequence guard

Phases are ordered by dependency, and Phase 0 is a hard prerequisite:
no router or server work starts while the parser accepts inputs the
origin would reject differently. A phase may be reordered only with a
design note in `docs/architecture.md` explaining the new dependency.

## Executed: what each phase actually exited with

Recorded here rather than folded into the phase text above, so the plan
still reads as the plan and this reads as the result.

| phase | exit | evidence |
|---|---|---|
| 0 | G1-G6 closed; `http1` parser with compatible/strict profiles, limits, typed errors | two fuzz-found corpus seeds committed red-then-green; 9.7M executions with no finding |
| 1 | framing table, fixed-length and chunked reader, trailers, limits, bounded drain | every row of the framing table asserted in both profiles; every prefix of a chunked stream asserted to fail; 6.1M and 3.9M fuzz executions |
| 2 | router: immutable build, segment trie, host patterns, 405/HEAD/OPTIONS, trailing slash | differential against `net/http.ServeMux` over 300 generated pattern sets plus 6 fixed; 5 sets skipped and logged. Match 96.9 ns / 0 allocs against ServeMux's 169.4 ns / 2 |
| 3 | error adapter, simdjson JSON, query/form helpers, middleware | 11-test end-to-end suite on a real server: TLS, HTTP/2, hijack, flush, 50-request keep-alive. `Query` 70.2 ns / 0 allocs against `net/url`'s 306.8 ns / 7 |
| 4 | hot-loop disassembly gate, pipefail-safe `bench-check`, cross-arch and tier lanes | 10 hot loops with no indirect call; `bench-check` proved red by halving a baseline row; arm64, s390x, ppc64le, riscv64, loong64 build and vet |

Four divergences from `net/http.ServeMux` that the Phase 2 differential
found, and one dependency fact the Phase 3 test found, are recorded in
`docs/wrong.md` entries 9-13. Each was probed against the reference
before being fixed rather than reasoned about.

## Remaining

- **A server loop.** `http1` can frame a connection end to end -- head,
  body, drain, next head -- but nothing in this repository accepts a
  socket. The router is an `http.Handler` and runs under `net/http`
  today, which is deliberate: it is the arrangement that keeps the
  ecosystem, and a server of our own has to earn its way past that.
- **A quiet-machine baseline.** `testdata/bench.txt` records the load
  average it was captured at (3.76), which is too high for the
  wall-clock floor to mean much. `make bench-baseline` on a quiet
  machine replaces it.
- **The 8.3% floor is inherited, not measured here.** It comes from the
  simd repository's record; this repository has never measured its own
  layout noise (`docs/wrong.md` entry 8).
