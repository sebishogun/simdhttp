# LLD: net/http integration and boundaries

How the approved pieces fit into the net/http ecosystem, and where
simdhttp deliberately stops. Not built yet.

## 1. The adaptation layer

`simdhttp` ships as ordinary `http.Handler` values; there is no custom
server in v1 and no fork of net/http.

```
net/http.Server ──► simdhttp.Router.ServeHTTP ──► route handlers
                        │
                        └─► simdhttp/http1.Parse + BodyReader  (when the
                            server delegates head/body reading)
```

Two modes of use:

1. **Handler mode (default).** The router is registered on a standard
   `http.Server`; net/http does the connection loop, head reading, and
   body framing with its own `maxHeaderBytes` etc. simdhttp's hardened
   parser is *not* in this path; it is used for the pieces net/http
   does not optimize. This mode is the compatibility floor: everything
   works today, nothing about the request lifecycle changes.
2. **Framing mode (roadmap Phase 1+).** A connection wrapper hands the
   bytes to `simdhttp/http1.Parse` + `BodyReader` and constructs the
   `*http.Request` from parsed fields — the strict-security profile's
   real use. The boundary contract: the wrapper must satisfy the
   `http.ResponseWriter`/`Hijacker`/`Flusher`/`ReaderFrom`/`Pusher`
   interface set the surrounding ecosystem expects, or explicitly not
   (documented per wrapper).

The parser/body layer never talks to the network; it consumes a
`*bufio.Reader`. The server owns sockets and deadlines.

## 2. Boundaries: what simdhttp touches and never touches

| ecosystem piece | relationship |
|---|---|
| `http.Handler` | the router *is* one; middleware is `func(http.Handler) http.Handler` |
| `*http.Request` | passed through unmodified; the router writes only `PathValue` via `SetPathValue` |
| `http.ResponseWriter` | passed through; wrapped only by user middleware |
| `PathValue` / `SetPathValue` | the param mechanism (Go ≥ 1.22; go.mod is 1.26.2) |
| `httptest` | `httptest.NewRecorder` + `NewRequest` are the primary test harness; router tests never open sockets |
| HTTP/2, TLS | untouched; h2 requests never carry a text head, so `http1` is HTTP/1-only by construction and the router is agnostic |
| reverse proxies | `httputil.ReverseProxy` consumes the same `*http.Request`; proxy headers (`X-Forwarded-*`) are helper territory, not router behavior |
| observability | middleware-shaped (logging, tracing, metrics) at the `Use` stack; the observability seam (§5) is the only place the router itself may dispatch |
| WebSocket | `Hijacker` preserved: the router never wraps `ResponseWriter` in a way that loses it; upgrades are handler business |

**No custom context.** `context.Context` stays `req.Context()`; helpers
never invent their own value types.

## 3. Error endpoint adapter (optional)

```
func Wrap(h func(http.ResponseWriter, *http.Request) error) http.Handler
```

- Converts the returned error to a status via an `ErrorMapper` (default
  `http.Error` semantics; typed sentinel errors map to 400/404/405/413
  classes per `http1`'s error taxonomy).
- If the response already started (headers written), the error is logged
  through the observability seam and the connection is closed; it is
  never written as a second status.
- Absent from the path when not requested: handlers that manage their
  own errors are untouched.

## 4. Middleware and helpers

- `Use(...)` chains standard middleware at build time (router LLD §7).
- Helpers: `JSON` encode/decode on simdjson, query/form accessors,
  `Param`. All concrete functions; the codec seam (§5) is the only
  indirection a helper may use.
- Nothing in the helper set may call `encoding/json`; the simdjson
  dependency is declared at the root package and versioned in `go.mod`.

## 5. Seams (the only permitted dispatch)

From `docs/architecture.md` §3.5, made concrete:

- **codec** — value parsing pluggable per handler (e.g. an alternate
  JSON codec) via explicit function fields with defaults;
- **error** — the `ErrorMapper` of §3;
- **observability** — optional hooks (request start/end, error, timing)
  with no-op defaults; hooks are called once per request, outside the
  match/parse hot loops;
- **future server** — reserved: a custom connection server would use
  this seam, not new interfaces in the router.

Every seam defaults to a concrete zero-cost path; a seam's indirect call
must not appear in the disassembly of the hot loops unless enabled.

## 6. Upgrades and protocol notes

- h2c upgrade requests (`Upgrade: h2c`) arrive as HTTP/1.1 text heads;
  the router routes them normally and the handler may accept the
  upgrade. `http1` framing treats `Upgrade` as ordinary; the
  `Connection: upgrade` hop-by-hop rule is recorded, not enforced, in
  compatible mode; strict mode requires the adapter to handle it
  explicitly.
- HTTP/2 and HTTP/3 never reach `http1` text parsing; the router's
  method/path matching is protocol-agnostic.
- WebSocket handshakes are routed as GET upgrades; the router must not
  and does not inspect `Sec-WebSocket-*`.

## 7. Tests

- `httptest`-based end-to-end: router + middleware + error adapter over
  a real `http.Server` and a real client, including hijack and
  `Flusher` assertions.
- Differential: `httptest.NewRecorder` behavior equality with ServeMux
  for the route corpus (`docs/verification.md` §5).
- Protocol boundary: h2 (via `httptest`'s HTTP/2 server support) and
  TLS smoke tests run against the router; they must pass without any
  `http1` involvement.
