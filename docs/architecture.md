# Architecture

## 1. What exists today

The module `github.com/sebishogun/simdhttp` (go.mod declares `go 1.26.2`;
the oracle for every behavior claim in this document is the Go 1.26.5
toolchain actually run; depends only on `github.com/sebishogun/simd`
v1.20.0, no cgo) ships exactly one thing: a borrowed-buffer HTTP/1
request-head parser.

**Exported surface** (`parser.go`):

- `Request{Method, Target, Proto, Headers []Header}` — all views alias the
  input buffer; `lineEnds []int32` is unexported scratch reused across
  `Parse` calls on the same `Request`.
- `Header{Name, Value []byte}` — name as written, value OWS-trimmed.
- `Parse(req *Request, b []byte) (consumed int, err error)` — one head from
  the start of `b`; `consumed` includes the terminating blank line; any
  error returns `consumed == 0`.
- `ErrIncomplete` (read more and call again), `ErrMalformed`.

**Parser shape:** request line split on two spaces (`Method`, `Target`,
`Proto`); version must be exactly `HTTP/1.0` or `HTTP/1.1`; method and
header names must be RFC 9110 tokens (256-entry inline table); one
`simd.IndexAll` pass finds every line end in the header block into the
reused scratch, then each line is split at the colon (compiler intrinsic),
OWS-trimmed, and control-scanned — inline below 64 bytes, `simd.IndexAnyOrLess`
kernel at 64 bytes and above (`ctlScanThreshold`).

**Strictness vs net/http** — every deliberate difference from Go's
reader is enumerated in the canonical deviations list (§2.1); today
there are three, all request-smuggling surface: space inside a field
name, bare-LF line endings, and obs-fold continuation lines are
malformed (net/http tolerates or joins all three).

**Tests and gates:** `TestParseAgainstNetHTTP` (corpus, prefix
incompleteness, 5,000 mutations), `FuzzParseAgainstNetHTTP` (one-direction
differential: never accept what net/http rejects), `BenchmarkParse` and
`BenchmarkSweep` (none / typical-9 / many-100 / giant-value). Makefile
targets `test`, `vet`, `bench`, `bench-check`.

## 2. Verified gaps in the shipped parser

All confirmed by live differential against net/http on the Go 1.26.5
toolchain, 2026-08-13. Full records: `docs/wrong.md`; LLD
`docs/lld/http1-head-parser.md`.

| # | gap | consequence |
|---|---|---|
| G1 | long header value (≥ 64 B) whose first HTAB precedes a control byte: the kernel's single hit is the tab, the guard passes, the scan stops | NUL/DEL after a tab in a long value accepted |
| G2 | duplicate `Host` accepted, even identical | differs from net/http ("too many Host headers") |
| G3 | no `Host` presence, emptiness, or format validation | HTTP/1.1 heads without Host, `Host:`, malformed hosts accepted; net/http's server rejects missing and malformed Host (present-but-empty `Host:` is accepted by Go) |
| G4 | control bytes (NUL, DEL, …) and invalid percent-escapes (`%zz`, `%2`) in the request-target accepted | net/http rejects via net/url |
| G5 | no framing validation: `Content-Length` + `Transfer-Encoding`, or duplicate `Content-Length`s, accepted | differs from net/http; smuggling surface when a server is added |
| G6 | no limits: head size, header count, value length unbounded | resource exhaustion against any server built on it |
| G7 | no body, chunked, trailers, drain, pipelining | the parser stops at the blank line, by design — but nothing beyond it exists either |
| G8 | no router, helpers, middleware, server | the package is a primitive, not a front door |

G1 is additionally invisible to the differential fuzz: its short seeds do
not grow ≥ 64-byte values, so the case never reaches the oracle.

## 2.1 Canonical deviations from Go 1.26.5 (current and future)

