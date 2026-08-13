# LLD: HTTP/1 body framing (`simdhttp/http1`)

Owns everything after the head's blank line: message framing, the
streaming `BodyReader`, trailers, drain, and pipelining. Not built yet —
this is the approved design; the roadmap stages it after the head-parser
hardening.

## 1. Framing decision (RFC 9112 §6.3, enforced)

For a request with a non-empty body, the length is determined by, in
order:

1. `Transfer-Encoding` present -> chunked (exactly one TE field with
   exactly the value `chunked`, case-insensitive; anything else —
   including `gzip, chunked` and a second TE field — is rejected, §4,
   matching Go's single-encoding rule);
2. else `Content-Length` present -> fixed length;
3. else the request has no body (`GET`, `HEAD`, `DELETE`-style; RFC
   semantics apply: a `Content-Length: 0` is an empty body, not a
   framing error).

`BodyReader` is constructed from the head's framing fields and the
connection's reader; it owns the remaining bytes of the connection and
reports exactly what it consumed.

## 2. Streaming `BodyReader`

```
type BodyReader struct { ... }               // concrete; no interfaces
func NewBodyReader(br *bufio.Reader, framing Framing, limits Limits) *BodyReader
func (b *BodyReader) Read(p []byte) (n int, err error)  // io.Reader-compatible
func (b *BodyReader) Close() error           // drain or abort, never hang
func (b *BodyReader) Trailers() http.Header  // valid after EOF (chunked)
func (b *BodyReader) Consumed() int64        // bytes read from the connection
```

- **Fixed length**: serves exactly `Content-Length` bytes; EOF is
  `io.EOF` at exactly the declared length; more body bytes than declared
  are *left on the connection* and reported via `Consumed` (pipelining,
  §5). `Content-Length` is parsed to a single int64 with no leading
  `+`, no whitespace, digits only — a malformed value is a framing error.
- **Chunked**: chunk-size line (hex digits, optional extension, CRLF),
  chunk data, CRLF, repeated; terminates at a zero-size chunk followed by
  the trailer section and the final CRLF. Chunk-size line and trailer
  parsing reuse the head parser's line discipline (CRLF required, token
  names, control scan) and limits.
- **Trailers**: parsed with the same rules as headers (name token, value
  control scan, count limit); exposed via `Trailers()`; merged
  canonical + raw forms follow the head parser's contract.
- **`Read` semantics**: never returns `(0, nil)`; a short read followed
  by a later error is legal; after `io.EOF` no further reads. No panic on
  any input; every error path leaves the connection drainable (§6).
- **Hang policy**: every read is bounded by the limits; a connection that
  stops mid-chunk surfaces `io.ErrUnexpectedEOF` or the limit error
  rather than blocking forever, because the caller supplies deadlines
  through the underlying reader. `BodyReader` itself never blocks on the
  network beyond one underlying `Read`.

## 3. Limits

| limit | compatible | strict |
|---|---|---|
| `MaxBodySize` | 32 MiB | 4 MiB |
| `MaxChunkSizeLine` | 8 KiB | 4 KiB |
| `MaxChunkExtensionLen` | 4 KiB | 1 KiB |
| `MaxTrailerCount` | 100 | 50 |
| `MaxTrailerValueLen` | 1 MiB | 64 KiB |
| `MaxDrainSize` | 1 MiB | 256 KiB |

`MaxBodySize` is enforced as bytes served, not bytes allocated: the
reader streams; only the current chunk and trailer section are held.
Exceeding a limit yields a typed error (`ErrBodyTooLarge`,
`ErrChunkLineTooLong`, …) mapped by the caller's server loop to
`413`/`400`.

## 4. Exact ambiguity policy

These verdicts are the contract. The oracle column shows verified Go
1.26.5 behavior for `ReadRequest` and, where they differ, the server.
`reject` always means a framing error from `NewBodyReader`/first
`Read`, never a silent choice. Rows marked **deviation** are
deliberately stricter than `ReadRequest` and are enumerated in
`docs/architecture.md` §2.1 (D6, D7); every other row is parity.

| input | compatible | strict | Go 1.26.5 (verified) |
|---|---|---|---|
| `Content-Length` + `Transfer-Encoding` (**D7**) | reject | reject | ReadRequest deletes CL and frames chunked; the *server* rejects the combination |
| two `Content-Length` lines, equal values (**D6**) | reject | reject | ReadRequest dedupes, accepts |
| two `Content-Length` lines, differing | reject | reject | reject ("message cannot contain multiple Content-Length headers") |
| empty `Content-Length:` | reject | reject | reject ("invalid empty Content-Length") |
| `Transfer-Encoding: gzip, chunked` | reject | reject | reject ("unsupported transfer encoding") — Go supports exactly one TE field with exactly the value `chunked` |
| `Transfer-Encoding: identity` / `gzip` | reject | reject | reject (unsupported) |
| `Transfer-Encoding: chunked` twice | reject | reject | reject ("too many transfer encodings") |
| `Transfer-Encoding: chunked` (single, exact) | chunked body | chunked body | chunked body — parity |
| `Expect: 100-continue` | surfaced; caller may send `100 Continue` | reject unless exactly `100-continue` | honored |
| `Connection: close` | no body framing effect; hop-by-hop list recorded | same | same |
| body bytes beyond `Content-Length` | left on wire, `Consumed` reports | left on wire, `Consumed` reports | consumed as next request |

The policy is a single table-driven function — one place, tested against
the smuggling corpora (`docs/verification.md` §3), so the "two parsers
disagree" class of attack has no second implementation to disagree.

## 5. Pipelining and drain

- `BodyReader` never over-reads past the end of the framed body except
  into its own small buffered chunk. `Consumed()` plus the head's
  `consumed` delimit the request on the wire.
- `Close` before EOF drains up to `MaxDrainSize` bytes of the remaining
  body so the connection can be reused, then reports whether the drain
  completed; a body larger than the drain budget is aborted (connection
  closed by the caller's server loop) — never read unboundedly, never
  hung.
- Pipelined requests: the next head parse starts at the reported
  `Consumed` offset; the parser/reader split exists precisely so the next
  head can be parsed while the previous body drains.

## 6. No panic, no hang

- All chunk-size arithmetic is checked (hex decode into int64 with
  overflow -> framing error); all slicing is bounds-checked by
  construction (lengths come from the same bounded scans).
- Fuzz targets (§`docs/verification.md` §4) run the reader over arbitrary
  byte streams under `-race`; the no-panic/no-hang property is asserted
  with timeouts in tests, not by inspection.
- Error paths are explicitly tested: truncated chunk, oversized chunk
  line, control byte in trailer, EOF in the middle of the final CRLF.

## 7. Hot loop

`Read`'s inner loop is: copy from the chunk buffer (memmove/intrinsic),
advance; chunk-size-line parsing is an inline hex loop over a bounded
line. No interfaces, no closures, no allocation in the serve path; the
`Trailers()`/`Consumed()` accessors are plain field reads. Seam rules and
gates per `docs/architecture.md` §3.5 and `docs/verification.md` §6–7.
