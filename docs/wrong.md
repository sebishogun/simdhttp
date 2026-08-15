# Where the obvious answer was wrong

Entries are written as *what was believed / what was true / how it
surfaced*, with the source that established it, because that is the only
form in which this knowledge is useful. This repository records only two
classes of finding so far: the measured performance reversal in the
shipped parser's history, and sourced safety findings in the shipped
parser. A finding that cost a measurement belongs here whether or not
any code changed.

---

## 1. The typical-shape advantage reversed 1.22x to 1.05x

**Believed.** The one-pass boundary scan and inline short-line path that
fixed the 100-header shape (1.3x loss -> 4.7x win) could be judged by
that row alone; the typical nine-header shape was already a win and
would stay one.

**Actually.** The rework that fixed the many-header shape cost the
typical shape. Commit `a60a44b` (initial parser) measured 1,097 ns vs
net/http's 1,341 on the nine-header browser request — 1.22x, "minimum
of three". Commit `530ac05` (one-pass scan, inline short path, kernel
threshold) shipped a README quoting the typical row at ~1,400 vs ~1,470
— 1.05x. The 100-header row went from a 1.3x loss to 2,219 vs 10,491
(4.7x), and the giant-value row stayed 3.5x on aliasing, but the
typical shape's margin nearly halved.

**How it surfaced.** Not by any new measurement of the typical shape —
the number was already in the 530ac05 README diff, next to the win it
was expected to justify. The reversal is only visible by comparing the
two commits' READMEs (and the chart, `docs/bench.svg`, which draws the
post-rework ratio rounded up to 1.1× — see the README's provenance
note; the table is authoritative).

**Source.** `git log` — commit messages and README diffs of `a60a44b`
and `530ac05`; README.md as of `5c2bee2`.

**Consequence.** The README now carries the chart and table as
historical amd64/AVX-512 data, not as a current or portable promise,
and any future change to the header walk re-measures the typical shape
against the 8.3% floor plus `perf stat` and disassembly.

---

## 2. The long-value control scan stops at a tab

**Believed.** The value control scan is a single
`simd.IndexAnyOrLess(val, "\x7f", 0x20)` call with an HTAB allowance;
`\t` is below 0x20, so a value containing a tab is checked, the tab
excused, and the scan done.

**Actually.** The call returns the *first* byte in `\x7f ∪ < 0x20`. If
that byte is `\t`, the guard `val[i] != '\t'` passes and the scan
stops — a control byte after the first HTAB in a value at or above
`ctlScanThreshold` (64) bytes is never examined. The short-value inline
loop has no such hole (it scans every byte). net/http rejects the same
value ("malformed MIME header line").

**How it surfaced.** Source reading of `parser.go`'s threshold branch
during the 2026-08-13 documentation audit, then a live differential
run: `X-Long: <70×v>\t<10×w>\x00<20×x>` is accepted by simdhttp and
rejected by net/http; the identical value with the NUL before the tab
is rejected by both; the identical value under 64 bytes is rejected by
both. The differential fuzz cannot find it either: its seeds are short
and a 15 s run (~3.9 M execs) never grows a ≥ 64-byte value, so the
oracle is never asked.

**Source.** `parser.go:148-158` (threshold branch), `simd` v1.20.0
`IndexAnyOrLess` contract (`text.go`), live differential, Go 1.26.5.

**Consequence.** G1 in `docs/architecture.md`; fix designed in
`docs/lld/http1-head-parser.md` §3.3 with a regression test that fails
on today's parser; fuzz seeds extended in Phase 0.

---

## 3. Duplicate Host is accepted, even when identical

**Believed.** Headers are an unordered list; two `Host` lines with the
same value are equivalent to one.

**Actually.** net/http rejects *any* second `Host` line at read time —
"too many Host headers" — including two identical ones
(`request.go:1139`). simdhttp accepts them.

