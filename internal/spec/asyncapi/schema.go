package asyncapi

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// resourceBase is the URL scheme under which package files are registered
// with the JSON Schema compiler.
const resourceBase = "mockmint:///"

// Schema is a compiled message payload or headers schema.
type Schema struct {
	compiled *jsonschema.Schema
	node     *yaml.Node // for example synthesis
	at       loc
	docs     *docSet
}

// Violation is one schema validation failure.
type Violation struct {
	Path    string `json:"path"` // JSON pointer into the payload, "" for the root
	Message string `json:"message"`
}

func (v Violation) String() string {
	if v.Path == "" {
		return v.Message
	}
	return v.Path + ": " + v.Message
}

// Validate checks a JSON-compatible value (decoded with encoding/json or
// normalized) and returns nil when it is valid.
func (s *Schema) Validate(v any) []Violation {
	err := s.compiled.Validate(v)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []Violation{{Message: err.Error()}}
	}
	var out []Violation
	leaves(ve, &out)
	slices.SortStableFunc(out, func(a, b Violation) int { return cmp.Compare(a.Path, b.Path) })
	return out
}

// leaves collects the most specific errors: group errors such as
// "validation failed" or "allOf failed" only restate their causes.
func leaves(ve *jsonschema.ValidationError, out *[]Violation) {
	if len(ve.Causes) > 0 {
		for _, c := range ve.Causes {
			leaves(c, out)
		}
		return
	}
	ptr := ""
	if len(ve.InstanceLocation) > 0 {
		parts := make([]string, len(ve.InstanceLocation))
		for i, p := range ve.InstanceLocation {
			parts[i] = escapePtr(p)
		}
		ptr = "/" + strings.Join(parts, "/")
	}
	*out = append(*out, Violation{Path: ptr, Message: ve.ErrorKind.LocalizedString(printer)})
}

var printer = message.NewPrinter(language.English)

// schemaSet compiles schemas, registering each package file once.
type schemaSet struct {
	fsys     fs.FS
	compiler *jsonschema.Compiler
	added    map[string]bool
}

func newSchemaSet(fsys fs.FS) *schemaSet {
	s := &schemaSet{fsys: fsys, compiler: jsonschema.NewCompiler(), added: map[string]bool{}}
	// AsyncAPI's default schema format is a superset of draft-07.
	s.compiler.DefaultDraft(jsonschema.Draft7)
	s.compiler.UseLoader(packageLoader{s})
	return s
}

// packageLoader serves $refs to other package files; anything else fails.
type packageLoader struct{ s *schemaSet }

func (p packageLoader) Load(u string) (any, error) {
	name, ok := strings.CutPrefix(u, resourceBase)
	if !ok {
		return nil, fmt.Errorf("schema reference %q is outside the package", u)
	}
	name, err := url.PathUnescape(name)
	if err != nil || !fs.ValidPath(name) {
		return nil, fmt.Errorf("schema reference %q escapes the package", u)
	}
	b, err := fs.ReadFile(p.s.fsys, name)
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return normalize(v), nil
}

func (s *schemaSet) compile(docs *docSet, n *yaml.Node, at loc) (*Schema, error) {
	if !s.added[at.file] {
		root, err := docs.file(at.file)
		if err != nil {
			return nil, err
		}
		var v any
		if err := root.Decode(&v); err != nil {
			return nil, fmt.Errorf("%s: %w", at.file, err)
		}
		if err := s.compiler.AddResource(resourceURL(at.file), normalize(v)); err != nil {
			return nil, fmt.Errorf("%s: %w", at.file, err)
		}
		s.added[at.file] = true
	}
	c, err := s.compiler.Compile(resourceURL(at.file) + "#" + (&url.URL{Fragment: at.ptr}).EscapedFragment())
	if err != nil {
		return nil, fmt.Errorf("%s: invalid schema: %w", at, err)
	}
	return &Schema{compiled: c, node: n, at: at, docs: docs}, nil
}

func resourceURL(file string) string {
	return resourceBase + (&url.URL{Path: file}).EscapedPath()
}

// normalize converts YAML-decoded values to the JSON value model
// (map[string]any, []any, float64, string, bool, nil).
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalize(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case float32:
		return float64(t)
	default:
		return v
	}
}

func normalizeMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return normalize(m).(map[string]any)
}
