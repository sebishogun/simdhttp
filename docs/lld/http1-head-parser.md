# LLD: HTTP/1 head parser (`simdhttp` today, `simdhttp/http1` next)

## 1. Current implementation (`parser.go`)

### 1.1 State machine

`Parse(req *Request, b []byte) (consumed int, err error)` runs exactly
three phases and returns at the first failure:

```
request line:
  nl   = IndexByte(b, '\n')                       -- kernel; <0 -> ErrIncomplete
  line = b[:nl]; must end '\r'                    -- else ErrMalformed
  sp1  = IndexByte(line, ' ')                     -- method end; <=0 -> ErrMalformed
  sp2  = IndexByte(line[sp1+1:], ' ') + sp1 + 1   -- target end; <0 -> ErrMalformed
  Method = line[:sp1]; Target = line[sp1+1:sp2]; Proto = line[sp2+1:]
  Target non-empty; Proto == "HTTP/1.0" | "HTTP/1.1"; Method is a token

header block (b[nl+1:]):
  one simd.IndexAll pass fills lineEnds (reused scratch, sized len(block)/2+1;
  simd.IndexAll stops at len(dst), so the size is a bound, not a guess)
  for each line end:
    line must end '\r'                                  -- bare LF -> ErrMalformed
    empty line -> head complete, consumed = nl+1+end+1  -- includes blank line
    colon = bytes.IndexByte(line, ':') (intrinsic); colon <= 0 -> ErrMalformed
    name  = line[:colon]; must be a token (256-entry table, inline)
    value = OWS-trimmed both ends
    control scan: len(value) < 64 -> inline loop; >= 64 -> simd.IndexAnyOrLess
                  kernel; both reject control bytes other than HTAB
    append Header{Name, Value} into req.Headers (caller may pre-size)
  ran out of line ends -> ErrIncomplete
```

### 1.2 Errors and consumed

- `ErrIncomplete` — no terminating blank line yet; `consumed == 0`; the
  caller reads more and calls again with a grown buffer. The prefix loop
  in `parser_test.go` pins this for every prefix of a full head.
- `ErrMalformed` — the bytes cannot be an HTTP/1.x head; `consumed == 0`.
- Success — `consumed` includes the blank line; the request is valid.

Both errors leave `req.Method/Target/Proto/Headers` partially or fully
overwritten; a caller must not use the request after an error.

### 1.3 Aliasing and ownership

- `Method`, `Target`, `Proto`, `Header.Name`, `Header.Value` all alias
  `b`. The caller owns `b` and its lifetime; mutating `b` after `Parse`
  mutates the request.
- `req.lineEnds` and the `Headers` backing array are owned by `req`,
  reused across calls, and grow on demand (one allocation per growth, none
  per call after warmup).
- A `Request` is single-goroutine: `Parse` mutates shared scratch. The
  plan does not change this contract; concurrent servers own one `Request`
  per connection (or per worker), exactly as `bufio.Reader` is owned.

### 1.4 Thresholds and hot loop

- `ctlScanThreshold = 64`: below it an inline loop beats the kernel's call
  boundary; at and above it the kernel wins (sweep-sourced, commit `530ac05`).
- Hot path uses only kernels, intrinsics (`bytes.IndexByte`), and the
  `tokenBytes` table: no dispatch, no allocation, no closures.
- The request line validates the method inline; version check is a
  length + prefix compare.

## 2. Verified gaps (differential, 2026-08-13, Go 1.26.2)

G1–G5 from `docs/architecture.md` §2, with the mechanism:

| gap | mechanism | oracle behavior |
|---|---|---|
| G1 control scan | kernel returns the *first* byte in `\x7f ∪ <0x20`; if it is `\t` the guard passes and scanning stops — later controls unseen | net/http rejects the same value ("malformed MIME header line") |
| G2 duplicate Host | no Host bookkeeping at all | "too many Host headers", even for identical values |
| G3 Host presence/format | no Host code at all | server: "missing required Host header" (HTTP/1.1, server.go), "malformed Host header" (`ValidHostHeader`) |
| G4 target controls | target is only split, never scanned | net/url: "invalid control character in URL" |
| G5 CL/TE framing | headers are opaque name/value pairs | `ReadRequest`: "message cannot contain multiple Content-Length headers" |

G1 is unreachable by the differential fuzz (seeds are short; 15 s smoke
does ~3.9 M execs without growing a ≥ 64-byte value). G2–G5 are reachable
and simply not asserted in the corpus — the corpus has no duplicate Host,
no control-byte target, no double Content-Length.

## 3. Hardened design (`simdhttp/http1`)

### 3.1 Package shape