**How it surfaced.** Differential run, 2026-08-13:
`Host: a.com\r\nHost: a.com` — net/http errors, simdhttp returns both.
The differential fuzz reached the same case on 2026-08-13 evening and
now fails its one-direction assertion on it: the input
`0 * HTTP/1.0\r\nHost:\r\nHost:\r\n\r\n` (two empty Host lines on an
HTTP/1.0 request line) is accepted by simdhttp and rejected by
net/http. On this machine the input is replayed at baseline coverage
from the **local campaign state** — the `GOCACHE` fuzz cache plus the
run-written `testdata/fuzz/FuzzParseAgainstNetHTTP/4cb4ee00bf74f878` —
so the fuzz smoke is red here. A **fresh clone has neither**: no
regression corpus file is committed on this branch (verified:
`git ls-files` contains no `testdata`), so a fresh clone's fuzz stays
green until a run happens to rediscover the input, or until Phase 0
ships. The production plan's Task 3 commits the corpus file
`4cb4ee00bf74f878` together with the duplicate-Host fix, so fresh
clones replay it at baseline and cannot regress. The red gate is
itself the documented finding, not a launderable flake.

**Source.** Live differential; Go 1.26.5 `net/http/request.go:1139`;
`go test -fuzz=FuzzParseAgainstNetHTTP` on 2026-08-13.

---

## 4. Request-target control bytes and invalid escapes are accepted

**Believed.** The fuzz contract says URI semantics are the caller's
(net/url) business, so the parser need not look at the target at all.

**Actually.** The exclusion is about *semantics* (what net/url accepts
as a URI). Control bytes — NUL, DEL — and invalid percent-escapes
(`%zz`, a trailing `%2`) in the target are a framing-level hygiene
matter: net/http rejects them via `url.ParseRequestURI` ("invalid
control character in URL", "invalid URL escape") while simdhttp
accepts them unchanged. `%20`-style escapes are fine in both; that is
the semantic part. The parser's own strictness (tokens, CRLF, controls
in values) makes the target the one unchecked control surface.

**How it surfaced.** Differential run, 2026-08-13: `GET /a\x00b` and
`GET /a\x7fb` accepted by simdhttp, rejected by net/http; later the
same run showed `GET /%zz` and `GET /a%2` accepted by simdhttp,
rejected by net/http.

**Source.** Live differential; `fuzz_test.go`'s scoping comment
(read with care: it excludes net/url verdicts, which is exactly the
layer that catches this).

---

## 5. Framing fields are opaque — CL+TE and duplicate CL pass

**Believed.** A head parser that stops at the blank line has no framing
responsibility; whatever a future body layer needs, it can re-derive.

