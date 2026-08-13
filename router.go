package simdhttp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

// A route table that is mutable until Build and read-only afterwards.
//
// The split exists so the serving path has nothing to synchronize: after Build
// the trie, the method table and the handler chain are never written again, so
// any number of goroutines walk them without a lock and without an atomic. The
// cost is that a table is assembled once -- which is also what makes a conflict
// a build-time error rather than a request-time surprise.
//
// The matching rules are net/http.ServeMux's, probed rather than assumed and
// held to them by a differential test. A router that agrees with ServeMux on
// the ordinary route and diverges on a trailing slash or a 405 is worse than
// one that is obviously different: the divergence is found in production
// instead of at the swap.

// Router matches requests to handlers. The zero value is not usable; call New.
type Router struct {
	// RedirectTrailingSlash redirects a request for /users to /users/ when the
	// slash form has a pattern for the request method. Set it before Build.
	RedirectTrailingSlash bool

	regs  []registration
	mws   []func(http.Handler) http.Handler
	errs  []error
	built bool

	roots      map[string]*node // keyed by the pattern's host; "" matches any
	methodIDs  map[string]int   // custom methods only; IDs from numCommonMethods
	numMethods int
	allowAll   string // every method in the table, for asterisk-form OPTIONS
	hostRoutes bool   // any host-scoped pattern registered
}

type registration struct {
	method  string
	pattern string
	h       http.Handler
}

// handlerEntry is one registered route at a node. The wildcard names live here
// rather than on the node because two methods may register the same shape with
// different names -- "GET /a/{x}" and "POST /a/{y}" reach the same node, and
// binding both to whichever was registered first would be wrong.
type handlerEntry struct {
	h       http.Handler
	names   []string // wildcard names in match order; "" binds nothing
	pattern string
}

// node is one segment position in the compiled trie.
type node struct {
	static   map[string]*node
	single   *node           // {name}: one non-empty segment
	multi    *node           // trailing slash or *: the rest of the path
	handlers []*handlerEntry // indexed by method ID; nil means not registered
	methods  []string        // registered names, sorted, for the 405 Allow value
}

// New returns an empty router in compatible mode.
func New() *Router {
	return &Router{RedirectTrailingSlash: true, roots: map[string]*node{}}
}

// Handle registers a handler. It never panics: an invalid method or pattern,
// or a conflict with an existing route, is reported by Build. Registering
// after Build is itself a build error, so a table cannot change under a server
// already serving from it.
func (r *Router) Handle(method, pattern string, h http.Handler) {
	if r.built {
		r.errs = append(r.errs, fmt.Errorf("simdhttp: %s %q registered after Build", method, pattern))
		return
	}
	// Go strings are immutable, so holding the arguments is the copy the caller
	// is promised: nothing here aliases a caller's buffer.
	r.regs = append(r.regs, registration{method: method, pattern: pattern, h: h})
}

// HandleFunc registers a handler function.
func (r *Router) HandleFunc(method, pattern string, h func(http.ResponseWriter, *http.Request)) {
	r.Handle(method, pattern, http.HandlerFunc(h))
}

// Use adds middleware applied to every route, first added outermost. It is
// compiled into the handler chain at Build, not applied per request.
func (r *Router) Use(mws ...func(http.Handler) http.Handler) {
	if r.built {
		r.errs = append(r.errs, errors.New("simdhttp: Use called after Build"))
		return
	}
	r.mws = append(r.mws, mws...)
}

// Build compiles the table and freezes the router. It reports every problem it
// found rather than the first, so a bad table is fixed in one pass.
func (r *Router) Build() error {
	if r.built {
		return errors.New("simdhttp: Build called twice")
	}
	parsed := make([]*parsedRoute, 0, len(r.regs))
	for _, reg := range r.regs {
		p, err := parsePattern(reg.method, reg.pattern)
		if err != nil {
			r.errs = append(r.errs, err)
			continue
		}
		p.h = reg.h
		p.idx = len(parsed)
		parsed = append(parsed, p)
	}
	r.detectConflicts(parsed)
	r.assignMethodIDs(parsed)
	for _, p := range parsed {
		if err := r.insert(p); err != nil {
			r.errs = append(r.errs, err)
		}
	}
	r.allowAll = r.allMethodNames()
	for host := range r.roots {
		if host != "" {
			r.hostRoutes = true
			break
		}
	}
	r.built = true
	if len(r.errs) > 0 {
		return errors.Join(r.errs...)
	}
	return nil
}

