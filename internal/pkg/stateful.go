package pkg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mockmint/mockmint/internal/config"
	"github.com/mockmint/mockmint/internal/state"
	"github.com/mockmint/mockmint/internal/template"
)

// State actions.
const (
	StateCreate = "create"
	StateRead   = "read"
	StateUpdate = "update"
	StateDelete = "delete"
	StateList   = "list"
)

// Update modes: what an update stores.
const (
	UpdateMerge    = "merge"    // shallow merge of the request body into the stored object
	UpdateRequest  = "request"  // the request body
	UpdateResponse = "response" // the rendered example body
)

// StateConfig makes an operation stateful (mockmint.yaml operations.<op>.state).
type StateConfig struct {
	Action     string `yaml:"action"`
	Collection string `yaml:"collection"`
	// Key is a template: request data for read/update/delete, plus
	// .Response (the decoded response body) for create.
	Key string `yaml:"key"`
	// Value is what update stores: merge (default), request or response.
	Value string `yaml:"value"`
	// Where filters list results: field → template. An entry is kept when
	// each rendered value is empty or equals the entry's field, e.g.
	// {status: "{{.Request.Query.status}}"}.
	Where map[string]string `yaml:"where"`
}

// StateOp is a compiled StateConfig.
type StateOp struct {
	Action     string
	Collection string
	Key        *template.Template // nil for list
	Value      string
	Where      []WhereClause // list only, sorted by field
}

// WhereClause is one list filter.
type WhereClause struct {
	Field string
	Value *template.Template
}

func compileState(opID string, c *StateConfig) (*StateOp, error) {
	if c == nil {
		return nil, nil
	}
	s := &StateOp{Action: c.Action, Collection: c.Collection, Value: c.Value}
	switch c.Action {
	case StateCreate, StateRead, StateUpdate, StateDelete:
		if c.Key == "" {
			return nil, fmt.Errorf("state: %s needs a key template", c.Action)
		}
		t, err := template.Parse(opID+" state key", c.Key)
		if err != nil {
			return nil, fmt.Errorf("state: key: %w", err)
		}
		s.Key = t
	case StateList:
		if c.Key != "" {
			return nil, errors.New("state: list takes no key")
		}
		fields := make([]string, 0, len(c.Where))
		for f := range c.Where {
			fields = append(fields, f)
		}
		slices.Sort(fields)
		for _, f := range fields {
			t, err := template.Parse(opID+" state where "+f, c.Where[f])
			if err != nil {
				return nil, fmt.Errorf("state: where %s: %w", f, err)
			}
			s.Where = append(s.Where, WhereClause{Field: f, Value: t})
		}
	default:
		return nil, fmt.Errorf("state: action %q: want create, read, update, delete or list", c.Action)
	}
	if c.Collection == "" {
		return nil, errors.New("state: collection is required")
	}
	if c.Action != StateList && len(c.Where) > 0 {
		return nil, errors.New("state: where applies to list only")
	}
	switch {
	case c.Action != StateUpdate && c.Value != "":
		return nil, errors.New("state: value applies to update only")
	case c.Action == StateUpdate:
		s.Value = cmpOr(c.Value, UpdateMerge)
		if !slices.Contains([]string{UpdateMerge, UpdateRequest, UpdateResponse}, s.Value) {
			return nil, fmt.Errorf("state: value %q: want merge, request or response", c.Value)
		}
	}
	return s, nil
}

// SeedState stores the package's initialState in collections that were
// never seeded and are empty in s (see state.Store.Seed), so a reload keeps
// state that requests have changed.
func (p *Package) SeedState(ctx context.Context, s state.Store) error {
	if len(p.InitialState) == 0 {
		return nil
	}
	if err := s.Seed(ctx, p.Name, p.InitialState); err != nil {
		return fmt.Errorf("initialState: %w", err)
	}
	return nil
}

// Proxy triggers.
const (
	ProxyUnmatchedRoute = "unmatchedRoute" // no operation matches the path under the base path
	ProxyNoExample      = "noExample"      // the dispatcher found no example
)

// ProxyConfig forwards some requests to a live upstream (mockmint.yaml proxy).
type ProxyConfig struct {
	URL     string          `yaml:"url"`
	On      []string        `yaml:"on"`      // default: both triggers
	Timeout config.Duration `yaml:"timeout"` // default 30s
	// Record keeps proxied exchanges of known operations as examples
	// (default true).
	Record *bool `yaml:"record"`
}

// Proxy is a compiled ProxyConfig.
type Proxy struct {
	URL              *url.URL
	OnUnmatchedRoute bool
	OnNoExample      bool
	Timeout          time.Duration
	Record           bool
}

func compileProxy(c *ProxyConfig) (*Proxy, error) {
	if c == nil {
		return nil, nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("proxy.url %q must be an absolute http(s) URL", c.URL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("proxy.url %q must not have a query or fragment", c.URL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	p := &Proxy{URL: u, Timeout: 30 * time.Second, Record: c.Record == nil || *c.Record}
	if c.Timeout > 0 {
		p.Timeout = c.Timeout.D()
	}
	on := c.On
	if len(on) == 0 {
		on = []string{ProxyUnmatchedRoute, ProxyNoExample}
	}
	for _, t := range on {
		switch t {
		case ProxyUnmatchedRoute:
			p.OnUnmatchedRoute = true
		case ProxyNoExample:
			p.OnNoExample = true
		default:
			return nil, fmt.Errorf("proxy.on %q: want %s or %s", t, ProxyUnmatchedRoute, ProxyNoExample)
		}
	}
	return p, nil
}
