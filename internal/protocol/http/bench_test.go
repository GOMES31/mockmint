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
	rt, err := NewRouter(loadPetstoreB(b), 1<<20, nil)
	if err != nil {
		b.Fatal(err)
	}
	s := NewServer(rt, Options{}, discard())
	cases := []struct {
		name, method, path, body string
	}{
		{"static-get", "GET", "/petstore/1.0/pets/1", ""},
		{"list-query", "GET", "/petstore/1.0/pets?status=sold", ""},
		{"templated-post", "POST", "/petstore/1.0/pets", `{"name":"Kiwi","kind":"cat"}`},
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
