package simdhttp

import (
	"errors"
	"fmt"
	"net/http"
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

// Router matches requests to handlers. The zero value is not usable; call New.
type Router struct {
	// RedirectTrailingSlash redirects a request for /users to /users/ when
	// only the slash form is registered. Set it before Build.
	RedirectTrailingSlash bool

	regs  []registration
	mws   []func(http.Handler) http.Handler
	errs  []error
	built bool

	roots      map[string]*node // keyed by the pattern's host; "" matches any
	methodIDs  map[string]int   // custom methods only; IDs from numCommonMethods
	numMethods int
	allowAll   string // every method in the table, for asterisk-form OPTIONS
}

type registration struct {
	method  string
	pattern string
	h       http.Handler
}

// node is one segment position in the compiled trie.
type node struct {
	static    map[string]*node
	param     *node
	paramName string
	wildcard  *node
	handlers  []http.Handler // indexed by method ID; nil means not registered
	methods   []string       // registered names, sorted, for the 405 Allow value
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
	parsed := make([]parsedRoute, 0, len(r.regs))
	for _, reg := range r.regs {
		p, err := parsePattern(reg.method, reg.pattern)
		if err != nil {
			r.errs = append(r.errs, err)
			continue
		}
		p.h = reg.h
		parsed = append(parsed, p)
	}
	r.assignMethodIDs(parsed)
	for i := range parsed {
		if err := r.insert(&parsed[i]); err != nil {
			r.errs = append(r.errs, err)
		}
	}
	r.allowAll = r.allMethodNames()
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
	segStatic segKind = iota
	segParam
	segWildcard
)

type segment struct {
	kind segKind
	text string // the literal, or the parameter name
}

type parsedRoute struct {
	method  string
	host    string
	segs    []segment
	pattern string
	h       http.Handler
}

// parsePattern validates a registration and splits it into segments. Every
// rejection here is a shape that would otherwise have two readings at match
// time; the router refuses to pick one of them silently.
func parsePattern(method, pattern string) (parsedRoute, error) {
	p := parsedRoute{method: method, pattern: pattern}
	switch {
	case method == "":
		return p, fmt.Errorf("simdhttp: pattern %q has no method", pattern)
	case !isToken(method):
		return p, fmt.Errorf("simdhttp: %q is not a valid method token", method)
	case pattern == "":
		return p, errors.New("simdhttp: empty pattern")
	}
	path := pattern
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		p.host, path = pattern[:i], pattern[i:]
	}
	if path == "" || path[0] != '/' {
		return p, fmt.Errorf("simdhttp: pattern %q has no leading slash", pattern)
	}
	seen := make(map[string]bool, 4)
	for rest := path[1:]; ; {
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
		if s.kind == segParam {
			if seen[s.text] {
				return p, fmt.Errorf("simdhttp: pattern %q repeats parameter %q", pattern, s.text)
			}
			seen[s.text] = true
		}
		if s.kind == segWildcard && more {
			return p, fmt.Errorf("simdhttp: pattern %q has a wildcard before the final segment", pattern)
		}
		p.segs = append(p.segs, s)
		if !more {
			return p, nil
		}
	}
}