This is the single list of deliberate differences between simdhttp and
Go's reader (plus, where noted, the Go server). Every other verdict is
parity: both accept or both reject. "compatible" and "strict" refer to
the future `http1` profiles; current `simdhttp.Parse` behavior is shown
too. Each row is asserted one-directionally in the differential tests
(simdhttp rejects; Go may accept), never as a parity claim.

| # | deviation | Go 1.26.5 behavior (verified) | current | compatible | strict |
|---|---|---|---|---|---|
| D1 | bare-LF line endings malformed | ReadRequest tolerates | rejects | rejects | rejects |
| D2 | space inside a field name malformed (name must be a token) | ReadRequest tolerates | rejects | rejects | rejects |
| D3 | obs-fold continuation lines malformed | ReadRequest joins folded lines | rejects | rejects | rejects |
| D4 | duplicate `Host` rejected | any second Host line rejected ("too many Host headers") — parity | accepts (gap G2) | rejects | rejects |
| D5 | HTTP/1.1 `Host` presence required; empty treated as missing | server rejects missing/malformed; accepts present-but-empty `Host:` | accepts all (gap G3) | `ErrMissingHost` for absent or empty | rejects |
| D6 | duplicate `Content-Length` rejected, even identical | ReadRequest dedupes identical values, rejects differing ones | accepts all (gap G5) | rejects | rejects |
| D7 | `Content-Length` + `Transfer-Encoding` rejected | ReadRequest deletes CL and frames chunked; the *server* rejects the combination | accepts both (gap G5) | rejects | rejects |
| D8 | request-target controls and invalid percent-escapes rejected | ReadRequest rejects via net/url — parity | accepts (gap G4) | rejects | rejects |
| D9 | Host format stricter than Go's: no comma, balanced brackets | Go's `ValidHostHeader` is a byte-table scan: allows comma, has no bracket-balance logic (probed: `Host: a.com,b.com` and `Host: [::1` both accepted) | accepts all (gap G3) | rejects | rejects |

Version: both sides accept exactly `HTTP/1.0` and `HTTP/1.1` — parity,
not a deviation. D1–D3 ship today; D4–D9 are the future `http1`
profiles. "Compatible" is application-compatible, not
byte-for-byte-permissive: it may reject ambiguous or security-sensitive
forms Go accepts (D5–D7), and every such deviation is enumerated here.

## 3. Approved target

The production architecture below was approved as the design direction.
Everything in it is future work (see `docs/roadmap.md` and
`docs/plans/2026-08-13-simdhttp-production*.md`).

### 3.1 Root package: `simdhttp` — the router and helpers

- A **root, concrete, low-allocation, net/http-native router**:
  `type Router struct{...}` with `func (r *Router) Handle(method, pattern string, h http.Handler)`-style registration and
  `func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request)`.
  Registration builds an immutable route table; `ServeHTTP` never rebuilds.
- **Optional error endpoint adapter**: a small wrapper that converts a
  handler's returned error into an HTTP status + body, `http.Error`-style.
- **simdjson JSON helpers**: encode/decode/query bindings built on
  `github.com/sebishogun/simdjson`, not on `encoding/json`.
- **Query/form helpers**: borrowed-buffer accessors over
  `req.URL.Query()` / parsed forms.
- **Standard middleware**: `func(http.Handler) http.Handler` compositions,
  the ecosystem's one shape.
- **Ecosystem preservation, unchanged**: handlers remain `http.Handler`;
  requests stay `*http.Request` (no custom context, no wrapper struct);
  responses write through the plain `http.ResponseWriter`; route params go
  through `Request.SetPathValue`/`PathValue` (exported since Go 1.22), so
  `httptest`, HTTP/2, TLS, reverse proxies, observability middleware, and
  WebSocket hijacking continue to work untouched.

### 3.2 `simdhttp/http1`: hardened head parser + streaming body reader

A reusable subpackage with the borrowed-buffer discipline of the current
parser:

