// Package http serves mock packages over HTTP.
package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mockmint/mockmint/internal/observability"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/problem"
	"github.com/mockmint/mockmint/internal/state"
)

// Router is an immutable routing table for a set of packages. Build a new
// one and swap it into the Server to reload.
type Router struct {
	mux          *http.ServeMux
	log          *slog.Logger
	maxBodyBytes int64
	packages     []*pkg.Package

	state    state.Store
	traffic  *observability.Traffic
	metrics  *observability.Metrics
	recorder *Recorder
	proxies  map[*pkg.Package]*httputil.ReverseProxy
}

// RouterOptions are a Router's dependencies. Only MaxBodyBytes is required; a nil
// State gets a private in-memory store, and nil Traffic, Metrics or
// Recorder disable that feature. State, Traffic, Metrics and Recorder are
// shared across reloads by passing the same values to each new Router.
type RouterOptions struct {
	MaxBodyBytes int64
	Log          *slog.Logger
	State        state.Store
	Traffic      *observability.Traffic
	Metrics      *observability.Metrics
	Recorder     *Recorder
}

// route is one OpenAPI path template.
type route struct {
	pkg      *pkg.Package
	template string // OpenAPI path, e.g. /files/{name}.json
	segs     []segment
	ops      map[string]*pkg.Operation // by method
	allow    string                    // Allow header value
	// literal counts literal characters, used to prefer more specific
	// templates when several share a mux pattern.
	literal int
}

// segment describes how one path segment binds parameters.
type segment struct {
	wildcard string         // ServeMux wildcard name, "" for literal segments
	param    string         // parameter name when the segment is exactly {param}
	re       *regexp.Regexp // for mixed segments like {name}.json
	names    []string       // parameter names for re's groups
}

// NewRouter compiles packages into a Router. It fails on duplicate package
// mounts and on path templates that ServeMux cannot disambiguate.
func NewRouter(pkgs []*pkg.Package, o RouterOptions) (*Router, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.State == nil {
		o.State = state.NewMemory()
	}
	rt := &Router{mux: http.NewServeMux(), log: o.Log, maxBodyBytes: o.MaxBodyBytes, packages: pkgs,
		state: o.State, traffic: o.Traffic, metrics: o.Metrics, recorder: o.Recorder,
		proxies: map[*pkg.Package]*httputil.ReverseProxy{}}
	for _, p := range pkgs {
		if p.Proxy != nil {
			rt.proxies[p] = rt.newProxy(p)
		}
	}
	seen := map[string]string{}
	for _, p := range pkgs {
		id := p.Name + "@" + p.Version
		if prev, dup := seen[id]; dup {
			return nil, fmt.Errorf("packages %s and %s are both %s", prev, p.Source, id)
		}
		seen[id] = p.Source
	}

	groups := map[string][]*route{}
	var order []string
	for _, p := range pkgs {
		byPath := map[string]*route{}
		for _, op := range p.Operations {
			r := byPath[op.Path]
			if r == nil {
				pattern, segs, literal, err := convert(p.BasePath, op.Path)
				if err != nil {
					return nil, fmt.Errorf("package %s: %s: %w", p.Name, op.Path, err)
				}
				r = &route{pkg: p, template: op.Path, segs: segs, ops: map[string]*pkg.Operation{}, literal: literal}
				byPath[op.Path] = r
				if _, ok := groups[pattern]; !ok {
					order = append(order, pattern)
				}
				groups[pattern] = append(groups[pattern], r)
			}
			r.ops[op.Method] = op
		}
		for _, r := range byPath {
			r.allow = allowHeader(r.ops)
		}
	}

	for _, pattern := range order {
		routes := groups[pattern]
		// Most literal characters first, so /files/{id}.json wins over /files/{id}.
		slices.SortStableFunc(routes, func(a, b *route) int { return b.literal - a.literal })
		if err := rt.handle(pattern, routes); err != nil {
			return nil, err
		}
		// ServeMux redirects /users/x to /users/x/ when only the slash form
		// is registered; OpenAPI treats them as different paths, so claim
		// the slash-less form as unmatched unless a route owns it.
		if base, ok := strings.CutSuffix(pattern, "/{$}"); ok && base != "" {
			if _, owned := groups[base]; !owned {
				groups[base] = nil
				p := routes[0].pkg
				rt.mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) { rt.unmatched(w, r, p) })
			}
		}
	}

	// Packages that proxy unmatched routes own their whole base path.
	var rootProxy *pkg.Package
	for _, p := range pkgs {
		if p.Proxy == nil || !p.Proxy.OnUnmatchedRoute {
			continue
		}
		if p.BasePath == "" {
			if rootProxy != nil {
				return nil, fmt.Errorf("packages %s and %s both proxy unmatched routes at /", rootProxy.Name, p.Name)
			}
			rootProxy = p
			continue
		}
		handler := func(w http.ResponseWriter, r *http.Request) { rt.unmatched(w, r, p) }
		for _, pattern := range []string{p.BasePath + "/", p.BasePath} {
			if _, owned := groups[pattern]; owned {
				continue
			}
			if err := rt.handleFunc(pattern, handler); err != nil {
				return nil, fmt.Errorf("package %s proxy: %w", p.Name, err)
			}
		}
	}
	if err := rt.handleFunc("/", func(w http.ResponseWriter, r *http.Request) { rt.unmatched(w, r, rootProxy) }); err != nil {
		return nil, err
	}
	// Seed last: o.State is live, and a build that fails must not change it.
	// Seeding is idempotent, so rebuilding on reload keeps changed state.
	for _, p := range pkgs {
		if err := p.SeedState(context.Background(), o.State); err != nil {
			return nil, fmt.Errorf("package %s: %w", p.Name, err)
		}
	}
	return rt, nil
}

