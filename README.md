# simdhttp

An HTTP/1.1 request-head parser, body reader and router built on
[simd.go](https://github.com/sebishogun/simd): classify the bytes a vector
register at a time, then walk the boundaries instead of the bytes — the
two-stage shape simdjson proved on JSON, applied to a format whose structure
is even simpler. The router is a plain `http.Handler`, so it slots under
`net/http` and keeps the ecosystem that comes with it.

**What ships today:**

| surface | package | state |
|---|---|---|
| request-head parser, borrowed buffers | `simdhttp` | shipped |
| strict/compatible head parser with profiles | `simdhttp/http1` | shipped |
| body framing decision table | `simdhttp/http1` | shipped |
| fixed-length and chunked body reader with trailers, limits, drain | `simdhttp/http1` | shipped |
| router: immutable build, segment trie, host patterns, 405/HEAD/OPTIONS | `simdhttp` | shipped |
| error adapter, simdjson JSON, query/form helpers, middleware | `simdhttp` | shipped |
| a server loop | — | not built; the router runs under `net/http` |

The router is checked against `net/http.ServeMux` by a differential over
generated pattern sets: same route, same bound values, same status, the
`Allow` string byte for byte, and the redirect `Location`. Four divergences
it found are recorded in [docs/wrong.md](docs/wrong.md) entries 9-12.

## The router

```go
r := simdhttp.New()
r.HandleFunc("GET", "/users/{id}", func(w http.ResponseWriter, req *http.Request) {
	fmt.Fprint(w, req.PathValue("id"))
})
r.HandleFunc("GET", "/static/", serveFiles) // a trailing slash is a subtree
if err := r.Build(); err != nil {           // conflicts are errors, never panics
	log.Fatal(err)
}
http.ListenAndServe(":8080", r)
```

Registration mutates and never panics; `Build` compiles the trie, assigns
dense method IDs, folds the middleware chain in, and freezes the router.
After that the table is read-only, so any number of goroutines serve from it
with no lock and no atomic — and a conflict is a build error rather than a
request-time surprise. `MustBuild` is the one panic path, for tests and
top-level setup.

Patterns are ServeMux's: `{name}` matches one non-empty segment, `{$}` marks
the end of a path, a trailing slash matches the subtree, and `*` is the named
form of the same thing. Handlers stay `http.Handler`, requests stay
`*http.Request`, and parameters go through the standard
`SetPathValue`/`PathValue` — no context key, no wrapper type.

| | simdhttp | net/http.ServeMux | |
|---|---:|---:|---|
| 4-segment route, 2 params, 100k routes | **96.9 ns, 0 allocs** | 169.4 ns, 2 allocs | 1.75× |
| static route, 1k routes | **86.5 ns, 0 allocs** | 119.8 ns, 0 allocs | 1.39× |
| `Query` accessor | **70.2 ns, 0 allocs** | 306.8 ns, 7 allocs | 4.4× |

Measured on amd64 at `34bfc83`; the allocation counts are exact and the
latencies were taken on a machine that was not quiet. `make bench-baseline`
on a quiet machine is the reproduction.

## The parser

```go
var req simdhttp.Request
req.Headers = make([]simdhttp.Header, 0, 16) // reused across calls
n, err := simdhttp.Parse(&req, buf)
```

`Parse` reads exactly one request head — the request line and the header
block through the terminating blank line — from the start of `buf`.

- `Request` carries `Method`, `Target`, `Proto`, and `Headers`.
- Every field aliases the input buffer; after warmup, `Parse` allocates
  nothing for heads no larger than the largest one parsed so far — the
  `lineEnds` scratch and the `Headers` backing array grow on demand when
  a larger head arrives, and are then reused. The caller owns the bytes
  and their lifetime.
- `Header.Name` is the name as written (not canonicalized); `Header.Value`
  is the value with optional whitespace trimmed.
- `consumed` counts bytes through the terminating blank line.
- `ErrIncomplete` means the head is not terminated yet: read more and call
  again. `ErrMalformed` means the bytes cannot be an HTTP/1.x head. On
  either error, `consumed` is 0 and the request must not be used.
- A `Request` is not safe for concurrent use: `Parse` reuses its scratch.

The version must be exactly `HTTP/1.0` or `HTTP/1.1` — a deliberate
restriction (deviation D10: Go's reader accepts any single-digit
`HTTP/X.Y`, and the server also accepts the `PRI * HTTP/2.0` preface).
Methods and header names must be RFC 9110 tokens; header values may not
contain control bytes other than HTAB; and line endings must be CRLF.
Request-target semantics are the caller's job (net/url), not this
package's — today the parser does not even check the target for control
bytes or invalid percent-escapes (see the gap table below).

## Contract with net/http

Accepted heads are checked against `net/http`'s `ReadRequest` on a corpus
and under differential fuzz: anything the standard reader rejects (within
the documented scope — net/url target semantics excluded), this rejects,
and accepted requests carry byte-identical fields with OWS trimmed. (The
test's random-mutation loop is a no-panic smoke, not a verdict comparison:
it never asserts an outcome.) Where simdhttp is deliberately stricter than
Go — bare-LF line endings, spaces inside field names, obs-fold
continuations, and the future Host/CL policy — each deviation is
enumerated in the canonical list, [docs/architecture.md §2.1](docs/architecture.md).

## Safety and compatibility: `http1` vs the root package

Phase 0 of the production plan shipped `simdhttp/http1` — the hardened
parser. **New work should use `http1.Parse`**; the root `Parse` is
unchanged for compatibility and still carries every gap below.

`http1.Parse(req, b, profile)` takes a profile (`Compatible` mirrors
net/http's verdicts, `Strict` is the documented superset) and closes the
Phase 0 gaps:

| gap | root package | `http1` |
|---|---|---|
| Long header-value control bytes | a value ≥ 64 bytes whose first HTAB precedes a control byte slips the scan | **rejected** — the scan continues past each tab; the no-tab case still costs one kernel call |
| Duplicate `Host` | accepted, even an identical one | **rejected** (parity: "too many Host headers") |
| Missing / malformed `Host` | accepted | **rejected** — 1.1 requires a non-empty Host; the format check is stricter than `httpguts` on commas and bracket balance (D9), and a present-but-empty `Host:` counts as missing (D5) |
| Request-target controls and escapes | accepted | **rejected** — control bytes and non-hex percent-escapes (D8, parity with net/url) |
| Duplicate `Content-Length` | accepted | **rejected** in both profiles — stricter than Go, which dedupes identical values (D6) |
| `Transfer-Encoding` | accepted unvalidated | duplicate **rejected** (parity), and the value must be exactly `chunked` — empty, `gzip`, `identity` and lists are rejected, as net/http rejects them. `ValidTransferEncoding` is the single implementation the framing table will share |
| Limits | none | head size, request line, header count and value length per profile, each with its own typed error, never `ErrIncomplete` |
| Body / chunked / trailers | not parsed | **shipped** — `FramingOf` decides, `NewBodyReader` serves fixed-length and chunked bodies with trailers, per-profile limits, a bounded drain and a pipelining contract |

`CL` + `TE` together still parses at head level: that verdict belongs to
the framing decision table (deviation D7), which reads the head's
`ContentLengthLines` / `TransferEncodingLines` and rejects the pair. One
function decides body framing, because two implementations of that policy is
exactly what a smuggled request needs — one component frames the body one
way, the next frames it another, and the bytes between the two readings are
a request nobody inspected.

The differential fuzz asserts the one-direction contract — never accept
what net/http rejects. Two committed corpus seeds pin what it found:
`4cb4ee00bf74f878` (duplicate `Host`, the G2 record) and
`b073e10c2a865463` (empty `Transfer-Encoding`, found while hardening).
Both are red on the unfixed parser and green now, so a fresh clone
reproduces rather than starting green. The last smoke ran 9.7M
executions with no finding. The committed corpus also covers the shapes
short seeds never grow into — the ≥ 64-byte value with a tab before a
NUL among them.

## Speed (historical)

![benchmarks](docs/bench.svg)

The chart and table are **historical measurements from August 2026 on
amd64/AVX-512** — this machine, this toolchain, the code at commit `5c2bee2`.
They are evidence for the sweep-then-fix story below, not a compatibility
promise for any other machine.

Two provenance notes on the frozen SVG (`docs/bench.svg` cannot change):

- the quoted numbers are the **min-of-three** runs recorded with the
  original README (`a60a44b` measured the typical head; `530ac05` shipped
  the four-shape table). The SVG and the `-count=6` `bench` Make target
  landed **together** in `daf5fdc`; the caption's "minimum of six"
  contradicts the min-of-three source runs it was drawn from, and the
  `-count=6` target is a reproduction command — neither is the source
  of the quoted numbers;
- the SVG rounds ratios for display: the typical row's 1.05× renders as
  "1.1×". The **table above is authoritative**; `make bench` (one process,
  shuffled, `-count=6`) is today's reproduction command, not the source
  of the quoted numbers, and may differ on any machine or day.

| shape | simdhttp | net/http | |
|---|---|---|---|
| typical 9-header | ~1,400 | ~1,470 | 1.05× |
| 100 headers | **2,219** | 10,491 | **4.7×** |
| giant header value | **8,032** | 27,893 | **3.5×** |

The 100-header row is the sweep-then-fix story: a first cut lost it 1.3×
because per-header work called kernels on sub-threshold slices where
dispatch dominates. One `IndexAll` pass finds every line end; the colon and
short-line validation are then compiler intrinsics and inline loops, no
dispatch. The giant-value win is aliasing — net/http copies the value, this
returns a sub-slice. The fix reversed the typical shape from 1.22× to 1.05×
(the record is in [docs/wrong.md](docs/wrong.md)).

Pure Go, no cgo in the dependency path: simd ships its kernels as committed
assembly, so this is an ordinary `go get`.

## Gates

```
make check     # gofmt, vet, tests, race, scalar tier, purego, hot loops, 5 architectures
make verify    # check, plus fuzz smokes and bench-check
```

`hot-loops-check` fails if any hot loop disassembles with an indirect call;
ten functions across both packages are covered. `Router.ServeHTTP` is
excluded on purpose — its indirect calls are `w.Header`, `w.WriteHeader` and
the handler handoff, which are calls through `http.ResponseWriter` and
`http.Handler` and are the API this package exists to fit. `bench-check`
compares against a committed baseline with no pipe carrying the verdict;
the previous target could not fail, which is
[docs/wrong.md](docs/wrong.md) entry 8.

## Roadmap

Remaining: an independent server loop, and the `http1` reader as a complete
alternative to `net/http`'s. Staged in [docs/roadmap.md](docs/roadmap.md).

## Documentation

- [docs/architecture.md](docs/architecture.md) — shipped surface, gaps
  G1–G8, behavior policy D1–D10, production target.
- [docs/roadmap.md](docs/roadmap.md) — the staged plan; Phases 0-4 are
  executed, the server loop is not.
- [docs/lld/router.md](docs/lld/router.md) — router LLD (shipped).
- [docs/lld/http1-head-parser.md](docs/lld/http1-head-parser.md) — head
  parser LLD (today and target).
- [docs/lld/http1-body-framing.md](docs/lld/http1-body-framing.md) —
  body framing LLD (shipped).
- [docs/lld/net-http-integration.md](docs/lld/net-http-integration.md) —
  net/http integration LLD (shipped, less the server loop).
- [docs/verification.md](docs/verification.md) — every gate.
- [docs/wrong.md](docs/wrong.md) — the record of findings that cost
  measurement.
- [docs/plans/2026-08-13-simdhttp-production-design.md](docs/plans/2026-08-13-simdhttp-production-design.md) —
  the approved production design.
- [docs/plans/2026-08-13-simdhttp-production.md](docs/plans/2026-08-13-simdhttp-production.md) —
  the TDD implementation plan; Phases 0-4 executed.
- [AGENTS.md](AGENTS.md) and [CLAUDE.md](CLAUDE.md) — the working rules
  for agents; AGENTS.md is canonical.
