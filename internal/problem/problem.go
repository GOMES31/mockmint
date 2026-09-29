// Package problem implements RFC 9457 Problem Details for HTTP APIs.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// ContentType is the media type for problem details.
const ContentType = "application/problem+json"

// TypeBase prefixes mockmint-specific problem type URIs.
const TypeBase = "https://mockmint.dev/problems/"

// Well-known problem types.
const (
	TypeNotFound          = TypeBase + "not-found"
	TypeMethodNotAllowed  = TypeBase + "method-not-allowed"
	TypeUnsupportedMedia  = TypeBase + "unsupported-media-type"
	TypeValidation        = TypeBase + "validation-failed"
	TypeNoMatchingExample = TypeBase + "no-matching-example"
	TypeRateLimited       = TypeBase + "rate-limited"
	TypeBodyTooLarge      = TypeBase + "body-too-large"
	TypeInjectedFault     = TypeBase + "injected-fault"
	TypeInternal          = TypeBase + "internal"
)

// Detail is an RFC 9457 problem object. Extension members go in Extensions and
// are serialized at the top level alongside the standard members.
type Detail struct {
	Type       string
	Title      string
	Status     int
	Detail     string
	Instance   string
	Extensions map[string]any
}

// New returns a problem with the given type, status and detail. The title
// defaults to the HTTP status text.
func New(typ string, status int, detail string) *Detail {
	return &Detail{Type: typ, Title: http.StatusText(status), Status: status, Detail: detail}
}

// With sets an extension member and returns p for chaining. Keys that collide
// with standard members are ignored on output.
func (p *Detail) With(key string, value any) *Detail {
	if p.Extensions == nil {
		p.Extensions = make(map[string]any, 1)
	}
	p.Extensions[key] = value
	return p
}

// MarshalJSON flattens extensions next to the standard members, which always win.
func (p *Detail) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, 5+len(p.Extensions))
	for k, v := range p.Extensions {
		m[k] = v
	}
	typ := p.Type
	if typ == "" {
		typ = "about:blank"
	}
	m["type"] = typ
	if p.Title != "" {
		m["title"] = p.Title
	} else {
		delete(m, "title")
	}
	if p.Status != 0 {
		m["status"] = p.Status
	} else {
		delete(m, "status")
	}
	if p.Detail != "" {
		m["detail"] = p.Detail
	} else {
		delete(m, "detail")
	}
	if p.Instance != "" {
		m["instance"] = p.Instance
	} else {
		delete(m, "instance")
	}
	return json.Marshal(m)
}

// Write serializes p to w. Instance defaults to the request path.
func Write(w http.ResponseWriter, r *http.Request, p *Detail) {
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	body, err := json.Marshal(p)
	if err != nil {
		// Extensions are mockmint-controlled; fall back to the bare problem.
		body, _ = json.Marshal(&Detail{Type: p.Type, Title: p.Title, Status: p.Status, Detail: p.Detail, Instance: p.Instance})
	}
	h := w.Header()
	h.Set("Content-Type", ContentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}
