package http

import (
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"

	"github.com/mockmint/mockmint/internal/dispatch"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/problem"
	"github.com/mockmint/mockmint/internal/template"
)

// render writes resp, executing body and header templates with the
// request's RNG and the package clock.
func (rt *Router) render(w http.ResponseWriter, r *http.Request, p *pkg.Package, op *pkg.Operation, resp *pkg.Response,
	dreq *dispatch.Request, params map[string]string, rng *rand.Rand, log *slog.Logger) {
	body := resp.Body
	h := w.Header()
	if resp.BodyTemplate != nil || resp.HeaderTemplates != nil {
		data := template.NewData(templateRequest(r, dreq, params), op.ID, resp.Example, rng, p.Now())
		if resp.BodyTemplate != nil {
			out, err := resp.BodyTemplate.Execute(data)
			if err != nil {
				log.Error("render body template", "example", resp.Example, "error", err)
				problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError,
					"rendering example "+strconv.Quote(resp.Example)+" failed: "+err.Error()))
				return
			}
			body = out
		}
		for _, ht := range resp.HeaderTemplates { // sorted: fixed RNG consumption order
			out, err := ht.Template.Execute(data)
			if err != nil {
				log.Error("render header template", "example", resp.Example, "header", ht.Name, "error", err)
				problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError,
					"rendering header "+ht.Name+" of example "+strconv.Quote(resp.Example)+" failed: "+err.Error()))
				return
			}
			h.Set(ht.Name, string(out))
		}
	}
	for k, v := range resp.Headers {
		h.Set(k, v)
	}
	if resp.MediaType != "" && len(body) > 0 {
		h.Set("Content-Type", resp.MediaType)
	}
	h.Set(HeaderExample, resp.Example)
	if bodyAllowed(resp.Status) {
		h.Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(resp.Status)
	if bodyAllowed(resp.Status) && r.Method != http.MethodHead {
		_, _ = w.Write(body) //nolint:gosec // G705: echoing author-defined, templated content is the product; Content-Type is always the example's
	}
	log.Debug("served", "method", r.Method, "path", r.URL.Path, "status", resp.Status, "example", resp.Example)
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
