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
segment  = literal | "{" name "}" | "*" | "{$}"  ; "*" and "{$}" final only
host     = literal [ "." literal ]             ; no params in host
```

- `Handle("GET", "/users/{id}", h)` — one segment, param `id`.
- `Handle("GET", "/files/*", h)` — `*` must be the entire final segment;
  matches one or more remaining segments including their slashes
  (`req.PathValue("*")` = remainder, URL-decoded once).
- **A pattern ending in `/` is a subtree**, equivalent to `*` with no
  name bound: `/users/` matches `/users/`, `/users/x` and `/users/x/y`,
  and `/` therefore matches every path. This is ServeMux's rule, probed
  on the toolchain oracle and pinned by the differential; the earlier
  exact-match reading in this document was wrong and would have 404ed
  the most common ServeMux idiom.
- `{$}` is the end-of-path marker: `/users/{$}` matches `/users/` and
  nothing below it. It is the exact-match form the subtree rule
  displaces.
- `{name}` matches exactly one **non-empty** segment. Probed: ServeMux
  answers 404 for `/a/` against `GET /a/{x}`.
- Request paths are **cleaned before matching** (`//`, `/./`, `/../`)
  and an unclean path is answered with a 307 to its clean form rather
  than matched, so `/a//b` cannot reach a handler `/a/b` would not.
  `CONNECT` is exempt, as in ServeMux, because it names an authority
  rather than a path.
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
more specific than a param, a param than a wildcard.

Conflicts are decided by the same relationship algebra ServeMux uses.
Two patterns are compared position by position, each position yielding
equivalent / more-general / more-specific / disjoint, and the results
combine: one position saying more-general and another saying
more-specific means **neither contains the other**, so some request
matches both with nothing to choose between them. That case, and exact
equivalence, are `Build` errors — never a runtime panic, which is the
one deliberate difference from ServeMux, and never a silent choice.

Detection is pairwise in principle. To keep a hundred-thousand-route
table from costing a quadratic `Build`, patterns are grouped by method
and host and then split on the literal at each position; patterns that
are not a literal at that position (a param, a wildcard, or a pattern
that has ended) can match alongside any bucket and so join all of them.
Measured: 100k routes build in 129 ms.

405 and method handling:

- **Method space.** `Handle` accepts any valid RFC 9110 token method
  and requires a non-empty one — every pattern carries an explicit
  method. Method-less patterns (ServeMux's `HandleFunc("/path", …)`,
  which matches any method) are **outside simdhttp's API by design**:
  the router is built on the explicit token-method surface, and the
  differential never generates method-less patterns. An empty-method
  `Handle("", …)` registration cannot be rejected at registration
  time — `Handle` returns nothing and never panics — so it surfaces
  as a `Build` error (§1). At `Build`, every registered method — and
  the common set `GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS,
  CONNECT, TRACE` — is compiled into a dense ID space (§6). Matching
  is exact and case-sensitive (`get` does not match `GET`; the parser
  already rejects non-token methods).
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

- `/users` matches that path exactly; `/users/` matches that path and
  everything below it (§2). Registering both is not a conflict: the
  bare form is more specific than the subtree at the position where
  they differ, so `/users` takes `/users` and `/users/` takes the rest.
- A request for `/users` when only `/users/` exists — the
    router redirects with a **307 Temporary Redirect** to `/users/`
    in compatible mode, **but only when the slash variant has a
    pattern matching the request method** (ServeMux's
    `matchOrRedirect` requires an exact match on `path+"/"` for the
    request method). When no method matches the slash variant, the
    compatible-mode verdict is **405**, and `Allow` unions the methods
    registered on the slash variant per §3 (probed on Go 1.26.5:
    `GET /users` with `GET /users/` registered → 307,
    `Location: /users/`; `GET /items` with only `POST /items/`
    registered → 405, `Allow: POST`). Strict mode answers `404`
    instead of either. The redirect mode is a `Build` option
    (`RedirectTrailingSlash bool`), default compatible = on. This is
    parity with ServeMux; the differential asserts status and
    location agreement, and the 405-with-union path is asserted in
    the Allow tests (§3, §8).

## 6. The match loop (hot path)

Compiled form: a segment trie, concrete nodes, no interfaces.

```
type node struct {
    static   map[string]*node   // literal segment
    single   *node              // {name}: one non-empty segment
    multi    *node              // "*" or a trailing slash: the rest
    handlers []*handlerEntry    // by method ID; nil = not registered
    methods  []string           // sorted, for the Allow value
}

type handlerEntry struct {
    h     http.Handler
    names []string // wildcard names in match order; "" binds nothing
}
```

Wildcard **names live on the entry, not the node**, because two methods
may register the same shape with different names — `GET /a/{x}` and
`POST /a/{y}` reach the same node, and binding both to whichever was
registered first would be wrong.

Hosts are **separate tries** keyed by the pattern's host rather than a
field on the node, so a host-scoped table is a lookup rather than a
per-node comparison; a request falls back to the hostless trie when its
own host has no match (§4).

**Matching is per method.** ServeMux keys its tree by method first, so
the most specific pattern is chosen among that method's patterns. The
walk therefore carries the request's method ID and accepts a terminal
node only if it serves that method; choosing the most specific pattern
overall and then checking its method answers 405 for requests the
reference serves. The 405/`Allow` path then re-walks without the filter
and unions the methods of **every** matching pattern.

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
  trailing-slash redirect (status **and** location — both are 307,
  and only when the slash variant matches the request method; the
  method-mismatch case is a 405 with the unioned `Allow` on both
  sides), and `PathValue` contents (`docs/verification.md` §5).
  `OPTIONS *` is excluded: it is simdhttp-specific (ServeMux alone
  400s; the standard server intercepts it unless
  `DisableGeneralOptionsHandler` is set) and is asserted directly in
  router tests.
- Conflict and precedence unit tests (conflicts are `Build` errors;
  `MustBuild` panics on them by design); param decoding (`%20`, `%2F`,
  double-encoded) pinned against ServeMux.
- Method-space tests: a custom token method (`MKCOL`, `REPORT`,
  case-sensitive `get` vs `GET`) matches exactly and participates in
  `Allow` lexicographically; the common-method inline path and the
  custom fallback table agree with `Handle` registrations; an
  empty-method `Handle("", …)` registration surfaces as a `Build`
  error (registration itself returns nothing and never panics).
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
