package dispatch

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"sync/atomic"

	"github.com/mockmint/mockmint/internal/jsonpath"
)

// Strategy names.
const (
	Auto        = "auto"
	Static      = "static"
	PathParams  = "path_params"
	QueryParams = "query_params"
	Header      = "header"
	BodyJSON    = "body_jsonpath"
	Sequence    = "sequence"
	Script      = "script"
)

// Config is an operation's dispatcher definition in mockmint.yaml.
type Config struct {
	Type string `yaml:"type"`
	// Example is the example returned by static (default: the operation's
	// default example).
	Example string `yaml:"example"`
	// Rules are evaluated in order by path_params, query_params, header and
	// body_jsonpath; the first rule whose conditions all hold wins.
	Rules []Rule `yaml:"rules"`
	// Default is returned when no rule matches (default: the operation's
	// default example; "-" disables it so the fallback applies).
	Default string `yaml:"default"`
	// Examples is the sequence order.
	Examples []string `yaml:"examples"`
	// Exhausted is what sequence does after the last example: last (repeat
	// it, the default), cycle (start over) or fallback.
	Exhausted string `yaml:"exhausted"`
	// Script is an expr-lang expression returning an example name.
	Script string `yaml:"script"`
}

// Rule maps conditions to an example. When keys are parameter names, header
// names or JSONPaths depending on the strategy.
type Rule struct {
	When    map[string]Matcher `yaml:"when"`
	Example string             `yaml:"example"`
}

// Candidate is an example's request half, used by the auto strategy.
type Candidate struct {
	Name       string
	PathParams map[string]string
	Query      map[string]string
	Headers    map[string]string
	Body       any // decoded JSON; matched as a subset of the request body
}

func (c *Candidate) constraints() int {
	n := len(c.PathParams) + len(c.Query) + len(c.Headers)
	if c.Body != nil {
		n++
	}
	return n
}

// Inputs is what Build needs to know about the operation.
type Inputs struct {
	Examples       []string    // all example names
	DefaultExample string      // "" if none
	Candidates     []Candidate // request halves for auto
}

// Build compiles cfg into a Dispatcher, checking that every referenced
// example exists.
func Build(cfg Config, in Inputs) (Dispatcher, error) {
	exists := func(name string) error {
		if !slices.Contains(in.Examples, name) {
			return fmt.Errorf("unknown example %q", name)
		}
		return nil
	}
	def := in.DefaultExample
	switch cfg.Default {
	case "":
	case "-":
		def = ""
	default:
		if err := exists(cfg.Default); err != nil {
			return nil, fmt.Errorf("default: %w", err)
		}
		def = cfg.Default
	}

	switch cfg.Type {
	case "", Auto:
		// The implicit default only catches unmatched requests when it has
		// no request half of its own: answering GET /pets/77 with the
		// example recorded for /pets/2 would be wrong. An explicit default
		// is always honored.
		if cfg.Default == "" && constrained(in.Candidates, def) {
			def = ""
		}
		return newAuto(in.Candidates, def), nil
	case Static:
		name := cfg.Example
		if name == "" {
			name = def
		}
		if name == "" {
			return nil, errors.New("static: no example and no default example")
		}
		if err := exists(name); err != nil {
			return nil, fmt.Errorf("static: %w", err)
		}
		return static(name), nil
	case PathParams, QueryParams, Header, BodyJSON:
		return newRules(cfg.Type, cfg.Rules, def, exists)
	case Sequence:
		if len(cfg.Examples) == 0 {
			return nil, errors.New("sequence: examples must not be empty")
		}
		for _, n := range cfg.Examples {
			if err := exists(n); err != nil {
				return nil, fmt.Errorf("sequence: %w", err)
			}
		}
		switch cfg.Exhausted {
		case "", "last", "cycle", "fallback":
		default:
			return nil, fmt.Errorf("sequence: exhausted %q: want last, cycle or fallback", cfg.Exhausted)
		}
		return &sequence{names: cfg.Examples, mode: cfg.Exhausted}, nil
	case Script:
		return newScript(cfg.Script, def, in.Examples)
	default:
		return nil, fmt.Errorf("unknown dispatcher type %q", cfg.Type)
	}
}

type static string

func (s static) Dispatch(*Request) (string, bool, error) { return string(s), true, nil }

// --- rules -----------------------------------------------------------------

type condition struct {
	key  string
	path *jsonpath.Path
	m    Matcher
}

type rule struct {
	conds   []condition
	example string
}

type rules struct {
	kind  string
	rules []rule
	def   string
}