// allMethodNames is the Allow value for an asterisk-form OPTIONS: every method
// the table registers anywhere, with the implicit HEAD that GET implies.
func (r *Router) allMethodNames() string {
	seen := map[string]bool{}
	for _, reg := range r.regs {
		if reg.method == "" {
			continue
		}
		seen[reg.method] = true
		if reg.method == "GET" {
			seen["HEAD"] = true
		}
	}
	names := make([]string, 0, len(seen))
	for m := range seen {
		names = append(names, m)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// MustBuild builds and panics on error. It is the only panic path for a bad
// table, and exists so tests and top-level setup can assert a table is valid.
func (r *Router) MustBuild() *Router {
	if err := r.Build(); err != nil {
		panic(err)
	}
	return r
}

// ---- pattern parsing ----

type segKind uint8

const (
	// segLit matches one path segment exactly. Its text is empty only for the
	// "{$}" end-of-path marker, which matches the empty segment a trailing
	// slash produces.
	segLit segKind = iota
	// segSingle matches one non-empty path segment. Probed: ServeMux answers
	// 404 for "/a/" against "GET /a/{x}".
	segSingle
	// segMulti matches one or more remaining segments and is always final. A
	// pattern ending in "/" gets one implicitly, which is what makes "/users/"
	// a subtree rather than an exact path.
	segMulti
)

type segment struct {
	kind segKind
	text string // the literal, or the wildcard name ("" binds nothing)
}

type parsedRoute struct {
	method  string
	host    string
	segs    []segment
	pattern string
	names   []string // wildcard names in match order
	h       http.Handler
	idx     int
}

// parsePattern validates a registration and splits it into segments. Every
// rejection here is a shape that would otherwise have two readings at match
// time; the router refuses to pick one of them silently.
func parsePattern(method, pattern string) (*parsedRoute, error) {
	p := &parsedRoute{method: method, pattern: pattern}
	switch {
	case method == "":
		return p, fmt.Errorf("simdhttp: pattern %q has no method", pattern)
	case !isToken(method):
		return p, fmt.Errorf("simdhttp: %q is not a valid method token", method)
	case pattern == "":
		return p, errors.New("simdhttp: empty pattern")
	}
	route := pattern
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		// Lower-cased once here rather than per request: a host is
		// case-insensitive, and a table that behaved differently depending on
		// how its patterns were typed would be a trap.
		p.host, route = strings.ToLower(pattern[:i]), pattern[i:]
	}
	if route == "" || route[0] != '/' {
		return p, fmt.Errorf("simdhttp: pattern %q has no leading slash", pattern)
	}
	trailingSlash := strings.HasSuffix(route, "/")
	seen := make(map[string]bool, 4)
	for rest := route[1:]; ; {
		var text string
		more := false
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			text, rest, more = rest[:j], rest[j+1:], true
		} else {
			text = rest
		}
		s, err := parseSegment(text, pattern)
		if err != nil {
			return p, err
		}
		if more && s.kind == segLit && s.text == "" {
			// An empty segment anywhere but the end is unreachable: request
			// paths are cleaned before matching, so "//" never arrives.
			return p, fmt.Errorf("simdhttp: pattern %q has an empty segment", pattern)
		}
		if more && s.kind == segMulti {
			return p, fmt.Errorf("simdhttp: pattern %q has a wildcard before the final segment", pattern)
		}
		if s.kind == segSingle || s.kind == segMulti {
			if s.text != "" {
				if seen[s.text] {
					return p, fmt.Errorf("simdhttp: pattern %q repeats parameter %q", pattern, s.text)
				}
				seen[s.text] = true
			}
			p.names = append(p.names, s.text)
		}
		p.segs = append(p.segs, s)
		if !more {
			break
		}
	}
	if trailingSlash {
		// The empty final segment a trailing slash produced becomes the
		// subtree wildcard: "/users/" matches "/users/", "/users/x" and
		// "/users/x/y". Probed against ServeMux, which reads it the same way,
		// and which is why "/" matches every path.
		last := len(p.segs) - 1
		if p.segs[last].kind == segLit && p.segs[last].text == "" {
			p.segs[last] = segment{kind: segMulti}
			p.names = append(p.names, "")
		}
	}
	return p, nil
}

