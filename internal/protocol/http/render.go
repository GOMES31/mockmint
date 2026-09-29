package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/mockmint/mockmint/internal/dispatch"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/template"
)

// built is a rendered response, before it is written.
type built struct {
	status    int
	mediaType string
	headers   map[string]string
	body      []byte
	example   string
}

// build renders resp's templates. data is only created when a template
// needs it.
func (rt *Router) build(resp *pkg.Response, data func() *template.Data) (*built, error) {
	out := &built{status: resp.Status, mediaType: resp.MediaType, body: resp.Body, example: resp.Example,
		headers: make(map[string]string, len(resp.Headers)+len(resp.HeaderTemplates))}
	for k, v := range resp.Headers {
		out.headers[k] = v
	}
	if resp.BodyTemplate == nil && resp.HeaderTemplates == nil {
		return out, nil
	}
	d := data()
	d.Example = resp.Example
	if resp.BodyTemplate != nil {
		b, err := resp.BodyTemplate.Execute(d)
		if err != nil {
			return nil, fmt.Errorf("rendering example %q failed: %w", resp.Example, err)
		}
		out.body = b
	}
	for _, ht := range resp.HeaderTemplates { // sorted: fixed RNG consumption order
		b, err := ht.Template.Execute(d)
		if err != nil {
			return nil, fmt.Errorf("rendering header %s of example %q failed: %w", ht.Name, resp.Example, err)
		}
		out.headers[ht.Name] = string(b)
	}
	return out, nil
}

func (b *built) write(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	for k, v := range b.headers {
		h.Set(k, v)
	}
	if b.mediaType != "" && len(b.body) > 0 {
		h.Set("Content-Type", b.mediaType)
	}
	h.Set(HeaderExample, b.example)
	if bodyAllowed(b.status) {
		h.Set("Content-Length", strconv.Itoa(len(b.body)))
	}
	w.WriteHeader(b.status)
	if bodyAllowed(b.status) && r.Method != http.MethodHead {
		_, _ = w.Write(b.body) //nolint:gosec // G705: echoing author-defined, templated content is the product; Content-Type is always the example's
	}
}

// bodyAllowed reports whether a status may carry a body (RFC 9110).
func bodyAllowed(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}

// templateRequest builds the .Request view for templates.
func templateRequest(r *http.Request, dreq *dispatch.Request, params map[string]string) template.Request {
	q := dreq.Query
	tr := template.Request{
		Method:  r.Method,
		Path:    r.URL.Path,
		Params:  params,
		Query:   make(map[string]string, len(q)),
		Queries: q,
		Headers: make(map[string]string, len(r.Header)),
		RawBody: string(dreq.Body),
	}
	for k, v := range q {
		if len(v) > 0 {
			tr.Query[k] = v[0]
		}
	}
	for k, v := range r.Header {
		if len(v) > 0 {
			tr.Headers[k] = v[0]
		}
	}
	if doc, ok := dreq.JSON(); ok {
		tr.Body = doc
	}
	return tr
}

func jsonOrNil(b []byte) any {
	var v any
	if len(b) == 0 || json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

func itoa(i int) string { return strconv.Itoa(i) }
