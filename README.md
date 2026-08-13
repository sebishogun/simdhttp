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
- Every field aliases the input buffer; parsing allocates nothing after the
  first call, when one scratch slice (`lineEnds`) and the `Headers` backing
  array are sized. The caller owns the bytes and their lifetime.
- `Header.Name` is the name as written (not canonicalized); `Header.Value`
  is the value with optional whitespace trimmed.
- `consumed` counts bytes through the terminating blank line.
- `ErrIncomplete` means the head is not terminated yet: read more and call
  again. `ErrMalformed` means the bytes cannot be an HTTP/1.x head. On
  either error, `consumed` is 0 and the request must not be used.
- A `Request` is not safe for concurrent use: `Parse` reuses its scratch.

The version must be exactly `HTTP/1.0` or `HTTP/1.1`; methods and header
names must be RFC 9110 tokens; header values may not contain control bytes
other than HTAB; and line endings must be CRLF. Request-target semantics are
the caller's job (net/url), not this package's.

## Contract with net/http

Accepted heads are checked against `net/http`'s `ReadRequest` on a corpus,
under random mutation, and under differential fuzz: anything the standard
reader rejects, this rejects, and accepted requests carry byte-identical
fields with OWS trimmed. In three places it is deliberately **stricter**,
all request-smuggling surface:

- a space inside a header field name (the standard reader tolerates it);
- bare-LF line endings (the standard reader tolerates them);
- obs-fold continuation lines (the standard reader joins them).

## Known safety and compatibility gaps

Verified against the source and net/http on 2026-08-13 (Go 1.26.2). None of
these are fixed yet; the current parser is a parsing primitive, not a
hardened front door. Do not put this parser in front of an origin without
addressing them.

| gap | what happens today | net/http |
|---|---|---|
| Long header-value control bytes | a value ≥ 64 bytes whose first HTAB precedes a control byte (NUL, DEL, …) slips the control scan — the kernel returns the tab, the guard passes, the scan stops | rejects |
| Duplicate `Host` | any second `Host` line is accepted, even an identical one | rejects ("too many Host headers") |
| Missing / malformed `Host` | an HTTP/1.1 head with no `Host`, an empty `Host:`, or a malformed host is accepted | server rejects ("missing required Host header", "malformed Host header") |
| Request-target controls | NUL, DEL and other control bytes in the target are accepted | rejects (net/url control-character check) |
| `Content-Length` framing | `Content-Length` + `Transfer-Encoding` together, or two differing `Content-Length`s, are accepted | rejects multiple `Content-Length` |
| Body / chunked / trailers | not parsed at all — no framing, no `BodyReader` | full framing |
| Limits | none: unbounded head size, header count, value length | bounded by `MaxHeaderBytes` and the 10 MB field-value cap |

The differential fuzz's one-direction contract (never accept what net/http
rejects) has held for 35M+ executions, but it cannot pass judgment it never
reaches: the long-value case above needs a ≥ 64-byte value that its short
seeds do not grow into.

## Speed (historical)

![benchmarks](docs/bench.svg)

The chart and table are **historical measurements from August 2026 on
amd64/AVX-512** — this machine, this toolchain, the code at commit `5c2bee2`.
They are evidence for the sweep-then-fix story below, not a compatibility
promise for any other machine. Reproduce with `make bench` (one process,
shuffled, minimum of six).

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