// Packages returns the packages the router serves.
func (rt *Router) Packages() []*pkg.Package { return rt.packages }

func (rt *Router) handle(pattern string, routes []*route) (err error) {
	defer func() {
		// ServeMux panics on conflicting or duplicate patterns.
		if v := recover(); v != nil {
			names := make([]string, 0, len(routes))
			for _, r := range routes {
				names = append(names, r.pkg.Name+" "+r.template)
			}
			err = fmt.Errorf("route %s (%s) conflicts with another route: %v", pattern, strings.Join(names, ", "), v)
		}
	}()
	rt.mux.Handle(pattern, &pathHandler{rt: rt, routes: routes})
	return nil
}

func (rt *Router) handleFunc(pattern string, h http.HandlerFunc) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("route %s conflicts with another route: %v", pattern, v)
		}
	}()
	rt.mux.HandleFunc(pattern, h)
	return nil
}

// ServeHTTP dispatches to the matching path handler, or answers 404,
// recording metrics and traffic.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.observe(w, r, rt.mux)
}

func (rt *Router) notFound(w http.ResponseWriter, r *http.Request) {
	problem.Write(w, r, problem.New(problem.TypeNotFound, http.StatusNotFound, "no mock operation matches "+r.URL.Path))
}

// convert turns an OpenAPI path template under basePath into a ServeMux
// pattern (without method). Parameter names are replaced by p0..pN because
// OpenAPI names need not be valid wildcard names.
func convert(basePath, tmpl string) (pattern string, segs []segment, literal int, err error) {
	if !strings.HasPrefix(tmpl, "/") {
		return "", nil, 0, fmt.Errorf("path must start with /")
	}
	var b strings.Builder
	b.WriteString(basePath)
	parts := strings.Split(tmpl[1:], "/")
	n := 0
	for i, part := range parts {
		b.WriteByte('/')
		if part == "" {
			if i == len(parts)-1 {
				b.WriteString("{$}") // exact match on trailing slash
				continue
			}
			return "", nil, 0, fmt.Errorf("empty path segment")
		}
		names, re, isParam, err := parseSegment(part)
		if err != nil {
			return "", nil, 0, err
		}
		if len(names) == 0 {
			if strings.ContainsAny(part, "{}") {
				return "", nil, 0, fmt.Errorf("unbalanced braces in %q", part)
			}
			b.WriteString(part)
			segs = append(segs, segment{})
			literal += len(part)
			continue
		}
		w := "p" + strconv.Itoa(n)
		n++
		b.WriteString("{" + w + "}")
		s := segment{wildcard: w, names: names}
		if isParam {
			s.param = names[0]
		} else {
			s.re = re
			literal += len(regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(part, ""))
		}
		segs = append(segs, s)
	}
	return b.String(), segs, literal, nil
}

var paramRE = regexp.MustCompile(`\{([^{}]+)\}`)

// parseSegment finds {param} expressions in one segment. isParam is true
// when the whole segment is a single parameter.
func parseSegment(part string) (names []string, re *regexp.Regexp, isParam bool, err error) {
	locs := paramRE.FindAllStringSubmatchIndex(part, -1)
	if len(locs) == 0 {
		return nil, nil, false, nil
	}
	if len(locs) == 1 && locs[0][0] == 0 && locs[0][1] == len(part) {
		return []string{part[locs[0][2]:locs[0][3]]}, nil, true, nil
	}
	var expr strings.Builder
	expr.WriteByte('^')
	last := 0
	for _, l := range locs {
		lit := part[last:l[0]]
		if strings.ContainsAny(lit, "{}") {
			return nil, nil, false, fmt.Errorf("unbalanced braces in %q", part)
		}
		expr.WriteString(regexp.QuoteMeta(lit))
		expr.WriteString("(.+?)")
		names = append(names, part[l[2]:l[3]])
		last = l[1]
	}
	if strings.ContainsAny(part[last:], "{}") {
		return nil, nil, false, fmt.Errorf("unbalanced braces in %q", part)
	}
	expr.WriteString(regexp.QuoteMeta(part[last:]))
	expr.WriteByte('$')
	re, err = regexp.Compile(expr.String())
	return names, re, false, err
}

// params binds r's path values to parameter names; ok is false when a mixed
// segment does not match its template.
func (rt *route) params(r *http.Request) (map[string]string, bool) {
	out := map[string]string{}
	for _, s := range rt.segs {
		if s.wildcard == "" {
			continue
		}
		v := r.PathValue(s.wildcard)
		if s.re == nil {
			out[s.param] = v
			continue
		}
		m := s.re.FindStringSubmatch(v)
		if m == nil {
			return nil, false
		}
		for i, name := range s.names {
			out[name] = m[i+1]
		}
	}
	return out, true
}

func allowHeader(ops map[string]*pkg.Operation) string {
	methods := make([]string, 0, len(ops)+1)
	for m := range ops {
		methods = append(methods, m)
	}
	if _, ok := ops[http.MethodGet]; ok {
		if _, ok := ops[http.MethodHead]; !ok {
			methods = append(methods, http.MethodHead)
		}
	}
	slices.Sort(methods)
	return strings.Join(methods, ", ")
}
