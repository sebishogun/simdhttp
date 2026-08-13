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
  segment; `%2F` inside a segment decodes to `/` and is treated
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

- **Method space.** `Handle` accepts any valid RFC 9110 token method
  and requires a non-empty one — every pattern carries an explicit
  method. Method-less patterns (ServeMux's `HandleFunc("/path", …)`,
  which matches any method) are **outside simdhttp's API by design**:
  the router is built on the explicit token-method surface, and the
  differential never generates method-less patterns. At `Build`,
  every registered method — and the common set `GET, HEAD, POST, PUT,
  PATCH, DELETE, OPTIONS, CONNECT, TRACE` — is compiled into a dense
  ID space (§6). Matching is exact and case-sensitive (`get` does not
  match `GET`; the parser already rejects non-token methods).
- A path matched by patterns of *other* methods registers `405 Method
  Not Allowed` with an `Allow` header built **ServeMux-exactly** (Go
  1.26.5): the methods registered for the path — including custom
  token methods — **sorted lexicographically**, plus an implicit
  `HEAD` entry whenever `GET` is registered; there is **no implicit
  `OPTIONS`**. Two ServeMux details are reproduced verbatim: the
  trailing-slash variant of the path is unioned in when the request
  path lacks a slash (`mux.matchingMethods` in `server.go` unions
  `path` and `path+"/"` — a request for `/users` unions the methods
  registered on `/users/`), and implicit `HEAD` is added by the tree
  layer (`routing_node.matchingMethods` in `routing_tree.go`). The
  one deliberate relaxation of "list only registered methods":
  `Allow` lists `HEAD` even when no explicit `HEAD` pattern exists,
  because `GET` implies it — exactly as ServeMux does. The `Allow`
  value is deterministic for any build; construction happens only on
  the cold 405 path and may allocate.
- `HEAD` is served by the `GET` handler when no `HEAD` pattern exists
  (the handler must not write a body — standard contract, documented);
  an explicit `HEAD` pattern wins.
- `OPTIONS *` (asterisk-form, RFC 9110 §7.1) is answered by the router
  itself with `Allow` when no `OPTIONS` pattern matches; an explicit
  `OPTIONS` pattern wins. The router only ever sees this request when
  the surrounding server passes it through: Go 1.26.5's standard
  `http.Server` intercepts `OPTIONS *` before the handler
  (`serverHandler` swaps in `globalOptionsHandler`; probed: 200, no
  `Allow`) unless the application sets
  `Server.DisableGeneralOptionsHandler = true`. So simdhttp's
  with-`Allow` behavior is available in independent-reader/future-
  server mode, or in handler mode only with that flag set. Asserted
  directly in router tests (ServeHTTP called directly), not against
  ServeMux — which alone answers `OPTIONS *` with 400 (probed).

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
    compatible = on. This is **parity with ServeMux**, which also
    redirects trailing-slash mismatches with 307 (probed on Go 1.26.5:
    `GET /users` → 307, `Location: /users/`); the differential asserts
    status and location agreement.

## 6. The match loop (hot path)

Compiled form: a segment trie, concrete nodes, no interfaces.

```
type node struct {
    static   map[string]*node        // or sorted slice for small fan-out
    param    *node                   // {name}
    wildcard *node                   // *
    handlers [maxMethodID]*handlerRef // nil = no method
    host     string                  // "" = any
}
```

`ServeHTTP` walk: per segment, check static (map lookup on the decoded
segment — one decode pass, §2), then param, then wildcard; at the leaf,
resolve the method ID (§3's method space) and pick the handler, or
synthesize 405/OPTIONS. The walk allocates nothing; params are written
through `SetPathValue` (which stores into `req`'s existing map).

**Method resolution** — implementable, no interface dispatch, no
`map[string]` in the common path:

- The nine common methods (`GET, HEAD, POST, PUT, PATCH, DELETE,
  OPTIONS, CONNECT, TRACE`) own dense fixed IDs 0–8. Resolution is an
  inline compare chain: match by length and bytes, case-sensitive —
  two compares for typical methods, no table, no hash.
- Custom token methods registered via `Handle` are assigned IDs 9+
  at `Build` into an immutable open-addressing table (bucket by first
  byte and length, then full compare) that maps the raw method string
  to its ID. Custom methods are rare in practice, so the fallback
  lookup is a cold path; it uses no interfaces and no per-request
  allocation.
- `maxMethodID` is 9 + the number of distinct custom methods — the
  node's `handlers` array is sized at `Build` from the registered
  method set.
- 405 synthesis at the leaf enumerates the node's non-nil method IDs
  and emits the `Allow` value per §3's ServeMux-exact rule
  (lexicographic, implicit `HEAD` with `GET`, no implicit `OPTIONS`).
  This is the cold 405 path: it may allocate the `Allow` string. The
  match path itself allocates nothing.
- The one decode pass (§2), the trie walk, and method resolution
  together contain no closures and no indirect calls; the disassembly
  gate (`docs/verification.md` §6) applies to the whole walk.

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

- Route differential vs `net/http.ServeMux`: generated pattern sets
  (explicit-token-method patterns only — method-less ServeMux
  patterns are out of simdhttp's API, §3, and never generated) and
  request corpora must agree on match, params, 405/Allow,
  trailing-slash redirect (status **and** location — both are 307),
  and `PathValue` contents (`docs/verification.md` §5). `OPTIONS *`
  is excluded: it is simdhttp-specific (ServeMux alone 400s; the
  standard server intercepts it unless
  `DisableGeneralOptionsHandler` is set) and is asserted directly in
  router tests.
- Conflict and precedence unit tests (conflicts are `Build` errors;
  `MustBuild` panics on them by design); param decoding (`%20`, `%2F`,
  double-encoded) pinned against ServeMux.
- Method-space tests: a custom token method (`MKCOL`, `REPORT`,
  case-sensitive `get` vs `GET`) matches exactly and participates in
  `Allow` lexicographically; the common-method inline path and the
  custom fallback table agree with `Handle` registrations; a method-
  less `Handle("", …)` registration is rejected at registration time.
- 405 `Allow` unit tests pinned to ServeMux semantics: lexicographic
  order; implicit `HEAD` present whenever `GET` is registered (even
  with no explicit `HEAD` pattern) and absent otherwise; no implicit
  `OPTIONS`; custom methods sorted in with the rest; **the trailing-
  slash variant is unioned in when the request path lacks a slash**
  (e.g. only `/users/` has `POST` registered and `/users` is
  requested → `Allow` includes `POST`); the exact `Allow` string
  asserted (e.g. `GET, HEAD, POST`, `MKCOL, REPORT`).
- No-panic fuzz over random paths against a built table.
- Bench: match of a 4-segment route with 2 params at 100k patterns;
  gate = 8.3% floor + `perf stat` + disassembly (no indirect call in the
  walk).
