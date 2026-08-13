# Roadmap

Staged, safety first. Each phase ends in a commit with its gates green
(`docs/verification.md`). Nothing below is built yet; phases and gates
are the approved plan. The current tree is the head parser only
(`docs/architecture.md` §1–2).

## Phase 0 — Harden the shipped parser (safety first)

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

## Phase 1 — `simdhttp/http1`: framing and body reader

- Package skeleton, profiles, and the framing decision table
  (body-framing LLD §1, §4).
- Streaming `BodyReader`: fixed-length, then chunked + trailers, then
  drain and limits (LLD §2–3, §5).
- Pipelining contract: `Consumed()` delimits the request; next head
  parse starts at the reported offset (LLD §5).
- Request-smuggling corpus tests (verification §3) become the gate.

**Exit:** framing verdicts match the table against the corpora under
`-race`; no panic/hang fuzz over the reader passes its time budget.

## Phase 2 — Root router

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

## Phase 3 — Helpers, middleware, error adapter

- `Wrap` error adapter with `ErrorMapper` (integration LLD §3).
- simdjson JSON helpers, query/form accessors, `Param` (§4).
- Standard middleware set at the `Use` stack.

**Exit:** `httptest` end-to-end suite green, including hijack/Flusher
assertions and h2/TLS smoke (integration LLD §7).

## Phase 4 — Pipelining polish, seams, gates rework

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