```
simdhttp/http1
  Parse(req *Request, b []byte, profile Profile) (consumed int, err error)
  Profile  — enum: Compatible, Strict
  Request  — same fields as today + Host *Header, ContentLength *int64,
             TransferEncoding []byte, Connection, Expect accessors,
             Canonical http.Header (merged), Raw []Header (as written)
  Err*     — ErrIncomplete, ErrMalformed, ErrTooLarge, ErrTooMany,
             ErrBadHost, ErrAmbiguousFraming (all wrapping a cause)
```

Keeping `Parse` on the current signature (profile added) preserves the
borrowed-buffer contract and the bench shape.

### 3.2 Limits (both profiles, bounds differ)

- `MaxHeadSize` (compatible 64 KiB, strict 16 KiB)
- `MaxHeaderCount` (compatible 100, strict 50)
- `MaxHeaderValueLen` (compatible 1 MiB, strict 64 KiB)
- `MaxRequestLineLen` (compatible 8 KiB, strict 4 KiB)
- Each limit returns a typed error; the adapter layer maps it to a status.
- Enforced *during* the scan: the `IndexAll` pass bounds the walk, and the
  value scan checks length before the kernel call, so an oversized head
  costs one pass, not an allocation.

### 3.3 G1 fix (control scan)

Replace the hit-and-stop kernel call with a loop that consumes a `\t` hit
and continues:

```
i := simd.IndexAnyOrLess(val[pos:], "\x7f", 0x20)
for i >= 0 {
    if val[pos+i] != '\t' { return ErrMalformed }
    pos += i + 1
    i = simd.IndexAnyOrLess(val[pos:], "\x7f", 0x20)
}
```

Cost: the pathological all-tab long value pays one extra kernel call per
tab run; the common case (no tab) pays exactly what it pays today. The
regression gate for the typical shape is the 8.3% floor plus the
disassembly check; the fix must not reintroduce dispatch.

### 3.4 G2/G3 fix (Host)

In the header loop, case-insensitively match `Host`:

- count occurrences; second occurrence -> `ErrMalformed` (parity with
  "too many Host headers", both profiles);
- HTTP/1.1 request line + zero `Host` lines -> `ErrMalformed` in strict;
  in compatible, surface a typed `ErrMissingHost` the adapter maps to
  400 (net/http's server rejects at read time; a pure parser in
  compatible mode reports, the caller decides);
- format: `httpguts.ValidHostHeader`-equivalent rule — no spaces,
  no controls, brackets balanced for IPv6 literals, no comma in the
  authority — via an inline scan (no `net/http` import in the parser;
  the rule is small and tested against `ValidHostHeader` differentially).

### 3.5 G4 fix (target controls)

One control scan over `Target` (same kernel + tab rule as values, minus
the tab allowance: a tab in a target is a control and is rejected).
URI *semantics* stay with net/url (the fuzz contract in `fuzz_test.go`
stands). In compatible mode a target that net/url would reject is still
accepted by the parser — the adapter's net/url pass rejects it, exactly
as net/http does; in strict mode the parser rejects it directly.

### 3.6 G5 fix (framing fields)

Framing is owned by `http1` (the body layer), not the head parser — see
`docs/lld/http1-body-framing.md` §2–4. The head parser's contribution:
expose `Content-Length` lines and `Transfer-Encoding` lines raw, and
reject a *second* `Content-Length` line at parse time (parity, G5). CL+TE
combination and TE shape are the body layer's verdict.

### 3.7 Profiles

| rule | compatible-default | strict-security |
|---|---|---|
| name token / CRLF / no obs-fold | enforced | enforced |
| duplicate Host | rejected | rejected |
| missing Host (1.1) | `ErrMissingHost` (caller maps) | rejected |
| target control bytes | parser scans; adapter's net/url decides | rejected |
| CL+TE / double CL / non-final TE | rejected (matches Go server) | rejected |
| unknown `Expect` | surfaced, caller decides | rejected |
| limits | 64 KiB / 100 / 1 MiB | 16 KiB / 50 / 64 KiB |
| obs-text (≥ 0x80) in values | accepted (parity) | accepted (parity; RFC 9110 obs-text) |

## 4. Tests

- Differential corpus extended: duplicate Host (identical and differing),
  missing/empty Host on 1.0 and 1.1, control bytes in target, double CL,
  CL+TE, tab-before-control long values at 63/64/65 B and 1 KiB, obs-fold.
- The G1 regression test fails on the current parser and passes after the
  fix (TDD: it is written first).
- Fuzz seeds extended with a ≥ 64-byte value containing `\t` + `\x00`.
- Prefix-incompleteness loop kept; limits added to it (a head at the
  limit boundary must report the limit error, not `ErrIncomplete`).
