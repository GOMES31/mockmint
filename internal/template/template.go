// Package template renders response bodies and headers with text/template.
//
// Request-scoped helpers (random values, UUIDs, the clock, fake data) are
// methods on the Data passed to Execute, so one parsed template is shared by
// all requests and each render draws only from that request's RNG. With a
// package seed, the RNG is derived from the request content (see NewRand),
// which makes rendering deterministic for identical requests.
package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/mockmint/mockmint/internal/jsonpath"
)

// maxOutput bounds a single render.
const maxOutput = 4 << 20

// Template is a parsed, reusable template.
type Template struct {
	t *template.Template
}

// Parse parses text. Parse errors carry the template name and line.
func Parse(name, text string) (*Template, error) {
	t, err := template.New(name).Option("missingkey=zero").Funcs(funcs).Parse(text)
	if err != nil {
		return nil, err
	}
	return &Template{t: t}, nil
}

// IsTemplate reports whether text contains template actions.
func IsTemplate(text string) bool { return strings.Contains(text, "{{") }

// Execute renders the template with d.
func (t *Template) Execute(d *Data) ([]byte, error) {
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: maxOutput}
	if err := t.t.Execute(lw, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, fmt.Errorf("template output exceeds %d bytes", maxOutput)
	}
	l.n -= len(p)
	return l.w.Write(p)
}

var funcs = template.FuncMap{
	"json":     toJSON,
	"jsonPath": jsonPathFn,
	"upper":    strings.ToUpper,
	"lower":    strings.ToLower,
	"trim":     strings.TrimSpace,
	"default":  defaultFn,
	"add":      func(a, b int) int { return a + b },
	"sub":      func(a, b int) int { return a - b },
	"mul":      func(a, b int) int { return a * b },
}

// toJSON renders v as compact JSON; strings become quoted JSON strings, which
// makes request echo safe inside JSON bodies.
func toJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func jsonPathFn(v any, expr string) (any, error) {
	p, err := jsonpath.Compile(expr)
	if err != nil {
		return nil, err
	}
	out, _ := p.Get(v)
	return out, nil
}

// defaultFn returns def when v is empty. Usage: {{ .Request.Query.limit | default "10" }}.
func defaultFn(def, v any) any {
	switch t := v.(type) {
	case nil:
		return def
	case string:
		if t == "" {
			return def
		}
	}
	return v
}