func parseSegment(seg, pattern string) (segment, error) {
	switch {
	case seg == "*":
		return segment{kind: segMulti, text: "*"}, nil
	case seg == "{$}":
		// The end-of-path marker: "/users/{$}" matches only "/users/".
		return segment{kind: segLit, text: ""}, nil
	case strings.HasPrefix(seg, "{"):
		if !strings.HasSuffix(seg, "}") || len(seg) < 2 {
			return segment{}, fmt.Errorf("simdhttp: pattern %q has an unterminated parameter", pattern)
		}
		name := seg[1 : len(seg)-1]
		if name == "" {
			return segment{}, fmt.Errorf("simdhttp: pattern %q has an empty parameter name", pattern)
		}
		if strings.ContainsAny(name, "{}") {
			return segment{}, fmt.Errorf("simdhttp: pattern %q has a malformed parameter", pattern)
		}
		return segment{kind: segSingle, text: name}, nil
	case strings.ContainsAny(seg, "{}"):
		// "{id}.json" and "v{id}": a parameter sharing a segment with a
		// literal has more than one reading, so v1 rejects it rather than
		// fixing one of them in place.
		return segment{}, fmt.Errorf("simdhttp: pattern %q mixes a parameter and a literal in one segment", pattern)
	case strings.Contains(seg, "*"):
		return segment{}, fmt.Errorf("simdhttp: pattern %q mixes a wildcard and a literal in one segment", pattern)
	default:
		lit, err := decodeSegment(seg)
		if err != nil {
			return segment{}, fmt.Errorf("simdhttp: pattern %q has an invalid escape", pattern)
		}
		return segment{kind: segLit, text: lit}, nil
	}
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x80 || !methodTokenChar[c] {
			return false
		}
	}
	return true
}

var methodTokenChar = func() (t [128]bool) {
	for _, c := range []byte("!#$%&'*+-.^_`|~") {
		t[c] = true
	}
	for c := byte('0'); c <= '9'; c++ {
		t[c] = true
	}
	for c := byte('a'); c <= 'z'; c++ {
		t[c] = true
	}
	for c := byte('A'); c <= 'Z'; c++ {
		t[c] = true
	}
	return t
}()

// ---- conflict detection ----

// Two patterns conflict when some request matches both and neither is more
// specific: there is no rule that picks one, so accepting the table would mean
// choosing silently. ServeMux panics on these at registration; this reports
// them from Build, which is the same detection delivered differently.
type relationship uint8

const (
	relEquivalent relationship = iota
	relMoreGeneral
	relMoreSpecific
	relOverlaps
	relDisjoint
)

func combineRel(a, b relationship) relationship {
	switch {
	case a == relDisjoint || b == relDisjoint:
		return relDisjoint
	case a == relEquivalent:
		return b
	case b == relEquivalent:
		return a
	case a == b:
		return a
	default:
		// One position says more general, another says more specific, so
		// neither pattern contains the other.
		return relOverlaps
	}
}

// compareSegs relates two non-multi segments at the same position.
func compareSegs(s1, s2 segment) relationship {
	switch {
	case s1.kind == segLit && s2.kind == segLit:
		if s1.text == s2.text {
			return relEquivalent
		}
		return relDisjoint
	case s1.kind == segLit:
		if s1.text == "" {
			return relDisjoint // {$} matches the empty segment; {x} never does
		}
		return relMoreSpecific
	case s2.kind == segLit:
		if s2.text == "" {
			return relDisjoint
		}
		return relMoreGeneral
	default:
		return relEquivalent // both single
	}
}

