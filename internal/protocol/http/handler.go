package http

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mockmint/mockmint/internal/behavior"
	"github.com/mockmint/mockmint/internal/dispatch"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/problem"
	"github.com/mockmint/mockmint/internal/template"
)

// Response headers mockmint adds for observability.
const (
	HeaderExample    = "X-Mockmint-Example"
	HeaderValidation = "X-Mockmint-Validation"
)

type pathHandler struct {
	rt     *Router
	routes []*route
}

func (h *pathHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var (
		rt     *route
		params map[string]string
	)
	for _, cand := range h.routes {
		if p, ok := cand.params(r); ok {
			rt, params = cand, p
			break
		}
	}
	if rt == nil {
		h.rt.unmatched(w, r, h.routes[0].pkg)
		return
	}
	op := rt.ops[r.Method]
	if op == nil && r.Method == http.MethodHead {
		op = rt.ops[http.MethodGet] // net/http discards the body for HEAD
	}
	if op == nil {
		if p := rt.pkg; p.Proxy != nil && p.Proxy.OnUnmatchedRoute {
			annotate(w, p.Name, "", "")
			h.rt.forward(w, r, p, nil, nil, nil)
			return
		}
		w.Header().Set("Allow", rt.allow)
		problem.Write(w, r, problem.New(problem.TypeMethodNotAllowed, http.StatusMethodNotAllowed,
			r.Method+" is not allowed on "+rt.template).With("allow", strings.Split(rt.allow, ", ")))
		return
	}
	h.rt.serve(w, r, rt.pkg, op, params)
}

// unmatched answers a request under p's base path that matches no
// operation: proxied when p proxies unmatched routes, else 404.
func (rt *Router) unmatched(w http.ResponseWriter, r *http.Request, p *pkg.Package) {
	if p != nil && p.Proxy != nil && p.Proxy.OnUnmatchedRoute {
		annotate(w, p.Name, "", "")
		rt.forward(w, r, p, nil, nil, nil)
		return
	}
	rt.notFound(w, r)
}

