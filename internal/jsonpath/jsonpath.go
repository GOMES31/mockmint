// Package jsonpath evaluates a small, deterministic JSONPath subset against
// values decoded by encoding/json: a root `$` followed by `.name`,
// `['name']`, `["name"]` and `[index]` (negative indexes count from the end).
// Wildcards, filters and recursive descent are deliberately unsupported: a
// path selects at most one value.
package jsonpath

import (
	"fmt"
	"strconv"
	"strings"
)

type step struct {
	key   string
	index int
	isIdx bool
}

// Path is a compiled JSONPath.
type Path struct {
	src   string
	steps []step
}

// String returns the source expression.
func (p *Path) String() string { return p.src }

// Compile parses expr.
func Compile(expr string) (*Path, error) {
	s := strings.TrimSpace(expr)
	if !strings.HasPrefix(s, "$") {
		return nil, fmt.Errorf("jsonpath %q: must start with $", expr)
	}
	p := &Path{src: expr}
	i := 1
	for i < len(s) {
		switch s[i] {
		case '.':
			j := i + 1
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			key := s[i+1 : j]
			if key == "" || key == "*" || key == "." {
				return nil, fmt.Errorf("jsonpath %q: invalid member at offset %d", expr, i)
			}
			p.steps = append(p.steps, step{key: key})
			i = j
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("jsonpath %q: unclosed [ at offset %d", expr, i)
			}
			inner := strings.TrimSpace(s[i+1 : i+end])
			if n := len(inner); n >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[n-1] == inner[0] {
				p.steps = append(p.steps, step{key: inner[1 : n-1]})
			} else {
				idx, err := strconv.Atoi(inner)
				if err != nil {
					return nil, fmt.Errorf("jsonpath %q: unsupported selector [%s]", expr, inner)
				}
				p.steps = append(p.steps, step{index: idx, isIdx: true})
			}
			i += end + 1
		default:
			return nil, fmt.Errorf("jsonpath %q: unexpected %q at offset %d", expr, s[i], i)
		}
	}
	return p, nil
}

// MustCompile is Compile that panics on error. For tests and constants.
func MustCompile(expr string) *Path {
	p, err := Compile(expr)
	if err != nil {
		panic(err)
	}
	return p
}

// Get returns the selected value and whether it exists.
func (p *Path) Get(v any) (any, bool) {
	cur := v
	for _, st := range p.steps {
		if st.isIdx {
			arr, ok := cur.([]any)
			if !ok {
				return nil, false
			}
			i := st.index
			if i < 0 {
				i += len(arr)
			}
			if i < 0 || i >= len(arr) {
				return nil, false
			}
			cur = arr[i]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[st.key]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// Stringify renders a selected JSON value for comparison against rule
// values: strings as-is, numbers without exponent where exact, booleans and
// null as their JSON literals. Objects and arrays return ok=false.
func Stringify(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}
