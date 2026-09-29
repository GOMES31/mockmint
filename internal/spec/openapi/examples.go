package openapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Example is a named request/response pair.
type Example struct {
	Name   string
	Source string // "spec", a file path, or "generated"

	// Request half, used by the auto dispatcher. All optional.
	PathParams map[string]string
	Query      map[string]string
	Headers    map[string]string
	Body       any // decoded JSON request body

	// Response half.
	Status          int
	MediaType       string            // "" for an empty body
	ResponseHeaders map[string]string // header values may be templates
	ResponseBody    []byte            // may be a template
}

func (e *Example) hasRequest() bool {
	return len(e.PathParams)+len(e.Query)+len(e.Headers) > 0 || e.Body != nil
}

// collectSpecExamples gathers named response examples per status and media
// type, then attaches request halves from parameter and request body
// examples with the same name (the Microcks convention).
func (o *Operation) collectSpecExamples() {
	for _, sr := range o.Responses() {
		for _, mt := range sortedMediaTypes(sr.Response.Content) {
			media := sr.Response.Content[mt]
			for _, name := range sortedKeys(media.Examples) {
				ex := media.Examples[name]
				if ex == nil || ex.Value == nil {
					continue
				}
				if ex.Value.ExternalValue != "" && ex.Value.Value == nil {
					o.warnf("example %q: externalValue is not supported; skipped", name)
					continue
				}
				o.addSpecResponse(name, sr, mt, ex.Value.Value)
			}
			if media.Example != nil && len(media.Examples) == 0 {
				// A lone `example` is named after its status so several
				// statuses do not collide.
				o.addSpecResponse(fmt.Sprintf("%d", sr.Status), sr, mt, media.Example)
			}
		}
	}

	for _, pr := range o.Parameters() {
		p := pr.Value
		if p == nil {
			continue
		}
		for name, ex := range p.Examples {
			e, ok := o.Examples[name]
			if !ok || ex == nil || ex.Value == nil || ex.Value.Value == nil {
				continue
			}
			v := scalarString(ex.Value.Value)
			switch p.In {
			case openapi3.ParameterInPath:
				e.PathParams = setKey(e.PathParams, p.Name, v)
			case openapi3.ParameterInQuery:
				e.Query = setKey(e.Query, p.Name, v)
			case openapi3.ParameterInHeader:
				e.Headers = setKey(e.Headers, http.CanonicalHeaderKey(p.Name), v)
			}
		}
	}
	if rb := o.Op.RequestBody; rb != nil && rb.Value != nil {
		for _, mt := range sortedMediaTypes(rb.Value.Content) {
			for name, ex := range rb.Value.Content[mt].Examples {
				if e, ok := o.Examples[name]; ok && e.Body == nil && ex != nil && ex.Value != nil {
					e.Body = normalizeJSON(ex.Value.Value)
				}
			}
		}
	}
}

func (o *Operation) addSpecResponse(name string, sr StatusResponse, mt string, value any) {
	if prev, ok := o.Examples[name]; ok {
		o.warnf("example %q is declared for %d %s and %d %s; using the first", name, prev.Status, prev.MediaType, sr.Status, mt)
		return
	}
	body, err := encodeBody(mt, value)
	if err != nil {
		o.warnf("example %q: %v; skipped", name, err)
		return
	}
	e := &Example{Name: name, Source: "spec", Status: sr.Status, MediaType: mt, ResponseBody: body}
	e.ResponseHeaders = exampleHeaders(sr.Response)
	o.Examples[name] = e
	o.checkBody(e, value)
}