func comparePaths(p1, p2 *parsedRoute) relationship {
	rel := relEquivalent
	i := 0
	for i < len(p1.segs) && i < len(p2.segs) {
		s1, s2 := p1.segs[i], p2.segs[i]
		m1, m2 := s1.kind == segMulti, s2.kind == segMulti
		switch {
		case m1 && m2:
			return combineRel(rel, relEquivalent)
		case m1:
			// p1 takes every remaining segment; p2 needs at least the ones it
			// still lists, so p2's paths are a subset of p1's.
			return combineRel(rel, relMoreGeneral)
		case m2:
			return combineRel(rel, relMoreSpecific)
		}
		rel = combineRel(rel, compareSegs(s1, s2))
		if rel == relDisjoint {
			return relDisjoint
		}
		i++
	}
	if i == len(p1.segs) && i == len(p2.segs) {
		return rel
	}
	// One ran out without a multi, so it matches paths of exactly its own
	// length while the other needs more segments.
	return relDisjoint
}

// pairwiseLimit is the group size below which conflicts are checked directly.
// Above it the group splits on the literal at the current position, which is
// what keeps a hundred-thousand-route table from costing a quadratic Build.
const pairwiseLimit = 32

func (r *Router) detectConflicts(routes []*parsedRoute) {
	type key struct{ method, host string }
	groups := map[key][]*parsedRoute{}
	for _, p := range routes {
		k := key{p.method, p.host}
		groups[k] = append(groups[k], p)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	// Sorted so a build reports the same errors in the same order every time.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].host < keys[j].host
	})
	seen := map[[2]int]bool{}
	for _, k := range keys {
		r.scanGroup(groups[k], 0, seen)
	}
}

func (r *Router) scanGroup(g []*parsedRoute, depth int, seen map[[2]int]bool) {
	if len(g) < 2 {
		return
	}
	if len(g) <= pairwiseLimit {
		r.pairwise(g, seen)
		return
	}
	// Patterns with a literal here split into buckets that cannot match each
	// other. Anything else at this position -- a parameter, a wildcard, or a
	// pattern that has ended -- can match alongside any bucket, so it joins
	// all of them.
	lits := map[string][]*parsedRoute{}
	var wild []*parsedRoute
	for _, p := range g {
		if depth < len(p.segs) && p.segs[depth].kind == segLit {
			lits[p.segs[depth].text] = append(lits[p.segs[depth].text], p)
		} else {
			wild = append(wild, p)
		}
	}
	if len(lits) < 2 {
		r.pairwise(g, seen) // no split available; compare directly
		return
	}
	for _, b := range lits {
		sub := b
		if len(wild) > 0 {
			sub = append(append(make([]*parsedRoute, 0, len(b)+len(wild)), b...), wild...)
		}
		r.scanGroup(sub, depth+1, seen)
	}
	r.scanGroup(wild, depth+1, seen)
}

func (r *Router) pairwise(g []*parsedRoute, seen map[[2]int]bool) {
	for i := 0; i < len(g); i++ {
		for j := i + 1; j < len(g); j++ {
			a, b := g[i], g[j]
			k := [2]int{a.idx, b.idx}
			if k[0] > k[1] {
				k[0], k[1] = k[1], k[0]
			}
			if seen[k] {
				continue
			}
			seen[k] = true
			if rel := comparePaths(a, b); rel == relEquivalent || rel == relOverlaps {
				r.errs = append(r.errs, fmt.Errorf(
					"simdhttp: %s %q conflicts with %s %q: both match some paths and neither is more specific",
					a.method, a.pattern, b.method, b.pattern))
			}
		}
	}
}

// ---- method IDs ----

// The nine methods below own dense fixed IDs, resolved by an inline compare
// chain rather than a hash: they are what almost every request uses, and a map
// lookup per request is a cost every route would pay for a rare case.
const numCommonMethods = 9

// The IDs the serving path names directly.
const (
	methodIDGet  = 0
	methodIDHead = 1
)

