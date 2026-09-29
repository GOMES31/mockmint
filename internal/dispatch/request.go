// Package dispatch selects which named example answers a request.
package dispatch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
)

// Request is the protocol-neutral view of an incoming request that
// dispatchers match against. It is not safe for concurrent use.
type Request struct {
	Method     string
	Path       string
	PathParams map[string]string
	Query      url.Values
	Header     http.Header
	Body       []byte

	jsonOnce sync.Once
	json     any
	jsonOK   bool
}

// JSON returns the body decoded as JSON (numbers as float64), decoding it at
// most once. ok is false when the body is empty or not valid JSON.
func (r *Request) JSON() (v any, ok bool) {
	r.jsonOnce.Do(func() {
		b := bytes.TrimSpace(r.Body)
		if len(b) == 0 {
			return
		}
		r.jsonOK = json.Unmarshal(b, &r.json) == nil
	})
	return r.json, r.jsonOK
}

// Dispatcher chooses an example for a request. ok is false when nothing
// matched and there is no default; the caller then applies its fallback.
// err reports a dispatcher failure (e.g. a script runtime error).
type Dispatcher interface {
	Dispatch(r *Request) (example string, ok bool, err error)
}
