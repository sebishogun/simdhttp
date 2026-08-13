# simdhttp

A request-head parser for HTTP/1.1 built on [simd.go](https://github.com/sebishogun/simd):
classify the bytes a vector register at a time, then walk the boundaries
instead of the bytes — the two-stage shape simdjson proved on JSON,
applied to a format whose structure is even simpler.

**What ships today:** only the borrowed-buffer request-head parser described
below. There is no router, no body framing, no middleware, and no server.
The approved production design ([architecture](docs/architecture.md),
[roadmap](docs/roadmap.md)) is planned, not built.

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
| Body / chunked / trailers | not parsed | Phase 1 |

`CL` + `TE` together still parses at head level: that verdict belongs to
the framing decision table (Phase 1, deviation D7), and the head exposes
`ContentLengthLines` / `TransferEncodingLines` for it.

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

## Roadmap

The approved production target — a net/http-native low-allocation router,
streaming body framing, helpers, and middleware, with safety work staged
first — is specified in [docs/roadmap.md](docs/roadmap.md). Until the
roadmap's Phase 0 lands, treat the parser as internally consistent but not
yet hardened.

## Documentation

- [docs/architecture.md](docs/architecture.md) — shipped surface, gaps
  G1–G8, behavior policy D1–D10, production target.
- [docs/roadmap.md](docs/roadmap.md) — the staged, safety-first plan;
  nothing in it is built yet.
- [docs/lld/router.md](docs/lld/router.md) — router LLD (target).
- [docs/lld/http1-head-parser.md](docs/lld/http1-head-parser.md) — head
  parser LLD (today and target).
- [docs/lld/http1-body-framing.md](docs/lld/http1-body-framing.md) —
  body framing LLD (target).
- [docs/lld/net-http-integration.md](docs/lld/net-http-integration.md) —
  net/http integration LLD (target).
- [docs/verification.md](docs/verification.md) — every gate.
- [docs/wrong.md](docs/wrong.md) — the record of findings that cost
  measurement.
- [docs/plans/2026-08-13-simdhttp-production-design.md](docs/plans/2026-08-13-simdhttp-production-design.md) —
  the approved production design.
- [docs/plans/2026-08-13-simdhttp-production.md](docs/plans/2026-08-13-simdhttp-production.md) —
  the future TDD implementation plan (not to be executed until tasked).
- [AGENTS.md](AGENTS.md) and [CLAUDE.md](CLAUDE.md) — the working rules
  for agents; AGENTS.md is canonical.