func commonMethodID(m string) int {
	switch len(m) {
	case 3:
		switch m {
		case "GET":
			return 0
		case "PUT":
			return 3
		}
	case 4:
		switch m {
		case "HEAD":
			return 1
		case "POST":
			return 2
		}
	case 5:
		switch m {
		case "PATCH":
			return 4
		case "TRACE":
			return 8
		}
	case 6:
		if m == "DELETE" {
			return 5
		}
	case 7:
		switch m {
		case "OPTIONS":
			return 6
		case "CONNECT":
			return 7
		}
	}
	return -1
}

var commonMethodNames = [numCommonMethods]string{
	"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE",
}

// assignMethodIDs gives every custom method a dense ID above the common nine,
// so a node's handler lookup is a slice index rather than a map lookup.
func (r *Router) assignMethodIDs(routes []*parsedRoute) {
	r.numMethods = numCommonMethods
	for _, p := range routes {
		if commonMethodID(p.method) >= 0 {
			continue
		}
		if r.methodIDs == nil {
			r.methodIDs = map[string]int{}
		}
		if _, ok := r.methodIDs[p.method]; ok {
			continue
		}
		r.methodIDs[p.method] = r.numMethods
		r.numMethods++
	}
}

// methodID resolves a request method to its dense ID, or -1.
func (r *Router) methodID(m string) int {
	if id := commonMethodID(m); id >= 0 {
		return id
	}
	if r.methodIDs != nil {
		if id, ok := r.methodIDs[m]; ok {
			return id
		}
	}
	return -1
}

// ---- trie construction ----

func (r *Router) insert(p *parsedRoute) error {
	root := r.roots[p.host]
	if root == nil {
		root = &node{}
		r.roots[p.host] = root
	}
	n := root
	for _, s := range p.segs {
		switch s.kind {
		case segLit:
			if n.static == nil {
				n.static = map[string]*node{}
			}
			next := n.static[s.text]
			if next == nil {
				next = &node{}
				n.static[s.text] = next
			}
			n = next
		case segSingle:
			if n.single == nil {
				n.single = &node{}
			}
			n = n.single
		case segMulti:
			if n.multi == nil {
				n.multi = &node{}
			}
			n = n.multi
		}
	}
	id := r.methodID(p.method)
	if id < 0 {
		return fmt.Errorf("simdhttp: method %q has no ID", p.method)
	}
	if n.handlers == nil {
		n.handlers = make([]*handlerEntry, r.numMethods)
	}
	if n.handlers[id] != nil {
		// detectConflicts already reported this pair; keeping the first
		// registration leaves the table deterministic either way.
		return nil
	}
	n.handlers[id] = &handlerEntry{h: r.wrap(p.h), names: p.names, pattern: p.pattern}
	n.methods = append(n.methods, p.method)
	sort.Strings(n.methods)
	return nil
}

// wrap applies the middleware stack once, at Build, so a request pays for the
// chain's calls but never for assembling it.
func (r *Router) wrap(h http.Handler) http.Handler {
	for i := len(r.mws) - 1; i >= 0; i-- {
		h = r.mws[i](h)
	}
	return h
}

// ---- the match loop ----

// maxInlineParams is how many bindings a walk records without touching the
// heap. Beyond it the walk still matches; it stops being allocation-free.
const maxInlineParams = 8

