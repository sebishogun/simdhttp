# simdhttp

HTTP/1.1 request-head parsing built on [simd.go](https://github.com/sebishogun/simd):
classify the bytes a vector register at a time, then walk the boundaries
instead of the bytes — the two-stage shape simdjson proved on JSON,
applied to a format whose structure is even simpler.

```go
var req simdhttp.Request
req.Headers = make([]simdhttp.Header, 0, 16) // reused across calls
n, err := simdhttp.Parse(&req, buf)
```

Every field aliases the input buffer, so parsing allocates nothing. The
caller owns the bytes.

## Contract

Anything `net/http`'s reader rejects, this rejects, and accepted requests
carry byte-identical fields with optional whitespace trimmed — checked
against `net/http` on a corpus and under mutation.

In two places it is deliberately **stricter**, both request-smuggling
surface: a space inside a header field name, and bare-LF line endings.
The standard reader tolerates both; this requires a token name and CRLF.

## Speed

Four shapes, minimum of three on amd64/avx512, ns per head:

| shape | simdhttp | net/http | |
|---|---|---|---|
| typical 9-header | ~1,400 | ~1,470 | 1.05× |
| 100 headers | **2,219** | 10,491 | **4.7×** |
| giant header value | **8,032** | 27,893 | **3.5×** |

The 100-header row is the sweep-then-fix story: a first cut lost it 1.3x
because per-header work called kernels on sub-threshold slices where
dispatch dominates. One IndexAll pass finds every line end; the colon and
short-line validation are then compiler intrinsics and inline loops, no
dispatch. The giant-value win is aliasing -- net/http copies the value,
this returns a sub-slice.

Pure Go, no cgo: simd ships its kernels as committed assembly, so this is
an ordinary `go get`.
