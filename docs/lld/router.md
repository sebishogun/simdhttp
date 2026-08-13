# LLD: Router (`simdhttp` root package)

A root, concrete, low-allocation, net/http-native router. Not built yet —
this is the approved design; the roadmap stages it after `http1`.

## 1. Shape

```
type Router struct { ... }                      // concrete; no interfaces inside
func New() *Router
func (r *Router) Handle(method, pattern string, h http.Handler)   // never panics; conflicts surface at Build
func (r *Router) HandleFunc(method, pattern string, h func(http.ResponseWriter, *http.Request))
func (r *Router) Use(mws ...func(http.Handler) http.Handler)      // before Build; wraps all
func (r *Router) Build() error                                    // immutable: lock, compile, freeze; conflict -> error
func (r *Router) MustBuild() *Router                              // explicit helper; panics on Build error
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request)
```

- **Immutable build**: registration mutates and never panics; `Build`
  compiles the route table once, validates conflicts, and freezes.
  A registration error or conflict is returned by `Build` — the only
  way to get a panic for a bad table is the explicit `MustBuild`
  helper, which exists precisely so tests and top-level setup can
  assert "this table is valid". `ServeHTTP` and all accessors after
  `Build` are read-only and safe for concurrent use by any number of
  goroutines. There is no reconfiguration after `Build`.
- **Ownership**: the router owns its compiled table; handlers own
  themselves. Route patterns are copied at registration (the caller may
  reuse or mutate the strings). `ServeHTTP` allocates nothing on the
  matched path and does not retain `req`.
- **Params**: `Request.SetPathValue(name, value)` (exported since Go
  1.22) — the router populates the standard mechanism, so middleware and
  handlers use `req.PathValue(name)` exactly as with `net/http.ServeMux`.
  No custom context, no wrapper type.

## 2. Route syntax

Pattern grammar (method is separate):

```
pattern  = [host "/"] path
path     = "/" segment *( "/" segment )        ; leading "/" required
segment  = literal | "{" name "}" | "*"        ; wildcard = final segment only
host     = literal [ "." literal ]             ; no params in host
```

- `Handle("GET", "/users/{id}", h)` — one segment, param `id`.
- `Handle("GET", "/files/*", h)` — `*` must be the entire final segment;
  matches the rest of the path including further slashes
  (`req.PathValue("*")` = remainder, URL-decoded once).
- Literals are matched byte-exact after a single URL-decode of each
  segment (see §5); `%2F` inside a segment decodes to `/` and is treated
  as a literal slash character within the segment — the same choice
  `net/http.ServeMux` documents, pinned by a differential test.
- Params may not repeat a name within one pattern (build error). Empty
  `{}` and adjacent params are build errors. `{name}` with a trailing
  literal inside a segment (e.g. `{id}.json`) is a build error in v1 —
  rejected, not silently redefined.

## 3. Precedence and conflicts

At a given path position, in order:

1. static segment (longest first);
2. param segment `{name}`;
3. wildcard `*`.

Full precedence: a pattern wins if it matches with a *more specific*
segment at the first position where they differ; a static segment is
more specific than a param, a param than a wildcard; otherwise the
longer literal prefix wins. Ties are build errors — `Build` rejects
ambiguous registrations (two patterns that could match the same request
with equal precedence) instead of silently choosing one, matching
`ServeMux`'s conflict detection but stricter: a conflict is always a
`Build` error, never a runtime panic.

405 and method handling:

- A path matched by patterns of *other* methods registers `405 Method
  Not Allowed` with an `Allow` header listing the methods (`GET, HEAD,
  OPTIONS` order canonical).
- `HEAD` is served by the `GET` handler when no `HEAD` pattern exists
  (the handler must not write a body — standard contract, documented);
  an explicit `HEAD` pattern wins.
- `OPTIONS *` (asterisk-form, RFC 9110 §7.1) is answered by the router
  itself with `Allow` when no `OPTIONS` pattern matches; an explicit
  `OPTIONS` pattern wins. This is **explicit simdhttp router behavior,
  not ServeMux parity**: Go's server does not special-case
  asterisk-form OPTIONS (probed on Go 1.26.5: ServeMux answers 400),
  so the router differential excludes this case and asserts simdhttp's
  behavior directly.
- Method match is exact, case-sensitive (`get` does not match `GET`);
  the parser already rejects non-token methods.

## 4. Host matching

`Handle("GET", "api.example.com/v1/{id}", h)` — host patterns are
matched case-insensitively against `req.Host` minus the port; a request
whose `Host` has a port and whose authority matches the pattern's host
matches. No wildcard hosts in v1. Patterns without a host match any
host. A request with no `Host` (HTTP/1.0) matches hostless patterns
only.

## 5. Trailing slash

- `/users` and `/users/` are distinct routes unless:
  - both are registered — then each matches its own form exactly
    (no merging);
  - only `/users/` exists and a request for `/users` arrives — the
    router answers with a **307 Temporary Redirect** to `/users/` in
    compatible mode; in strict mode it answers `404`. The redirect
    mode is a `Build` option (`RedirectTrailingSlash bool`), default
    compatible = on. The 307 is a documented difference from
    `ServeMux`, which redirects with 301: the differential asserts the
    location (append the slash) and asserts the status as the
    documented difference, not as agreement.

## 6. The match loop (hot path)

Compiled form: a segment trie, concrete nodes, no interfaces.

```
type node struct {
    static   map[string]*node   // or sorted slice for small fan-out
    param    *node              // {name}
    wildcard *node              // *
    handlers [maxMethodID]*handlerRef   // nil = no method
    host     string             // "" = any
}
```

`ServeHTTP` walk: per segment, check static (map lookup on the decoded
segment — one decode pass), then param, then wildcard; at the leaf,
pick the method's handler or synthesize 405/OPTIONS. The walk allocates
nothing; params are written through `SetPathValue` (which stores into
`req`'s existing map). Method dispatch is a switch on the method string
hashed once — no `map[string]` in the per-request path, no closures.

## 7. Middleware and helpers

- `Use` middleware stack wraps all routes at `Build` (compiled into the
  handler chain, not applied per request).
- Helpers (concrete functions, no interfaces): `JSON` encode/decode
  through simdjson, query/form accessors over `req.URL.Query()`,
  `Param(r, name)` sugar over `PathValue`, and the optional error
  endpoint adapter `Wrap(func(http.ResponseWriter, *http.Request) error)`.
- Everything composes with `httptest`, `http.Handler`, HTTP/2/TLS,
  proxies, observability and WebSocket hijacking because the router
  never wraps `ResponseWriter` unless middleware asks for it, and never
  touches `req` beyond `PathValue`/standard fields.

## 8. Tests

- Route differential vs `net/http.ServeMux`: generated pattern sets and
  request corpora must agree on match, params, 405/Allow, and
  `PathValue` contents (`docs/verification.md` §5). Two cases are
  asserted as documented differences, not agreement: trailing-slash
  redirect status (simdhttp 307, ServeMux 301; location must agree) and
  `OPTIONS *` (simdhttp's explicit behavior; ServeMux 400s).
- Conflict and precedence unit tests (conflicts are `Build` errors;
  `MustBuild` panics on them by design); param decoding (`%20`, `%2F`,
  double-encoded) pinned against ServeMux.
- No-panic fuzz over random paths against a built table.
- Bench: match of a 4-segment route with 2 params at 100k patterns;
  gate = 8.3% floor + `perf stat` + disassembly (no indirect call in the
  walk).