func newRules(kind string, in []Rule, def string, exists func(string) error) (*rules, error) {
	d := &rules{kind: kind, def: def}
	for i, r := range in {
		if err := exists(r.Example); err != nil {
			return nil, fmt.Errorf("%s rule %d: %w", kind, i+1, err)
		}
		if len(r.When) == 0 {
			return nil, fmt.Errorf("%s rule %d: when must not be empty", kind, i+1)
		}
		keys := make([]string, 0, len(r.When))
		for k := range r.When {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic evaluation order
		cr := rule{example: r.Example}
		for _, k := range keys {
			m := r.When[k]
			if err := m.compile(); err != nil {
				return nil, fmt.Errorf("%s rule %d, %q: %w", kind, i+1, k, err)
			}
			c := condition{key: k, m: m}
			switch kind {
			case Header:
				c.key = http.CanonicalHeaderKey(k)
			case BodyJSON:
				p, err := jsonpath.Compile(k)
				if err != nil {
					return nil, fmt.Errorf("%s rule %d: %w", kind, i+1, err)
				}
				c.path = p
			}
			cr.conds = append(cr.conds, c)
		}
		d.rules = append(d.rules, cr)
	}
	return d, nil
}

func (d *rules) Dispatch(r *Request) (string, bool, error) {
	for i := range d.rules {
		if d.matches(&d.rules[i], r) {
			return d.rules[i].example, true, nil
		}
	}
	return d.def, d.def != "", nil
}

func (d *rules) matches(ru *rule, r *Request) bool {
	for i := range ru.conds {
		c := &ru.conds[i]
		var ok bool
		switch d.kind {
		case PathParams:
			v, present := r.PathParams[c.key]
			ok = c.m.match(v, present)
		case QueryParams:
			ok = c.m.matchAny(r.Query[c.key])
		case Header:
			ok = c.m.matchAny(r.Header[c.key])
		case BodyJSON:
			var s string
			present := false
			if doc, isJSON := r.JSON(); isJSON {
				if v, found := c.path.Get(doc); found {
					s, present = jsonpath.Stringify(v)
				}
			}
			ok = c.m.match(s, present)
		}
		if !ok {
			return false
		}
	}
	return true
}

// --- sequence --------------------------------------------------------------

type sequence struct {
	names []string
	mode  string
	n     atomic.Uint64
}

func (s *sequence) Dispatch(*Request) (string, bool, error) {
	i := s.n.Add(1) - 1
	l := uint64(len(s.names))
	if i < l {
		return s.names[i], true, nil
	}
	switch s.mode {
	case "cycle":
		return s.names[i%l], true, nil
	case "fallback":
		return "", false, nil
	default:
		return s.names[l-1], true, nil
	}
}

// --- auto ------------------------------------------------------------------

func constrained(cands []Candidate, name string) bool {
	for i := range cands {
		if cands[i].Name == name {
			return cands[i].constraints() > 0
		}
	}
	return false
}

type auto struct {
	cands []Candidate // only those with constraints, sorted by name
	def   string
}

func newAuto(cands []Candidate, def string) *auto {
	a := &auto{def: def}
	for _, c := range cands {
		if c.constraints() > 0 {
			a.cands = append(a.cands, c)
		}
	}
	sort.Slice(a.cands, func(i, j int) bool { return a.cands[i].Name < a.cands[j].Name })
	return a
}

// Dispatch returns the candidate whose request half matches with the most
// constraints (ties broken by name), else the default.
func (a *auto) Dispatch(r *Request) (string, bool, error) {
	best, bestScore := "", 0
	for i := range a.cands {
		c := &a.cands[i]
		if score := c.constraints(); score > bestScore && a.matches(c, r) {
			best, bestScore = c.Name, score
		}
	}
	if best != "" {
		return best, true, nil
	}
	return a.def, a.def != "", nil
}

func (a *auto) matches(c *Candidate, r *Request) bool {
	for k, v := range c.PathParams {
		if r.PathParams[k] != v {
			return false
		}
	}
	for k, v := range c.Query {
		if !slices.Contains(r.Query[k], v) {
			return false
		}
	}
	for k, v := range c.Headers {
		if !slices.Contains(r.Header.Values(k), v) {
			return false
		}
	}
	if c.Body != nil {
		doc, ok := r.JSON()
		if !ok || !subset(c.Body, doc) {
			return false
		}
	}
	return true
}

// subset reports whether want is contained in got: objects recursively by
// key, everything else by equality.
func subset(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok || !subset(wv, gv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !subset(w[i], g[i]) {
				return false
			}
		}
		return true
	default:
		return want == got
	}
}
