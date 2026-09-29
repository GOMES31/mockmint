package dispatch

import (
	"errors"
	"fmt"
	"slices"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// maxScriptNodes bounds script size; the expr VM also enforces its default
// memory budget at run time. Scripts have no I/O: expr exposes only the
// environment below and pure builtins, and `now` is disabled to keep
// dispatch deterministic.
const maxScriptNodes = 2000

// ScriptEnv is the environment visible to dispatcher scripts, e.g.
//
//	body.kind == "cat" ? "cat" : headers["X-Tier"] == "gold" ? "vip" : ""
type ScriptEnv struct {
	Method  string              `expr:"method"`
	Path    string              `expr:"path"`
	Params  map[string]string   `expr:"params"`
	Query   map[string]string   `expr:"query"`   // first value
	Queries map[string][]string `expr:"queries"` // all values
	Headers map[string]string   `expr:"headers"` // first value, canonical names
	Body    any                 `expr:"body"`    // decoded JSON or nil
	RawBody string              `expr:"rawBody"`
}

type script struct {
	prog  *vm.Program
	def   string
	names []string
}

func newScript(src, def string, names []string) (*script, error) {
	if src == "" {
		return nil, errors.New("script: script must not be empty")
	}
	prog, err := expr.Compile(src,
		expr.Env(ScriptEnv{}),
		expr.DisableBuiltin("now"),
		expr.MaxNodes(maxScriptNodes),
	)
	if err != nil {
		return nil, fmt.Errorf("script: %w", err)
	}
	return &script{prog: prog, def: def, names: names}, nil
}

// Dispatch runs the script. A string result names the example; "" or nil
// means no match (default, then fallback). Any other result, or a name that
// is not an example, is an error.
func (s *script) Dispatch(r *Request) (string, bool, error) {
	out, err := expr.Run(s.prog, scriptEnv(r))
	if err != nil {
		return "", false, fmt.Errorf("script: %w", err)
	}
	var name string
	switch v := out.(type) {
	case nil:
	case string:
		name = v
	default:
		return "", false, fmt.Errorf("script: result must be a string example name, got %T", out)
	}
	if name == "" {
		return s.def, s.def != "", nil
	}
	if !slices.Contains(s.names, name) {
		return "", false, fmt.Errorf("script: returned unknown example %q", name)
	}
	return name, true, nil
}

func scriptEnv(r *Request) ScriptEnv {
	env := ScriptEnv{
		Method:  r.Method,
		Path:    r.Path,
		Params:  r.PathParams,
		Query:   make(map[string]string, len(r.Query)),
		Queries: r.Query,
		Headers: make(map[string]string, len(r.Header)),
		RawBody: string(r.Body),
	}
	for k, v := range r.Query {
		if len(v) > 0 {
			env.Query[k] = v[0]
		}
	}
	for k, v := range r.Header {
		if len(v) > 0 {
			env.Headers[k] = v[0]
		}
	}
	if doc, ok := r.JSON(); ok {
		env.Body = doc
	}
	return env
}