// ServeHTTP walks the compiled trie: literal, then parameter, then wildcard,
// at each position, backtracking when a more specific edge leads nowhere.
// Nothing on this path allocates -- segments are substrings of the request's
// own path, bindings live in a stack array, and the method is an array index.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !r.built {
		// A 404 here would answer "no such route" for a table that was never
		// compiled, presenting a setup mistake as a routing decision.
		http.Error(w, "simdhttp: router used before Build", http.StatusInternalServerError)
		return
	}
	escaped := escapedPath(req)
	id := r.methodID(req.Method)
	if req.Method == "OPTIONS" && (escaped == "*" || req.RequestURI == "*") {
		// RFC 9110 asterisk-form: a request about the server rather than a
		// resource, so it never reaches the trie. A standard http.Server
		// answers it before the handler unless DisableGeneralOptionsHandler is
		// set, which is why this is reachable at all.
		w.Header().Set("Allow", r.allowAll)
		w.WriteHeader(http.StatusOK)
		return
	}
	reqPath := escaped
	if req.Method != "CONNECT" {
		// CONNECT names an authority rather than a path, so it is not
		// canonicalized -- the same exception ServeMux makes.
		reqPath = cleanPath(escaped)
	}
	if reqPath == "" || reqPath[0] != '/' {
		http.NotFound(w, req)
		return
	}

	root := r.hostRoot(req.Host)
	var vals [maxInlineParams]string
	var n *node
	var nv int
	if root != nil {
		n, nv = matchSegments(root, reqPath[1:], id, &vals, 0)
	}
	if n == nil && root != r.roots[""] {
		// A host-scoped table that does not answer falls back to the patterns
		// registered for any host. The reverse never happens: a request whose
		// authority names one host is not served by another host's routes.
		root = r.roots[""]
		if root != nil {
			n, nv = matchSegments(root, reqPath[1:], id, &vals, 0)
		}
	}
	if root == nil {
		http.NotFound(w, req)
		return
	}

	// A trailing-slash redirect is decided on the cleaned path, and only then
	// is an unclean path redirected to its clean form. That is ServeMux's
	// order, probed.
	if n == nil && r.RedirectTrailingSlash && !strings.HasSuffix(reqPath, "/") {
		if lookupPath(root, reqPath+"/", id) != nil {
			// ServeMux builds this Location from the DECODED path, unlike the
			// clean-path redirect below which builds it from the escaped one.
			// The two therefore escape differently, and both were probed.
			target := reqPath + "/"
			if req.URL != nil {
				target = cleanPath(req.URL.Path) + "/"
			}
			redirect(w, req, target)
			return
		}
	}
	if reqPath != escaped {
		redirect(w, req, reqPath)
		return
	}
	if n == nil {
		r.serveNoRoute(w, req, root, reqPath, id)
		return
	}
	if id >= 0 && id < len(n.handlers) && n.handlers[id] != nil {
		serveEntry(w, req, n.handlers[id], &vals, nv)
		return
	}
	// HEAD is served by GET when no HEAD pattern exists. The handler must not
	// write a body; that is the standard contract, not something the router can
	// enforce without wrapping the ResponseWriter.
	if id == methodIDHead && n.handlers[methodIDGet] != nil {
		serveEntry(w, req, n.handlers[methodIDGet], &vals, nv)
		return
	}
	r.methodNotAllowed(w, req, root, reqPath)
}

// serveEntry binds the values this walk collected and runs the handler. The
// names come from the matched entry rather than the node, so two methods
// registered on the same shape with different names each get their own.
func serveEntry(w http.ResponseWriter, req *http.Request, e *handlerEntry, vals *[maxInlineParams]string, nv int) {
	for i := 0; i < nv && i < len(e.names); i++ {
		if e.names[i] != "" {
			req.SetPathValue(e.names[i], vals[i])
		}
	}
	e.h.ServeHTTP(w, req)
}

func redirect(w http.ResponseWriter, req *http.Request, target string) {
	// Built through url.URL exactly as ServeMux does, including the second
	// escaping pass it performs on an already-escaped path. Matching the
	// reference byte for byte matters more here than tidiness: a Location a
	// client follows differently is a different request.
	u := &url.URL{Path: target}
	if req.URL != nil {
		u.RawQuery = req.URL.RawQuery
	}
	http.Redirect(w, req, u.String(), http.StatusTemporaryRedirect)
}

// serveNoRoute handles a path no pattern covers: the trailing-slash variant
// with a method mismatch, or a plain 404.
func (r *Router) serveNoRoute(w http.ResponseWriter, req *http.Request, root *node, reqPath string, id int) {
	// The method-filtered walk found nothing. If some other method has a
	// pattern for this path, the path exists and only the method is wrong.
	if lookupPath(root, reqPath, -1) != nil {
		r.methodNotAllowed(w, req, root, reqPath)
		return
	}
	// The slash form may carry the patterns instead. Redirecting there would
	// send the client to a URL that refuses it just the same, so the answer is
	// the 405 that URL would give.
	if r.RedirectTrailingSlash && !strings.HasSuffix(reqPath, "/") &&
		lookupPath(root, reqPath+"/", -1) != nil {
		r.methodNotAllowed(w, req, root, reqPath)
		return
	}
	http.NotFound(w, req)
}

