package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkServe measures the in-process request path (no network) for a
// static example, a templated example, and a strictly validated POST.
func BenchmarkServe(b *testing.B) {
	rt, err := NewRouter(loadNotebookB(b), RouterOptions{MaxBodyBytes: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	s := NewServer(rt, Options{}, discard())
	cases := []struct {
		name, method, path, body string
	}{
		{"static-get", "GET", "/notebook/1.0/notes/1", ""},
		{"list-query", "GET", "/notebook/1.0/notes?status=published", ""},
		{"templated-post", "POST", "/notebook/1.0/notes", `{"title":"Meeting","content":"Agenda","status":"draft"}`},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
				if c.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, req)
				if rec.Code >= http.StatusBadRequest {
					b.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
			}
		})
	}
}
