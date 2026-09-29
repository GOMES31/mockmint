// Package openapi loads OpenAPI 3.0/3.1 documents with kin-openapi and turns
// them into mockable operations with named examples.
package openapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Spec is a loaded, validated OpenAPI document.
type Spec struct {
	Doc        *openapi3.T
	Title      string
	Version    string
	Operations []*Operation // sorted by path, then method
}

// Load reads the document at name from fsys. External $refs resolve only to
// files inside fsys; remote and absolute references are rejected.
func Load(ctx context.Context, fsys fs.FS, name string) (*Spec, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, u *url.URL) ([]byte, error) {
		if u.Scheme != "" || u.Host != "" {
			return nil, fmt.Errorf("remote $ref %q is not allowed", u.String())
		}
		p := strings.TrimPrefix(path.Clean(u.Path), "./")
		if !fs.ValidPath(p) {
			return nil, fmt.Errorf("$ref %q escapes the package", u.Path)
		}
		return fs.ReadFile(fsys, p)
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	doc, err := loader.LoadFromDataWithPath(data, &url.URL{Path: name})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	// Examples are checked separately (as warnings), so a bad example does
	// not make the whole spec unusable.
	if err := doc.Validate(ctx, openapi3.DisableExamplesValidation(), openapi3.EnableMultiError()); err != nil {
		return nil, fmt.Errorf("%s: invalid OpenAPI document: %w", name, err)
	}
	if doc.Paths == nil || doc.Paths.Len() == 0 {
		return nil, fmt.Errorf("%s: no paths", name)
	}
	s := &Spec{Doc: doc}
	if doc.Info != nil {
		s.Title, s.Version = doc.Info.Title, doc.Info.Version
	}
	for p, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			s.Operations = append(s.Operations, newOperation(doc, p, item, method, op))
		}
	}
	slices.SortFunc(s.Operations, func(a, b *Operation) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Method, b.Method))
	})
	return s, nil
}

// IsDocument reports whether data looks like an OpenAPI document (it has a
// top-level "openapi" key). Used to tell OpenAPI files from other YAML/JSON.
func IsDocument(data []byte) bool {
	for line := range strings.SplitSeq(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(line, "openapi:") || strings.HasPrefix(t, `"openapi"`) || strings.HasPrefix(t, `{"openapi"`) {
			return true
		}
	}
	return false
}

// Operation is one method on one path.
type Operation struct {
	ID          string // "GET /pets/{petId}", unique within a spec
	OperationID string
	Method      string
	Path        string

	Doc      *openapi3.T
	PathItem *openapi3.PathItem
	Op       *openapi3.Operation

	// Examples are keyed by name; see Finalize for the default.
	Examples       map[string]*Example
	DefaultExample string
	// Warnings are non-fatal problems found while building the operation.
	Warnings []string
}

func newOperation(doc *openapi3.T, p string, item *openapi3.PathItem, method string, op *openapi3.Operation) *Operation {
	o := &Operation{
		ID:          method + " " + p,
		OperationID: op.OperationID,
		Method:      method,
		Path:        p,
		Doc:         doc,
		PathItem:    item,
		Op:          op,
		Examples:    map[string]*Example{},
	}
	o.collectSpecExamples()
	return o
}

// Parameters returns the effective parameters: path-level merged with
// operation-level, the operation winning on (name, in).
func (o *Operation) Parameters() openapi3.Parameters {
	out := slices.Clone(o.Op.Parameters)
	for _, pr := range o.PathItem.Parameters {
		if pr.Value != nil && o.Op.Parameters.GetByInAndName(pr.Value.In, pr.Value.Name) == nil {
			out = append(out, pr)
		}
	}
	return out
}

// Responses returns the declared responses as (status, response) pairs,
// sorted by status. "default" maps to 500 and range keys ("2XX") to their
// base code; an explicit code wins over a range or default mapping to it.
func (o *Operation) Responses() []StatusResponse {
	if o.Op.Responses == nil {
		return nil
	}
	byStatus := map[int]StatusResponse{}
	for _, key := range o.Op.Responses.Keys() {
		ref := o.Op.Responses.Value(key)
		if ref == nil || ref.Value == nil {
			continue
		}
		status, exact := statusOf(key)
		if status == 0 {
			continue
		}
		if prev, ok := byStatus[status]; ok && prev.exact && !exact {
			continue
		}
		byStatus[status] = StatusResponse{Status: status, Key: key, Response: ref.Value, exact: exact}
	}
	out := make([]StatusResponse, 0, len(byStatus))
	for _, sr := range byStatus {
		out = append(out, sr)
	}
	slices.SortFunc(out, func(a, b StatusResponse) int { return cmp.Compare(a.Status, b.Status) })
	return out
}

// StatusResponse is a response with its resolved status code.
type StatusResponse struct {
	Status   int
	Key      string
	Response *openapi3.Response
	exact    bool
}

func statusOf(key string) (status int, exact bool) {
	switch {
	case key == "default":
		return 500, false
	case len(key) == 3 && strings.HasSuffix(strings.ToUpper(key), "XX") && key[0] >= '1' && key[0] <= '5':
		return int(key[0]-'0') * 100, false
	}
	var n int
	if _, err := fmt.Sscanf(key, "%3d", &n); err != nil || n < 100 || n > 599 || len(key) != 3 {
		return 0, false
	}
	return n, true
}

// RequestBodyMediaTypes returns the declared request body media types, or
// nil when the operation has no request body.
func (o *Operation) RequestBodyMediaTypes() []string {
	if o.Op.RequestBody == nil || o.Op.RequestBody.Value == nil {
		return nil
	}
	types := make([]string, 0, len(o.Op.RequestBody.Value.Content))
	for mt := range o.Op.RequestBody.Value.Content {
		types = append(types, mt)
	}
	slices.Sort(types)
	return types
}

// AcceptsContentType reports whether contentType (a Content-Type header
// value) matches a declared request body media type, including wildcards
// such as "application/*" and "*/*". An operation without a request body
// declaration accepts anything.
func (o *Operation) AcceptsContentType(contentType string) bool {
	declared := o.RequestBodyMediaTypes()
	if declared == nil {
		return true
	}
	mt := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	for _, d := range declared {
		if mediaTypeMatches(strings.ToLower(d), mt) {
			return true
		}
	}
	return false
}

func mediaTypeMatches(pattern, mt string) bool {
	if pattern == "*/*" || pattern == mt {
		return true
	}
	if pt, ok := strings.CutSuffix(pattern, "/*"); ok {
		typ, _, _ := strings.Cut(mt, "/")
		return typ == pt
	}
	return false
}

// ErrNoExamples is returned by Finalize when an operation has no examples
// and nothing could be synthesized.
var ErrNoExamples = errors.New("no examples and no declared responses")