func parseSegment(seg, pattern string) (segment, error) {
	switch {
	case seg == "*":
		return segment{kind: segWildcard, text: "*"}, nil
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
		return segment{kind: segParam, text: name}, nil
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
		return segment{kind: segStatic, text: lit}, nil
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

// ---- method IDs ----

// The nine methods below own dense fixed IDs, resolved by an inline compare
// chain rather than a hash: they are what almost every request uses, and a map
// lookup per request is a cost every route would pay for a rare case.
const numCommonMethods = 9

// The three IDs the serving path names directly.
const (
	methodIDGet     = 0
	methodIDHead    = 1
	methodIDOptions = 6
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
func (r *Router) assignMethodIDs(routes []parsedRoute) {
	r.numMethods = numCommonMethods
	for i := range routes {
		m := routes[i].method
		if commonMethodID(m) >= 0 {
			continue
		}
		if r.methodIDs == nil {
			r.methodIDs = map[string]int{}
		}
		if _, ok := r.methodIDs[m]; ok {
			continue
		}
		r.methodIDs[m] = r.numMethods
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
		case segStatic:
			if n.static == nil {
				n.static = map[string]*node{}
			}
			next := n.static[s.text]
			if next == nil {
				next = &node{}
				n.static[s.text] = next
			}
			n = next
		case segParam:
			if n.param == nil {
				n.param = &node{}
				n.paramName = s.text
			}
			n = n.param
		case segWildcard:
			if n.wildcard == nil {
				n.wildcard = &node{}
			}
			n = n.wildcard
		}
	}
	id := r.methodID(p.method)
	if id < 0 {
		return fmt.Errorf("simdhttp: method %q has no ID", p.method)
	}
	if n.handlers == nil {
		n.handlers = make([]http.Handler, r.numMethods)
	}
	if n.handlers[id] != nil {
		// Two patterns reaching the same node with the same method match the
		// same requests at equal precedence. Choosing one silently is how a
		// route table starts lying about which handler runs.
		return fmt.Errorf("simdhttp: %s %q conflicts with an earlier pattern", p.method, p.pattern)
	}
	n.handlers[id] = r.wrap(p.h)
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

type paramKV struct{ name, value string }

// ServeHTTP walks the compiled trie: static, then parameter, then wildcard, at
// each position, backtracking when a more specific edge leads nowhere. Nothing
// on this path allocates -- segments are substrings of the request's own path,
// bindings live in a stack array, and the method resolves to an array index.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !r.built {
		// A 404 here would answer "no such route" for a table that was never
		// compiled, presenting a setup mistake as a routing decision.
		http.Error(w, "simdhttp: router used before Build", http.StatusInternalServerError)
		return
	}
	root := r.roots[""]
	if root == nil {
		http.NotFound(w, req)
		return
	}
	path := escapedPath(req)
	if req.Method == "OPTIONS" && (path == "*" || req.RequestURI == "*") {
		// RFC 9110 asterisk-form: a request about the server rather than a
		// resource, so it never reaches the trie. A standard http.Server
		// answers it before the handler unless DisableGeneralOptionsHandler
		// is set, which is why this is reachable at all.
		w.Header().Set("Allow", r.allowAll)
		w.WriteHeader(http.StatusOK)
		return
	}
	if path == "" || path[0] != '/' {
		http.NotFound(w, req)
		return
	}
	var params [maxInlineParams]paramKV
	n, np := matchSegments(root, path[1:], &params, 0)
	id := r.methodID(req.Method)
	if n == nil {
		r.serveNoRoute(w, req, root, path, id)
		return
	}
	if id >= 0 && id < len(n.handlers) && n.handlers[id] != nil {
		for i := 0; i < np; i++ {
			req.SetPathValue(params[i].name, params[i].value)
		}
		n.handlers[id].ServeHTTP(w, req)
		return
	}
	// HEAD is served by GET when no HEAD pattern exists. The handler must not
	// write a body; that is the standard contract, not something the router
	// can enforce without wrapping the ResponseWriter.
	if id == methodIDHead && n.handlers[methodIDGet] != nil {
		for i := 0; i < np; i++ {
			req.SetPathValue(params[i].name, params[i].value)
		}
		n.handlers[methodIDGet].ServeHTTP(w, req)
		return
	}
	r.methodNotAllowed(w, req, root, path, n)
}

// serveNoRoute handles a path with no matching node: the asterisk-form
// OPTIONS, the trailing-slash variant, or a plain 404.
func (r *Router) serveNoRoute(w http.ResponseWriter, req *http.Request, root *node, path string, id int) {
	if !r.RedirectTrailingSlash || strings.HasSuffix(path, "/") {
		http.NotFound(w, req)
		return
	}
	slash := lookupPath(root, path+"/")
	if slash == nil {
		http.NotFound(w, req)
		return
	}
	if id >= 0 && id < len(slash.handlers) && slash.handlers[id] != nil {
		target := path + "/"
		if req.URL.RawQuery != "" {
			target += "?" + req.URL.RawQuery
		}
		http.Redirect(w, req, target, http.StatusTemporaryRedirect)
		return
	}
	// The slash form exists but has no pattern for this method. Redirecting
	// would send the client to a URL that refuses it just the same, so the
	// answer is the 405 that URL would give.
	w.Header().Set("Allow", allowValue(nil, slash))
	http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
}

// methodNotAllowed answers a path that matched with methods the request did
// not use. The Allow value unions the trailing-slash variant when the request
// path lacks one, which is what ServeMux's matchingMethods does.
func (r *Router) methodNotAllowed(w http.ResponseWriter, req *http.Request, root *node, path string, n *node) {
	var slash *node
	if !strings.HasSuffix(path, "/") {
		slash = lookupPath(root, path+"/")
	}
	w.Header().Set("Allow", allowValue(n, slash))
	http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
}

// allowValue builds the Allow header: the registered methods sorted
// lexicographically, plus an implicit HEAD wherever GET is registered. There
// is no implicit OPTIONS -- ServeMux does not add one, and a router that
// advertised a method it would then refuse would be worse than silent. This is
// the cold path and may allocate.
func allowValue(a, b *node) string {
	names := make([]string, 0, 8)
	add := func(n *node) {
		if n == nil {
			return
		}
		for _, m := range n.methods {
			names = append(names, m)
			if m == "GET" {
				names = append(names, "HEAD")
			}
		}
	}
	add(a)
	add(b)
	sort.Strings(names)
	out := names[:0]
	for i, m := range names {
		if i == 0 || m != names[i-1] {
			out = append(out, m)
		}
	}
	return strings.Join(out, ", ")
}

// lookupPath finds a node without binding parameters, for the Allow and
// trailing-slash decisions.
func lookupPath(root *node, path string) *node {
	if path == "" || path[0] != '/' {
		return nil
	}
	var scratch [maxInlineParams]paramKV
	n, _ := matchSegments(root, path[1:], &scratch, 0)
	return n
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

// matchSegments consumes one segment of rest and recurses, trying static, then
// parameter, then wildcard. It backtracks: a static edge that leads to a dead
// end does not prevent a parameter edge from matching the same request.
func matchSegments(n *node, rest string, params *[maxInlineParams]paramKV, np int) (*node, int) {
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
				if next.handlers != nil {
					return next, np
				}
			} else if got, gnp := matchSegments(next, tail, params, np); got != nil {
				return got, gnp
			}
		}
	}
	if n.param != nil && dec != "" {
		// A parameter does not match an empty segment. Probed on Go 1.26.5:
		// ServeMux answers 404 for "/a/" against "GET /a/{x}", so matching it
		// here would route a request the origin would have refused.
		pnp := np
		if pnp < maxInlineParams {
			params[pnp] = paramKV{n.paramName, dec}
			pnp++
		}
		if !more {
			if n.param.handlers != nil {
				return n.param, pnp
			}
		} else if got, gnp := matchSegments(n.param, tail, params, pnp); got != nil {
			return got, gnp
		}
	}
	if n.wildcard != nil && n.wildcard.handlers != nil {
		// The wildcard takes the whole remainder from this position, slashes
		// included. rest is already that remainder, so binding it costs no
		// allocation.
		whole, ok := decodeSegmentFast(rest)
		if !ok {
			return nil, 0
		}
		if np < maxInlineParams {
			params[np] = paramKV{"*", whole}
			np++
		}
		return n.wildcard, np
	}
	return nil, 0
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