// methodNotAllowed answers a path that matched with methods the request did
// not use. The Allow value unions the trailing-slash variant when the request
// path lacks one, which is what ServeMux's matchingMethods does.
func (r *Router) methodNotAllowed(w http.ResponseWriter, req *http.Request, root *node, reqPath string) {
	w.Header().Set("Allow", allowValue(root, reqPath))
	http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
}

// allowValue builds the Allow header: the registered methods sorted
// lexicographically, plus an implicit HEAD wherever GET is registered. There
// is no implicit OPTIONS -- ServeMux does not add one, and a router that
// advertised a method it would then refuse would be worse than silent. This is
// the cold path and may allocate.
func allowValue(root *node, reqPath string) string {
	set := map[string]bool{}
	if root != nil && len(reqPath) > 0 && reqPath[0] == '/' {
		collectMethods(root, reqPath[1:], set)
		if !strings.HasSuffix(reqPath, "/") {
			// ServeMux unions the trailing-slash variant when the request path
			// lacks one, so a POST registered only on /users/ is named in the
			// Allow for /users.
			collectMethods(root, reqPath[1:]+"/", set)
		}
	}
	names := make([]string, 0, len(set))
	for m := range set {
		names = append(names, m)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// lookupPath finds a node without binding parameters, for the Allow and
// trailing-slash decisions.
func lookupPath(root *node, p string, want int) *node {
	if p == "" || p[0] != '/' {
		return nil
	}
	var scratch [maxInlineParams]string
	n, _ := matchSegments(root, p[1:], want, &scratch, 0)
	return n
}

// hostRoot returns the trie for a request's authority, or the hostless trie.
// The host is compared without its port and case-insensitively; a request with
// no Host matches hostless patterns only, because an HTTP/1.0 client named no
// authority and picking one for it would be a guess.
func (r *Router) hostRoot(host string) *node {
	if !r.hostRoutes || host == "" {
		return r.roots[""]
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.IndexByte(host[i:], ']') < 0 {
		host = host[:i] // strip the port, leaving a bracketed IPv6 literal alone
	}
	// ToLower returns the input unchanged when it already is lower case, so an
	// ordinary request pays no allocation for this.
	if n := r.roots[strings.ToLower(host)]; n != nil {
		return n
	}
	return r.roots[""]
}

// escapedPath returns the path in its on-the-wire form so each segment is
// decoded exactly once. Matching the pre-decoded path would treat an encoded
// %2F as a separator and route the request somewhere the client did not ask.
func escapedPath(req *http.Request) string {
	if req.URL == nil {
		return "/"
	}
	return req.URL.EscapedPath()
}

// matchSegments consumes one segment of rest and recurses, trying literal,
// then parameter, then wildcard. It backtracks: a literal edge that leads to a
// dead end does not prevent a parameter edge from matching the same request.
func matchSegments(n *node, rest string, want int, vals *[maxInlineParams]string, nv int) (*node, int) {
	var seg string
	more := false
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		seg, more = rest[:j], true
	} else {
		seg = rest
	}
	dec, ok := decodeSegmentFast(seg)
	if !ok {
		return nil, 0
	}
	var tail string
	if more {
		tail = rest[len(seg)+1:]
	}

	if n.static != nil {
		if next := n.static[dec]; next != nil {
			if !more {
				if serves(next, want) {
					return next, nv
				}
			} else if got, gnv := matchSegments(next, tail, want, vals, nv); got != nil {
				return got, gnv
			}
		}
	}
	if n.single != nil && dec != "" {
		// A parameter does not match an empty segment. Probed on Go 1.26.2:
		// ServeMux answers 404 for "/a/" against "GET /a/{x}", so matching it
		// here would route a request the origin would have refused.
		snv := nv
		if snv < maxInlineParams {
			vals[snv] = dec
			snv++
		}
		if !more {
			if serves(n.single, want) {
				return n.single, snv
			}
		} else if got, gnv := matchSegments(n.single, tail, want, vals, snv); got != nil {
			return got, gnv
		}
	}
	if n.multi != nil && serves(n.multi, want) {
		// The wildcard takes the whole remainder from this position, slashes
		// included. rest is already that remainder, so binding it costs no
		// allocation.
		whole, ok := decodeSegmentFast(rest)
		if !ok {
			return nil, 0
		}
		if nv < maxInlineParams {
			vals[nv] = whole
			nv++
		}
		return n.multi, nv
	}
	return nil, 0
}

// serves reports whether a node answers the wanted method. want < 0 means any
// method, which is how the Allow walk and the trailing-slash probe ask.
func serves(n *node, want int) bool {
	if n.handlers == nil {
		return false
	}
	if want < 0 {
		return true
	}
	if want < len(n.handlers) && n.handlers[want] != nil {
		return true
	}
	// A HEAD request is answered by the GET pattern, so a node carrying only
	// GET still matches it.
	return want == methodIDHead && n.handlers[methodIDGet] != nil
}

// collectMethods unions the methods of every pattern that matches this path,
// which is what the Allow header names. ServeMux enumerates the same set: not
// the methods of the one node that won, but of every node that could have.
func collectMethods(n *node, rest string, set map[string]bool) {
	var seg string
	more := false
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		seg, more = rest[:j], true
	} else {
		seg = rest
	}
	dec, ok := decodeSegmentFast(seg)
	if !ok {
		return
	}
	var tail string
	if more {
		tail = rest[len(seg)+1:]
	}
	if n.static != nil {
		if next := n.static[dec]; next != nil {
			if !more {
				addMethods(next, set)
			} else {
				collectMethods(next, tail, set)
			}
		}
	}
	if n.single != nil && dec != "" {
		if !more {
			addMethods(n.single, set)
		} else {
			collectMethods(n.single, tail, set)
		}
	}
	if n.multi != nil {
		addMethods(n.multi, set)
	}
}

func addMethods(n *node, set map[string]bool) {
	for _, m := range n.methods {
		set[m] = true
		if m == "GET" {
			// GET implies HEAD, which is why Allow lists a method no pattern
			// registered. ServeMux does the same.
			set["HEAD"] = true
		}
	}
}

// cleanPath returns the shortest equivalent path, keeping a trailing slash. An
// unclean path is answered with a redirect rather than matched, so "/a//b" and
// "/a/./b" cannot reach a different handler than "/a/b" would.
func cleanPath(p string) string {
	if !needsClean(p) {
		return p
	}
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// needsClean reports whether a path could shorten. The common path has no "//"
// and no "/." and returns here, so an ordinary request never pays for Clean.
func needsClean(p string) bool {
	if p == "" || p[0] != '/' {
		return true
	}
	for i := 0; i+1 < len(p); i++ {
		if p[i] == '/' && (p[i+1] == '/' || p[i+1] == '.') {
			return true
		}
	}
	return p[len(p)-1] == '.'
}

// decodeSegmentFast resolves percent-escapes. The common case has no '%' and
// returns the input unchanged, so a match costs no allocation; only an escaped
// segment pays.
func decodeSegmentFast(seg string) (string, bool) {
	if strings.IndexByte(seg, '%') < 0 {
		return seg, true
	}
	s, err := decodeSegment(seg)
	if err != nil {
		return "", false
	}
	return s, true
}

// decodeSegment resolves percent-escapes within a path segment. A '+' stays a
// plus: it means a space in a query string, never in a path.
func decodeSegment(seg string) (string, error) {
	n := 0
	for i := 0; i < len(seg); i++ {
		if seg[i] == '%' {
			if i+2 >= len(seg) || !ishex(seg[i+1]) || !ishex(seg[i+2]) {
				return "", errors.New("simdhttp: invalid percent-escape")
			}
			n++
			i += 2
		}
	}
	if n == 0 {
		return seg, nil
	}
	out := make([]byte, 0, len(seg)-2*n)
	for i := 0; i < len(seg); i++ {
		if seg[i] == '%' {
			out = append(out, unhex(seg[i+1])<<4|unhex(seg[i+2]))
			i += 2
			continue
		}
		out = append(out, seg[i])
	}
	return string(out), nil
}

func ishex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
