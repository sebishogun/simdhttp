# simdhttp production design

## Goal

Turn the shipped request-head parser into the approved production
library: a root, concrete, low-allocation, net/http-native router with
helpers and middleware, a reusable hardened `simdhttp/http1` head parser
and streaming body reader, and a safety story that comes first. The
design and the plan are approved; nothing in this document is built yet.

## Audience

The primary reader is a Go developer deciding whether and how to use the
package after the production phases land. Secondary readers are
contributors to the parser, router, and framing layers, and maintainers
running the verification gates.

## Current state and the safety record

The shipped surface is exactly the head parser (`docs/architecture.md`
§1): `Parse`, `Request`, `Header`, `ErrIncomplete`, `ErrMalformed`, all
views aliasing the input, one reused scratch slice. The 2026-08-13 audit
verified eight gaps (G1–G8, architecture §2, records in `docs/wrong.md`
§2–6), of which G1 (long-value control scan stops at a tab) is also
invisible to the differential fuzz. The production design's first phase
closes the safety gaps; everything else stacks on top.

## Architecture decisions

### D1. Safety first, framing before routing

Phase order (roadmap): harden the shipped parser, then `http1` framing +
`BodyReader`, then the router, then helpers/middleware, then gates and
seams polish. No router or server work starts while the parser accepts
inputs the origin would reject differently.

### D2. net/http-native, ecosystem-preserving

The router is an `http.Handler`; requests stay `*http.Request`; params
go through `PathValue`/`SetPathValue` (Go ≥ 1.22; go.mod's floor is
1.26.2, the oracle is the 1.26.5 toolchain); no custom context;
`httptest`, HTTP/2, TLS, proxies, observability, and WebSocket hijacking
keep working because nothing is wrapped that does not have to be. The
optional error adapter is opt-in.

### D3. Borrowed-buffer discipline everywhere

The `http1` parser keeps the aliasing contract; `BodyReader` streams and
holds at most one chunk; the router's match walk allocates nothing on
the matched path. Limits are enforced during scans, not after.

### D4. Two profiles

`Compatible` is application-compatible with net/http, not
byte-for-byte-permissive: it matches Go's verdicts except where a
behavior-policy row in `docs/architecture.md` §2.1 (D1–D10 — deviations
D5–D7, D9–D10 deliberately reject ambiguous framing or version forms Go
accepts; D4 and D8 are parity closures) says otherwise.
`Strict` is a documented superset (tighter limits, rejects unusual
Hosts, non-canonical framing fields, unknown expectations). The framing
decision table has one implementation; the strict profile is a filter
over it, never a second parser.

### D5. Hot loops are concrete

No interfaces, no indirect calls, no closures in route matching, head
parsing, or body copying. Dispatch is allowed only at the codec, error,
observability, and future-server seams, and each seam must pass the
disassembly and cycle gates. The shipped parser's discipline (intrinsics
+ inline tables + kernel threshold at 64 B) is the template.

### D6. Custom connection server is deferred

The library adapts to `net/http.Server`. A socket-accepting server is a
non-goal with its own design requirement; the future-server seam exists
so it can be added without touching the router or parser.

## Package layout

```
simdhttp/          root: Router, Build, Handle/HandleFunc, Use,
                   ServeHTTP, Wrap (error adapter), helpers
                   (JSON via simdjson, query/form, Param), middleware
simdhttp/http1/    hardened head parser (Parse + profiles + limits),
                   framing table, BodyReader, trailers, drain
```

The root depends on `github.com/sebishogun/simdjson` and
`github.com/sebishogun/simd`; `http1` depends on simd only. No package
may import `encoding/json`.

## Sources of truth

- Current behavior: `parser.go`, tests, `Makefile`, `go.mod`/`go.sum`.
- History: commits `a60a44b`…`5c2bee2`; `docs/wrong.md` entries 1, 7.
- Verdicts: live differential against net/http on the machine's Go
  1.26.5 toolchain — the oracle is the executable.
- Limits and smuggling policy: RFC 9110/9112 and the Go server's
  enforcement, pinned by the corpora in `docs/verification.md` §3, with
  the behavior-policy rows (deviations and parity closures) in
  `docs/architecture.md` §2.1 (D1–D10).
- Performance: historical amd64/AVX-512 chart (README) plus the gates
  in `docs/verification.md` §6–7; no new claim from shape alone.

## Verification design

`docs/verification.md` is the full gate list: differential tests and
smuggling corpora, fuzz (no panic/hang, differential), route
differential vs `net/http.ServeMux`, race, cross-arch and tier runs,
disassembly and `perf stat` checks with the 8.3% floor, interleaved
minima, load < 1, and bare gates (the `bench-check` pipefail flaw is
recorded, wrong.md §8, and fixed in the gates rework).

## Out of scope

Custom connection server; custom context; interfaces in hot loops;
changing the aliasing/ownership contract; `encoding/json` compatibility;
wildcard hosts; reconfigurable routing after `Build`.
