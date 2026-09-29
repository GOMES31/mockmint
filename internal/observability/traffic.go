package observability

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxBodyPreview bounds the body bytes kept per recorded exchange.
const MaxBodyPreview = 4 << 10

// Redacted replaces sensitive header values.
const Redacted = "[REDACTED]"

// DefaultRedactHeaders are always redacted.
var DefaultRedactHeaders = []string{"Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization", "X-Api-Key"}

// Exchange is one recorded request (HTTP) or message (AMQP).
type Exchange struct {
	ID        uint64            `json:"id"`
	Time      time.Time         `json:"time"`
	Protocol  string            `json:"protocol"` // http, amqp
	Package   string            `json:"package,omitempty"`
	Operation string            `json:"operation,omitempty"`
	Example   string            `json:"example,omitempty"`
	Duration  time.Duration     `json:"durationNs"`
	Method    string            `json:"method,omitempty"`
	Path      string            `json:"path,omitempty"` // HTTP path+query or AMQP routing key
	Status    int               `json:"status,omitempty"`
	Outcome   string            `json:"outcome,omitempty"` // AMQP: ack, reject, requeue; HTTP: proxied
	Headers   map[string]string `json:"requestHeaders,omitempty"`
	Body      string            `json:"requestBody,omitempty"`
	RespBody  string            `json:"responseBody,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

// Traffic is a fixed-capacity ring buffer of recent exchanges, safe for
// concurrent use. Recording never blocks on readers for longer than a copy.
type Traffic struct {
	mu     sync.Mutex
	buf    []Exchange
	next   int // index of the next write
	full   bool
	seq    uint64
	redact map[string]bool // canonical header names
}

// NewTraffic returns a buffer holding the last capacity exchanges. Extra
// header names are redacted in addition to DefaultRedactHeaders.
func NewTraffic(capacity int, redact ...string) *Traffic {
	t := &Traffic{buf: make([]Exchange, max(capacity, 1)), redact: map[string]bool{}}
	for _, h := range append(slices.Clone(DefaultRedactHeaders), redact...) {
		t.redact[http.CanonicalHeaderKey(h)] = true
	}
	return t
}

// Record stores e (assigning its ID). Headers are redacted and bodies
// truncated here, so nothing sensitive or large is ever retained.
func (t *Traffic) Record(e Exchange) {
	if t == nil {
		return
	}
	for k := range e.Headers {
		if t.redact[http.CanonicalHeaderKey(k)] {
			e.Headers[k] = Redacted
		}
	}
	var tr1, tr2 bool
	e.Body, tr1 = preview(e.Body)
	e.RespBody, tr2 = preview(e.RespBody)
	e.Truncated = e.Truncated || tr1 || tr2
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	e.ID = t.seq
	t.buf[t.next] = e
	t.next = (t.next + 1) % len(t.buf)
	if t.next == 0 {
		t.full = true
	}
}

// Headers flattens h for recording (first value per name).
func Headers(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// Filter selects recorded exchanges; zero fields match everything.
type Filter struct {
	Package  string
	Protocol string
	Limit    int // newest first; 0 = all
}

// List returns matching exchanges, newest first.
func (t *Traffic) List(f Filter) []Exchange {
	t.mu.Lock()
	n := t.next
	if t.full {
		n = len(t.buf)
	}
	all := make([]Exchange, 0, n)
	for i := range n {
		// Walk backwards from the newest entry.
		idx := (t.next - 1 - i + len(t.buf)) % len(t.buf)
		all = append(all, t.buf[idx])
	}
	t.mu.Unlock()
	out := all[:0]
	for _, e := range all {
		if (f.Package == "" || e.Package == f.Package) && (f.Protocol == "" || e.Protocol == f.Protocol) {
			out = append(out, e)
			if f.Limit > 0 && len(out) == f.Limit {
				break
			}
		}
	}
	return out
}

// Clear empties the buffer.
func (t *Traffic) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.buf)
	t.next, t.full = 0, false
}

// preview truncates s to MaxBodyPreview bytes on a UTF-8 boundary.
func preview(s string) (string, bool) {
	if len(s) <= MaxBodyPreview {
		return s, false
	}
	cut := MaxBodyPreview
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.Clone(s[:cut]), true
}