func (rt *Router) serve(w http.ResponseWriter, r *http.Request, p *pkg.Package, op *pkg.Operation, params map[string]string) {
	log := rt.log.With("package", p.Name, "operation", op.ID)
	annotate(w, p.Name, op.ID, "")

	if ok, wait := op.Behavior.Allow(); !ok {
		secs := max(1, int((wait+999_999_999)/1_000_000_000))
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		problem.Write(w, r, problem.New(problem.TypeRateLimited, http.StatusTooManyRequests, "rate limit exceeded for "+op.ID))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rt.maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			problem.Write(w, r, problem.New(problem.TypeBodyTooLarge, http.StatusRequestEntityTooLarge,
				"request body exceeds "+strconv.FormatInt(tooBig.Limit, 10)+" bytes"))
			return
		}
		log.Debug("read request body", "error", err)
		return // client went away
	}

	if op.Validation != pkg.ValidationOff && !rt.validate(w, r, op, params, body, log) {
		return
	}

	rng := template.NewRand(p.Seed, []byte(op.ID), []byte(r.Method), []byte(r.URL.EscapedPath()), []byte(canonicalQuery(r.URL)), body)
	dreq := &dispatch.Request{Method: r.Method, Path: r.URL.Path, PathParams: params, Query: r.URL.Query(), Header: r.Header, Body: body}
	var data *template.Data
	getData := func() *template.Data {
		if data == nil {
			data = template.NewData(templateRequest(r, dreq, params), op.ID, "", rng, p.Now())
		}
		return data
	}

	// Stateful operations look their entry up before dispatch, so a missing
	// key becomes the fallback (or 404) regardless of example matching.
	var sc *stateCtx
	var resp *pkg.Response
	missing := false
	if op.State != nil {
		sc = &stateCtx{op: op.State, ns: p.Name, store: rt.state}
		switch err := sc.resolve(r.Context(), getData()); {
		case errors.Is(err, errStateMissing):
			missing = true
		case err != nil:
			log.Error("state lookup failed", "error", err)
			problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError, err.Error()))
			return
		}
		getData().State = template.NewState(sc.value, sc.found, stateReader{ctx: r.Context(), store: rt.state, ns: p.Name})
	}

	if missing {
		resp = op.Fallback
	} else {
		name, ok, err := op.Dispatcher.Dispatch(dreq)
		if err != nil {
			log.Error("dispatch failed", "error", err)
			problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError, err.Error()))
			return
		}
		resp = op.Responses[name]
		if (!ok || resp == nil) && p.Proxy != nil && p.Proxy.OnNoExample {
			rt.forward(w, r, p, op, params, body)
			return
		}
		if (!ok || resp == nil) && sc != nil {
			// The state entry exists (or is being created/listed): the body
			// comes from state, so any example shape will do. The fallback is
			// for missing entries only.
			resp = op.Responses[op.DefaultExample]
		}
		if resp == nil {
			resp = op.Fallback
		}
	}

	fault, faulted := op.Behavior.Fault(rng)
	if err := behavior.Sleep(r.Context(), op.Behavior.Delay(rng)); err != nil {
		return // client went away during injected latency
	}
	if faulted {
		if fault.Action == behavior.ActionDrop {
			drop(w)
			return
		}
		problem.Write(w, r, problem.New(problem.TypeInjectedFault, fault.Status, "fault injected by mockmint"))
		return
	}

	switch {
	case resp == nil && missing:
		problem.Write(w, r, problem.New(problem.TypeNotFound, http.StatusNotFound,
			"no "+op.State.Collection+" entry with key "+strconv.Quote(sc.key)))
		return
	case resp == nil:
		problem.Write(w, r, problem.New(problem.TypeNoMatchingExample, http.StatusNotFound,
			"no example of "+op.ID+" matches this request").With("examples", op.ExampleNames()))
		return
	}
	annotate(w, p.Name, op.ID, resp.Example)

	out, err := rt.build(resp, getData)
	if err != nil {
		log.Error("render example", "example", resp.Example, "error", err)
		problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError, err.Error()))
		return
	}
	// State changes apply to successful answers only; an error example
	// (validation 422, a fallback 404) leaves state untouched.
	if sc != nil && !missing && resp.Status >= 200 && resp.Status < 300 {
		replaced, err := sc.apply(r.Context(), getData(), out.body)
		if errors.Is(err, errStateMissing) { // deleted by a concurrent request
			problem.Write(w, r, problem.New(problem.TypeNotFound, http.StatusNotFound,
				"no "+op.State.Collection+" entry with key "+strconv.Quote(sc.key)))
			return
		}
		if err != nil {
			log.Error("state update failed", "error", err)
			problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError, err.Error()))
			return
		}
		if replaced != nil {
			out.body = replaced
			out.mediaType = "application/json"
		}
	}
	out.write(w, r)
	log.Debug("served", "method", r.Method, "path", r.URL.Path, "status", resp.Status, "example", resp.Example)
}

// validate applies request validation. It returns false when the request
// was rejected (strict mode) and a response has been written.
func (rt *Router) validate(w http.ResponseWriter, r *http.Request, op *pkg.Operation, params map[string]string, body []byte, log *slog.Logger) bool {
	strict := op.Validation == pkg.ValidationStrict
	if len(body) > 0 || r.Header.Get("Content-Type") != "" {
		if ct := r.Header.Get("Content-Type"); !op.AcceptsContentType(ct) {
			if strict {
				problem.Write(w, r, problem.New(problem.TypeUnsupportedMedia, http.StatusUnsupportedMediaType,
					"content type "+strconv.Quote(ct)+" is not accepted by "+op.ID).With("accepted", op.RequestBodyMediaTypes()))
				return false
			}
			log.Warn("unsupported content type", "contentType", ct)
			w.Header().Set(HeaderValidation, "failed")
			return true
		}
	}
	vreq := r.Clone(r.Context())
	vreq.Body = io.NopCloser(bytes.NewReader(body))
	vreq.ContentLength = int64(len(body))
	violations := op.ValidateRequest(r.Context(), vreq, params)
	if len(violations) == 0 {
		return true
	}
	if strict {
		problem.Write(w, r, problem.New(problem.TypeValidation, http.StatusBadRequest,
			"request does not match the OpenAPI definition of "+op.ID).With("errors", violations))
		return false
	}
	log.Warn("request validation failed", "violations", violations)
	w.Header().Set(HeaderValidation, "failed")
	return true
}

// canonicalQuery encodes the query with sorted keys so parameter order does
// not change the deterministic RNG seed.
func canonicalQuery(u *url.URL) string {
	return u.Query().Encode()
}

// drop closes the connection without writing a response. For HTTP/2 or
// writers that cannot be hijacked it aborts the handler instead, which
// resets the stream.
func drop(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}