- the **hardened head parser** — the shipped `Parse` plus the G1–G6 fixes
  and limits, behind **two profiles**:
  - *compatible-default*: application-compatible with net/http — every
    deviation from Go is enumerated in §2.1 (D5–D7 deliberately reject
    ambiguous forms Go accepts) — tolerant of real-world clients;
  - *strict-security*: additionally rejects anything a front door should
    not pass: unusual Hosts, non-canonical framing fields, control bytes
    everywhere, oversized heads.
- a **streaming `BodyReader`** — fixed-length and chunked bodies, trailers,
  drain, limits, EOF semantics, pipelining (leftover bytes reported, never
  swallowed), no panic, no hang, bounded memory.

### 3.3 Validation matrix

Both profiles validate, at the layer that owns the field:

| field | validation |
|---|---|
| `Host` | presence (HTTP/1.1; empty treated as missing, D5), single occurrence, simdhttp host rule (D9: no comma, balanced brackets) |
| request-target | control bytes rejected (D8); URI *semantics* remain the caller's (net/url), matching the fuzz contract |
| URI escapes | invalid `%` escapes rejected (D8); well-formed escapes are decoded by the caller, not rewritten |
| `Content-Length` | digits only, single value, parsed exactly; any duplicate rejected (D6) |
| `Transfer-Encoding` | exactly one field, exactly `chunked` (case-insensitive) — the single encoding Go supports; anything else rejected (D7) |
| `Connection` | parsed and exposed for hop-by-hop handling, not acted on by the parser |
| `Expect` | only `100-continue` recognized; unknown expectations rejected in strict profile, surfaced in compatible |
| trailers | chunked trailers parsed with the same control-byte rules as headers, bounded |

### 3.4 Request-smuggling policy

The framing layer applies the exact ambiguity policy in
`docs/lld/http1-body-framing.md` §4. In short: `Content-Length` and
`Transfer-Encoding` together is always a rejection (D7 — a deviation
from `ReadRequest`, which deletes CL and frames chunked; parity with
the Go server); any duplicate `Content-Length` is a rejection even when
the values agree (D6 — a deviation: Go dedupes identical values);
`Transfer-Encoding` must be exactly one field with exactly the value
`chunked` (matching Go's single-encoding rule); and the compatible
profile matches the Go server's verdicts where Go's reader and server
disagree, while the strict profile is a superset of both.

### 3.5 Hot-loop discipline

- **No interfaces, no indirect calls, no closures through `go:linkname`,
  in route matching or parser/body hot loops.** The route table is a
  concrete structure walked with concrete types; the parser uses
  intrinsics and inline tables as it does today.
- **Allowed seams**, and the only places that may dispatch:
  codec (value parsing), error handling, observability hooks, and the
  future custom connection server. Each seam must pass the disassembly and
  cycle checks (`docs/verification.md` §6–7) before it lands.

### 3.6 Resource bounds

Every reader and the parser are bounded: maximum head size, maximum header
count, maximum header-value length, maximum body size, maximum chunk size,
maximum chunk-extension length, maximum trailer count. Exceeding a bound
produces an error or a 400/413-class response, never a panic, never a
hang, and the connection state must stay drainable.

### 3.7 Non-goals

- **Custom connection server: deferred** — no socket-accepting server,
  no HTTP/2 frame handling, no TLS termination. The router adapts to
  `net/http.Server` (and, via the preserved interfaces, to any
  `http.Handler`-based framework). A custom server requires its own design
  document and the "future server" seam.
- **Custom context** — never.
- **`encoding/json` compatibility layer** — the simdjson helpers are
  additive; handlers that need `encoding/json` keep using it.

## 4. Sources of truth

Every claim in this document traces to: `parser.go`, the test files, the
`Makefile`, `go.mod`/`go.sum`, `git log` (commits `a60a44b` … `5c2bee2`),
the simd module in `GOMODCACHE` (`IndexAll` stops at `len(dst)`; the
parser's scratch sizing is therefore bounded), and the live differential
runs of 2026-08-13 on the Go 1.26.5 toolchain. Nothing in the target
sections is measured yet — they are design, and the plans mark each item
with its verification gate.