// AddExample adds or replaces an example from outside the spec (a package
// example file). Replacing a spec example is reported as a warning.
func (o *Operation) AddExample(e *Example) error {
	if e.Name == "" {
		return fmt.Errorf("%s: example without a name", e.Source)
	}
	if e.Status == 0 {
		e.Status = o.defaultStatus()
	}
	if e.Status < 100 || e.Status > 599 {
		return fmt.Errorf("%s: example %q: invalid status %d", e.Source, e.Name, e.Status)
	}
	if prev, ok := o.Examples[e.Name]; ok {
		o.warnf("example %q from %s replaces the one from %s", e.Name, e.Source, prev.Source)
	}
	if e.MediaType == "" && len(e.ResponseBody) > 0 {
		e.MediaType = o.responseMediaType(e.Status)
	}
	if e.ResponseHeaders == nil {
		if sr, ok := o.response(e.Status); ok {
			e.ResponseHeaders = exampleHeaders(sr.Response)
		}
	}
	o.Examples[e.Name] = e
	if strings.Contains(e.MediaType, "json") && !strings.Contains(string(e.ResponseBody), "{{") {
		var v any
		if err := json.Unmarshal(e.ResponseBody, &v); err != nil {
			o.warnf("example %q: body is not valid JSON: %v", e.Name, err)
		} else {
			o.checkBody(e, v)
		}
	}
	return nil
}

// Finalize synthesizes an example for the lowest 2xx response when none of
// the examples covers it, then chooses the default example: one named
// "default", else the alphabetically first example of the lowest 2xx
// status, else the first example of the lowest status. rng drives schema
// synthesis.
func (o *Operation) Finalize(rng *rand.Rand) error {
	resps := o.Responses()
	success := -1
	for i, sr := range resps {
		if sr.Status >= 200 && sr.Status < 300 {
			success = i
			break
		}
	}
	if success >= 0 && !o.hasStatus(resps[success].Status) {
		sr := resps[success]
		e := &Example{Name: "generated", Source: "generated", Status: sr.Status, ResponseHeaders: exampleHeaders(sr.Response)}
		if mt := preferredMediaType(sr.Response.Content); mt != "" {
			e.MediaType = mt
			var schema *openapi3.Schema
			if ref := sr.Response.Content[mt].Schema; ref != nil {
				schema = ref.Value
			}
			v, err := Generate(schema, rng)
			if err != nil {
				return fmt.Errorf("status %d: %w", sr.Status, err)
			}
			body, err := encodeBody(mt, v)
			if err != nil {
				return fmt.Errorf("generate example: %w", err)
			}
			e.ResponseBody = body
		}
		if _, taken := o.Examples[e.Name]; !taken {
			o.Examples[e.Name] = e
		}
	}
	if len(o.Examples) == 0 {
		if len(resps) == 0 {
			return ErrNoExamples
		}
		// Only error responses are declared and none has an example.
		sr := resps[0]
		o.Examples["generated"] = &Example{Name: "generated", Source: "generated", Status: sr.Status}
	}

	if _, ok := o.Examples["default"]; ok {
		o.DefaultExample = "default"
		return nil
	}
	names := o.ExampleNames()
	best := names[0]
	for _, n := range names[1:] {
		if rank(o.Examples[n]) < rank(o.Examples[best]) {
			best = n
		}
	}
	o.DefaultExample = best
	return nil
}

// rank orders examples for default selection: 2xx before other statuses,
// then examples without a request half (catch-alls) before request-bound
// ones, then lower status. Ties keep alphabetical order.
func rank(e *Example) int {
	r := e.Status
	if e.hasRequest() {
		r += 1000
	}
	if e.Status < 200 || e.Status >= 300 {
		r += 10000
	}
	return r
}