**Actually.** `Content-Length` and `Transfer-Encoding` are smuggling
surface from the moment a server consumes the head. Go 1.26.5 accepts
a CL+TE combination at every layer: `ReadRequest` and the server both
delete `Content-Length` and frame chunked (`fixLength` in
`transfer.go`; probed: server 200 with the chunked body). Two
*identical* `Content-Length` values are deduped and accepted;
*differing* duplicates are rejected ("message cannot contain multiple
Content-Length headers"). simdhttp returns all of these opaquely: both
lines of a duplicate CL, and CL+TE together, pass.

**How it surfaced.** Differential run, 2026-08-13: two differing CLs —
net/http errors, simdhttp accepts; two identical CLs — net/http
dedupes and accepts, simdhttp accepts both lines; CL+TE — both accept
at read level, and a live server probe (2026-08-13 re-review) showed
the Go server accepts it too: the handler ran with the chunked body
and answered 200.

**Source.** Live differential; Go 1.26.5 `net/http` `transfer.go`
(`fixLength`, `parseTransferEncoding`) and `request.go`.

**Consequence.** The future profiles reject every duplicate CL and the
CL+TE combination — deviations D6 and D7 in `docs/architecture.md`
§2.1, both deliberately stricter than `ReadRequest`.

---

## 6. HTTP/1.1 without a usable Host is accepted

**Believed.** Host is a header like any other; the parser's job stops
at the blank line.

**Actually.** Host is a framing-critical field. Go 1.26.5's server
rejects HTTP/1.1 requests with no Host ("missing required Host
header") and with a malformed Host ("malformed Host header", a
byte-table scan with no bracket-balance logic — and it *allows* a
comma), while a **present-but-empty** `Host:` is accepted (probed:
200 OK). `ReadRequest` rejects duplicates (finding 3). simdhttp
accepts `GET / HTTP/1.1\r\n\r\n`, `Host:`, `Host: bad host`, and
`Host: a.com,b.com` alike.

**How it surfaced.** Differential run, 2026-08-13, including a live
server probe (`net.Listen` + `http.Server`) that separated the three
cases: missing → 400, empty → 200, space-malformed → 400, comma
without space → 200, unbalanced bracket → 200.

**Source.** Live differential and server probe; Go 1.26.5
`net/http/server.go` (`conn.readRequest` Host checks) and
`internal/httpguts` `ValidHostHeader` (via x/net).

**Consequence.** The future profiles treat empty as missing (deviation
D5) and apply the stricter simdhttp host rule (D9: no comma, balanced
brackets).

---

## 7. The differential fuzz found two real gaps before passing

**Believed.** The initial parser's version check and request-line
splitting were complete.

**Actually.** Commit `530ac05`'s message records what the differential
fuzz found at ~35 M executions: an unvalidated HTTP version (any third
field passed) and the empty request-target case (`GET  HTTP/1.1`-style,
where the old `len(Proto)==0` guard did not cover the empty target).
Both became `isHTTPVersion` and the explicit empty-target rejection.

**How it surfaced.** Fuzz failure logs during the `530ac05` session,
then the two fixed branches.

**Source.** `git log` commit `530ac05` message and `parser.go` diff.

**Consequence.** The fuzz found what the corpus missed; the corpus is
extended in Phase 0 with the G1 long-value seed so the same class does
not hide again.

---

## 8. `bench-check` can never fail (gate flaw, not a code flaw)

**Believed.** `make bench-check` reports failure when a row regresses
past the 8% floor.

**Actually.** Two layers guarantee it cannot. The bench result is
piped through `tee` with no `pipefail`, so the recipe line's status is
`tee`'s; and the recipe has a second line — an unconditional
`@echo` — which is the target's last command, and therefore the status
`make` records. Either layer alone would launder a red run; together
the target always exits 0. The house rule ("never pipe a gate without
`pipefail`") is violated in this repo's own Makefile.

**How it surfaced.** Reading the `Makefile` against the house rule
during the 2026-08-13 audit. No regression has been laundered in this
repo's short history; the flaw is the exposure.

**Source.** `Makefile` `bench-check` target.

**Closed 2026-08-13** by the Phase 4 gates rework (`34bfc83`):
`scripts/bench-check.sh` compares against a committed
`testdata/bench.txt` with no pipe carrying the verdict, and was proved
red by halving one baseline row -- exit 2, bare and piped. The entry
below stands as the record of the flaw.

**Consequence.** `bench-check` is advisory until the Phase 4 gates
rework (it also references `testdata/bench.txt`, which no commit has
ever added); all benches are judged by bare, interleaved, minimum-of-six
runs per `docs/verification.md` §2. Two thresholds are at play and are
not the same number: the Makefile comment's "8%" is this repo's chosen
regression guard; the **8.3%** noise floor is the simd-family
measurement policy, inherited from the measured record in the simd
repository's `CLAUDE.md` — it has not been re-measured in this
repository (future work), so this repo quotes it as inherited policy,
not as a locally measured constant.

## 9. The router's trailing slash was designed as an exact path

**Believed.** `docs/lld/router.md` §5 stated that `/users` and
`/users/` are distinct routes and that each "matches its own form
exactly (no merging)". The first implementation did that: `/users/`
parsed to a final empty literal segment and matched only `/users/`.

**Actually.** `net/http.ServeMux` treats a pattern ending in a slash as
a **subtree**. Probed: with only `GET /users/` registered, `/users/x`
and `/users/x/y/z` both match it, and `GET /` matches
`/anything/at/all`. The exact-match reading would 404 the most common
ServeMux idiom — `mux.Handle("/static/", fileServer)` — for every path
below the prefix.

**How it surfaced.** The Task 16 differential, on its first run, over
generated pattern sets. Five hand-written fixed sets had not reached
it; the generated sets did, immediately and in bulk.

**Source.** `net/http` `routing_tree.go`; probe in the 2026-08-13
session against the toolchain oracle.

**Consequence.** The subtree reading is now the implementation and the
LLD says so. `{$}` was added as the end-of-path marker, which is the
exact-match form the subtree rule displaces, and `*` remains the named
form of the same thing. The document was wrong before the code was; a
differential against the reference is what distinguishes those.

## 10. Choosing the most specific pattern, then checking its method

**Believed.** Route matching is about the path; once the most specific
pattern is found, its method table answers whether the request is
served or gets a 405.

**Actually.** ServeMux keys its tree by **method first**, so the most
specific pattern is chosen among the patterns registered for *that
method*. The two readings differ whenever a more specific pattern
exists for another method: with `GET /x/{a}` and `POST /` registered, a
`POST /x/y` matches `/` under ServeMux and 405s under the
path-first reading — a request the reference serves, refused.

**How it surfaced.** The generated differential, as a cluster of
`-> 405, ServeMux 200` rows.

**Source.** `router.go` `matchSegments`, which now carries the request's
method ID and accepts a terminal node only if it serves that method.

**Consequence.** The walk takes a method filter; the 405 and `Allow`
paths re-walk without it. Two walks on a 405 is a cold-path cost for a
hot-path correctness property.

## 11. `Allow` named the winning node's methods

**Believed.** On a 405, the methods to advertise are the ones
registered at the node the path matched, plus the trailing-slash
variant.

**Actually.** ServeMux enumerates every pattern that *could* match the
path, across all branches — parameter and subtree patterns included —
not the methods of the one node that won. With `MKCOL /x` and
`PATCH /` registered, `POST /x` must answer `Allow: MKCOL, PATCH`; the
node-local reading answered `Allow: MKCOL`.

**How it surfaced.** The generated differential, as byte-level `Allow`
mismatches.

**Source.** `router.go` `collectMethods`, a second walk that visits
every matching branch.

**Consequence.** `Allow` is built by a full enumeration on the cold
path. The header is a promise about what the server would accept, and a
short one sends a client away from a method that would have worked.

## 12. One redirect, two escapings

**Believed.** A redirect's `Location` is the target path; how it was
built does not matter.

**Actually.** ServeMux builds the two redirects from different strings.
The trailing-slash redirect uses `cleanPath(u.Path)` — the **decoded**
path — while the unclean-path redirect uses the cleaned **escaped**
path, and both then pass through `url.URL.String()`, which escapes
again. So the same target arrives as `/a/%7Bp1%7D/` from one and
`/a/%257Bp1%257D/` from the other.

**How it surfaced.** The generated differential compared `Location`
byte for byte and produced both spellings against a request path
containing an unreplaced `{p1}` placeholder — an artifact of the
generator that happened to expose a real difference.

**Source.** `net/http` `server.go` `findHandler`; `router.go`
`redirect`.

**Consequence.** Both paths are reproduced exactly. A `Location` a
client follows differently is a different request, so byte agreement
here is worth more than a tidier construction.

## 13. simdjson links `encoding/json.Marshal`

**Believed.** The helper set reaches JSON only through simdjson, and
`encoding/json` is absent from the built binary.

**Actually.** `encoding/json.Marshal` is present in the linked binary
of a program that uses `simdhttp.JSON`. Two separate reasons: simdjson
imports `encoding/json` for **type identity** — `RawMessage`,
`Marshaler`, `Unmarshaler` and the error types are aliases, and must be
for a caller's structs to pass between the two packages — and
`marshal.go` retains a `json.Marshal(iv)` **fallback** for a value
whose `json.Marshaler`/`TextMarshaler` assertion fails at run time.

**How it surfaced.** The Task 18 dependency test, written to assert
`encoding/json` was absent from the graph, failed. `go tool nm` on a
real binary then showed `encoding/json.Marshal` linked, not just the
package present.

**Source.** `simdjson/compat.go`; `simdjson/marshal.go:481`.

**Consequence.** The test now asserts the precise true thing: simdjson
is the only package in the graph importing `encoding/json`, and no
package of this module does. The aliases are not removable without
breaking simdjson's drop-in promise. The `json.Marshal` fallback is
simdjson's to remove and is recorded there.

## 14. The oracle version in the documents was not the toolchain on PATH

**Believed.** `docs/verification.md` and `docs/architecture.md` both
stated that every behavior verdict refers to "the Go 1.26.5 toolchain
actually run", and `mise current` agrees: `go 1.26.5`.

**Actually.** `which go` resolves to
`.../mise/installs/go/1.26.2/bin/go` and `go version` reports
`go1.26.2`. Whatever mise declares, the binary that executes is 1.26.2,
and every probe recorded in this repository has used it.

**How it surfaced.** Writing the Phase 4 documentation pass and
checking the claim before repeating it.

**Source.** `go version`; `which go`; `mise current`.

**Consequence.** The claim was corrected rather than the record
rewritten, and the substance was checked rather than assumed: the
router probe battery was replayed under 1.26.5 with its own GOROOT and
build cache and produced **byte-identical output** to 1.26.2 —
trailing-slash subtree matching, `{$}`, path cleaning with 307, the
precedence rows and the conflict panic all agree. No verdict in this
repository changes. The lesson is narrower than it looks: a version
declared by a tool manager is not evidence that the version ran, and
`go version` costs nothing to check.

## 15. A fuzz target the verification document has listed since v1.2.0 and nobody wrote

`docs/verification.md:115` says:

> `FuzzRouterMatch` (Phase 2) — random paths and methods against a built
> table: no panic, deterministic params.

No test of that name has ever existed in this repository — `git log -S` finds
it only in the v1.2.0 documentation commit that introduced the sentence. It
sits in the document that lists what the suite checks, between two targets that
do exist, and reads exactly like them.

Found by a sweep across the whole family for documents naming tests that do not
exist. Ten repositories, ~1,180 declared tests: this was the only one.
(simdlogs had three of the same shape, all renames rather than absences;
simd had one rename and one benchmark name quoted from somebody else's blog
post.)

Written rather than deleted, because the guarantee is worth having and the
sentence was right about that. 15.4M executions, no panic, no non-determinism.

**Two things about writing it that are worth more than the target.**

The panic property probes cleanly — a deliberate `index out of range` on paths
beginning `/u` fails on the seed corpus alone. The determinism property does
not probe as cleanly, because making the router non-deterministic is not a
one-line mutation; it is asserted and not yet proven reachable.

And the first version of the fixture used `/static/{rest...}`, which this
router does not have — its wildcard is `*`. `Build()` accepted it silently as a
single-segment parameter named `rest...`, so `/static/a/b/c` matched nothing
and the wildcard branch was never fuzzed. Nothing in `FuzzRouterMatch` would
have said so: "no panic, and the same answer twice" is satisfied perfectly by a
table that matches nothing at all. `TestTheRouterFuzzSeedsReachRealRoutes` is
what caught it, and it is there because a fuzz target over a table that never
matches is a panic check on the miss path wearing a router's name.

## 16. The hand-kept fuzz list, and what discovering the targets found in ten seconds

`make fuzz-smoke` named its targets by hand: three lines, all `./http1`. It
missed `FuzzRouterMatch` the moment that target was written (entry 15) — so the
fix for a target that existed only in a document would have gone straight back
into a target that existed only in the tree.

Targets are DISCOVERED now, via `go test -list '^Fuzz'`, which asks the
compiler. Same reasoning as simdlogs' fuzz workflow, which already did this and
says why: "A hand-maintained list is how a new target gets written and never
run."

**Two defects surfaced within a minute of the change.**

The first was mine, in the discovery loop itself. `-fuzz "^$$t$$$$"` in a
Makefile expands to `^$t$$` in the shell, and `$$` in a shell is the PROCESS
ID. Every target ran with a pattern matching nothing, so `go test` ran the seed
corpus and reported `ok ... 0.003s` — five green lines, four seconds of work,
no fuzzing at all. Caught only because the timings were implausible for
`-fuzztime 5s`. A gate that reports success in three milliseconds is not a fast
gate.

The second was real. With the pattern fixed, `FuzzParseAgainstNetHTTP` failed
in 3 seconds:

    net/http rejects (invalid empty Content-Length ""), simdhttp accepts:
    "0 * HTTP/1.0\r\n0000:0\r\nContent-Length:\r\n\r\n"

That target's contract is the security one — "simdhttp must never accept a head
that net/http rejects", because a front parser more permissive than the origin
is how a smuggled request slips through. It had been in the hand-kept list all
along and had not surfaced this.

It is a false positive, and the reason is worth more than the finding.
`parseContentLength` rejects an empty value; `FramingOf` returns
`ErrBadContentLength` for it. But the target compared `Parse` alone against
`http.ReadRequest`, and `ReadRequest` parses the head AND resolves the framing
in one call. Two parsers were being compared at different points in their
pipelines, so the target could report a divergence the library does not have —
and it wrote one into the committed corpus, where it would have read as a
security regression to whoever found it next. It compares `Parse` followed by
`FramingOf` now, which is the same decision, and 17.9M executions agree.

**The shape.** Entry 15 was a target named in a document and never written.
This is the same class one layer over: a target written and never run, and then
a target run against the wrong comparison point. In both cases the artefact
looked like coverage from every angle except the one that asks what it executes.
