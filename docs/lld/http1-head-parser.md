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

## 2. Verified gaps (differential, 2026-08-13, Go 1.26.5)

G1–G5 from `docs/architecture.md` §2, with the mechanism:

| gap | mechanism | oracle behavior |
|---|---|---|
| G1 control scan | kernel returns the *first* byte in `\x7f ∪ <0x20`; if it is `\t` the guard passes and scanning stops — later controls unseen | net/http rejects the same value ("malformed MIME header line") |
| G2 duplicate Host | no Host bookkeeping at all | "too many Host headers", even for identical values |
| G3 Host presence/format | no Host code at all | server: "missing required Host header" (HTTP/1.1), "malformed Host header"; present-but-empty `Host:` accepted |
| G4 target controls and escapes | target is only split, never scanned | net/url: "invalid control character in URL", "invalid URL escape" (`%zz`, `%2`) |
| G5 CL/TE framing | headers are opaque name/value pairs | `ReadRequest` dedupes identical `Content-Length`s and rejects differing ones; with `Transfer-Encoding` present — and on the server too — `Content-Length` is deleted and the request frames chunked (probed: server 200, chunked body) |

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

- `MaxHeadSize` (compatible 2 MiB, strict 256 KiB)
- `MaxHeaderCount` (compatible 100, strict 50)
- `MaxHeaderValueLen` (compatible 1 MiB, strict 64 KiB)
- `MaxRequestLineLen` (compatible 8 KiB, strict 4 KiB)

`MaxHeadSize` deliberately exceeds `MaxHeaderValueLen` so that a
single oversized value reaches the value-limit error and is not
pre-empted by the head-size check. Each limit returns a typed error;
the caller's server loop maps it to a status. Enforced *during* the
scan: the `IndexAll` pass bounds the walk, and the value scan checks
length before the kernel call, so an oversized head costs one pass,
not an allocation.

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
- HTTP/1.1 request line with zero `Host` lines, or with an empty
  `Host:` value, -> `ErrMalformed` in strict; in compatible, surface a
  typed `ErrMissingHost` (400). Treating empty as missing is an
  enumerated deviation (D5): Go's server accepts a present-but-empty
  `Host:` (probed, 200 OK);
- format: the **simdhttp host rule**, deliberately stricter than Go's.
  Go's `httpguts.ValidHostHeader` is a pure byte-table scan — it
  allows `,` (sub-delims) and has no bracket-balance logic (probed:
  `Host: a.com,b.com` and unbalanced `Host: [::1` both accepted by the
  Go server). The simdhttp rule is: no space, tab, control, or comma
  anywhere in the authority; `[` requires a matching `]`, and `]` a
  preceding `[`; empty value handled as above — via an inline scan (no
  `net/http` import in the parser; the rule is small and its cases are
  asserted in tests against the probed Go verdicts, with the comma and
  bracket rows pinned as deviations D9, not parity).

### 3.5 G4 fix (target controls and escapes)

One control scan over `Target` (same kernel + tab rule as values, minus
the tab allowance: a tab in a target is a control and is rejected),
plus a percent-escape check: every `%` must be followed by two hex
digits. Both profiles reject controls and invalid escapes — parity with
`ReadRequest`, which rejects them via net/url (D8). URI *semantics*
beyond that stay with net/url (the fuzz contract in `fuzz_test.go`
stands): a well-formed but semantically odd target (e.g. an absolute
URI) is the caller's business.

### 3.6 G5 fix (framing fields)

Framing is owned by `http1` (the body layer), not the head parser — see
`docs/lld/http1-body-framing.md` §2–4. The head parser's contribution:
expose `Content-Length` lines and `Transfer-Encoding` lines raw, and
reject a *second* `Content-Length` line at parse time. That rejection is
an enumerated deviation (D6): Go's `ReadRequest` dedupes identical
`Content-Length` values and only rejects differing ones; simdhttp
rejects every duplicate in both profiles. The CL+TE combination is the
body layer's framing-table verdict (D7 — a deviation from both Go's
reader and server); the TE-shape rule (exactly one field, exactly
`chunked`) is parity with Go's single-encoding rule, not D7.

### 3.7 Profiles

| rule | compatible-default | strict-security |
|---|---|---|
| name token / CRLF / no obs-fold | enforced (D1–D3) | enforced |
| version exactly `HTTP/1.0`/`HTTP/1.1` | enforced (D10: deliberate; Go accepts any `HTTP/X.Y` and the h2 preface) | enforced |
| duplicate Host | rejected (D4, parity closure) | rejected |
| missing or empty Host (1.1) | `ErrMissingHost` (caller maps to 400; D5) | rejected |
| target controls and invalid escapes | rejected (D8, parity closure) | rejected |
| duplicate CL (any) | rejected (D6) | rejected |
| CL+TE | rejected (D7) | rejected |
| TE exactly `chunked` | enforced (parity with Go's single-encoding rule) | enforced |
| unknown `Expect` | surfaced, caller decides | rejected |
| limits | 2 MiB / 100 / 1 MiB | 256 KiB / 50 / 64 KiB |
| obs-text (≥ 0x80) in values | accepted (parity) | accepted (parity; RFC 9110 obs-text) |

## 4. Tests

- Differential corpus extended: duplicate Host (identical and differing),
  missing/empty Host on 1.0 and 1.1, target controls and invalid escapes
  (`%zz`, `%2`), duplicate CL (identical and differing), CL+TE, TE
  `gzip, chunked`, tab-before-control long values at 63/64/65 B and
  1 KiB, obs-fold, comma and bracket Hosts. Deviation rows (D5–D9) are
  asserted one-directionally — simdhttp rejects, and the Go verdict is
  recorded from the probed oracle — never as parity.
- The G1 regression test fails on the current parser and passes after the
  fix (TDD: it is written first).
- Fuzz seeds extended with a ≥ 64-byte value containing `\t` + `\x00`.
- Prefix-incompleteness loop kept; limits added to it (a head at the
  limit boundary must report the limit error, not `ErrIncomplete`).