// ExampleNames returns example names sorted alphabetically.
func (o *Operation) ExampleNames() []string {
	names := make([]string, 0, len(o.Examples))
	for n := range o.Examples {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// HasRequestExamples reports whether any example has a request half.
func (o *Operation) HasRequestExamples() bool {
	for _, e := range o.Examples {
		if e.hasRequest() {
			return true
		}
	}
	return false
}

func (o *Operation) hasStatus(status int) bool {
	for _, e := range o.Examples {
		if e.Status == status {
			return true
		}
	}
	return false
}

func (o *Operation) response(status int) (StatusResponse, bool) {
	for _, sr := range o.Responses() {
		if sr.Status == status {
			return sr, true
		}
	}
	return StatusResponse{}, false
}

func (o *Operation) defaultStatus() int {
	resps := o.Responses()
	for _, sr := range resps {
		if sr.Status >= 200 && sr.Status < 300 {
			return sr.Status
		}
	}
	if len(resps) > 0 {
		return resps[0].Status
	}
	return http.StatusOK
}

func (o *Operation) responseMediaType(status int) string {
	if sr, ok := o.response(status); ok {
		if mt := preferredMediaType(sr.Response.Content); mt != "" {
			return mt
		}
	}
	return "application/json"
}

// checkBody warns when an example body does not satisfy its response schema.
func (o *Operation) checkBody(e *Example, value any) {
	sr, ok := o.response(e.Status)
	if !ok {
		o.warnf("example %q: status %d is not declared", e.Name, e.Status)
		return
	}
	media := sr.Response.Content.Get(e.MediaType)
	if media == nil || media.Schema == nil || media.Schema.Value == nil || !strings.Contains(e.MediaType, "json") {
		return
	}
	if strings.Contains(string(e.ResponseBody), "{{") {
		return // templates are checked after rendering, not here
	}
	if err := media.Schema.Value.VisitJSON(normalizeJSON(value), openapi3.VisitAsResponse(), openapi3.MultiErrors()); err != nil {
		o.warnf("example %q does not match the %d response schema: %v", e.Name, e.Status, oneLine(err))
	}
}

func (o *Operation) warnf(format string, args ...any) {
	o.Warnings = append(o.Warnings, fmt.Sprintf(format, args...))
}

// exampleHeaders returns example values declared on response headers.
func exampleHeaders(r *openapi3.Response) map[string]string {
	var out map[string]string
	for name, h := range r.Headers {
		if h == nil || h.Value == nil {
			continue
		}
		var v any
		switch {
		case h.Value.Example != nil:
			v = h.Value.Example
		case h.Value.Schema != nil && h.Value.Schema.Value != nil && h.Value.Schema.Value.Example != nil:
			v = h.Value.Schema.Value.Example
		default:
			continue
		}
		out = setKey(out, http.CanonicalHeaderKey(name), scalarString(v))
	}
	return out
}

// encodeBody serializes an example value for a media type: JSON media types
// get JSON, strings are written as-is for everything else.
func encodeBody(mt string, v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok && !isJSONMediaType(mt) {
		return []byte(s), nil
	}
	b, err := json.Marshal(normalizeJSON(v))
	if err != nil {
		return nil, fmt.Errorf("encode example: %w", err)
	}
	return b, nil
}

func isJSONMediaType(mt string) bool {
	mt = strings.ToLower(mt)
	return mt == "application/json" || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "/json")
}

// preferredMediaType picks application/json, then any JSON type, then the
// alphabetically first declared type.
func preferredMediaType(c openapi3.Content) string {
	types := sortedMediaTypes(c)
	if len(types) == 0 {
		return ""
	}
	return types[0]
}

func sortedMediaTypes(c openapi3.Content) []string {
	types := make([]string, 0, len(c))
	for mt := range c {
		types = append(types, mt)
	}
	score := func(mt string) int {
		switch {
		case strings.EqualFold(mt, "application/json"):
			return 0
		case isJSONMediaType(mt):
			return 1
		default:
			return 2
		}
	}
	slices.SortFunc(types, func(a, b string) int { return cmp.Or(cmp.Compare(score(a), score(b)), cmp.Compare(a, b)) })
	return types
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func setKey(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

// scalarString renders a parameter example the way it appears on the wire.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64, int, int64, bool:
		b, _ := json.Marshal(t)
		return string(b)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// normalizeJSON round-trips v through encoding/json so YAML-decoded values
// (ints, map[any]any) compare equal to request bodies decoded from JSON.
func normalizeJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
