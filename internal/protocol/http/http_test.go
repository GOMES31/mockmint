package http

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/problem"
)

func loadPetstore(t *testing.T) []*pkg.Package {
	t.Helper()
	pkgs, err := pkg.LoadPath(context.Background(), "../../../examples/petstore", pkg.Defaults{Validation: "warn"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

func loadFS(t *testing.T, files map[string]string) *pkg.Package {
	t.Helper()
	fsys := fstest.MapFS{}
	for k, v := range files {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	p, err := pkg.Load(context.Background(), fsys, "test", pkg.Defaults{Validation: "strict"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newServer(t *testing.T, pkgs ...*pkg.Package) *httptest.Server {
	t.Helper()
	rt, err := NewRouter(pkgs, 1<<10, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(rt, Options{}, discard()))
	t.Cleanup(srv.Close)
	return srv
}

type result struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, headers ...string) result {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

func problemOf(t *testing.T, r result) map[string]any {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != problem.ContentType {
		t.Fatalf("content-type = %q, body %s", ct, r.body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPetstoreEndToEnd(t *testing.T) {
	srv := newServer(t, loadPetstore(t)...)
	base := "/petstore/1.0"

	// auto dispatch: request halves from parameter examples.
	r := do(t, srv, "GET", base+"/pets/2", "")
	if r.status != 200 || !strings.Contains(r.body, `"name":"Rex"`) || r.header.Get(HeaderExample) != "rex" {
		t.Fatalf("GET /pets/2 = %d %s", r.status, r.body)
	}
	// fallback example for unmatched ids.
	r = do(t, srv, "GET", base+"/pets/77", "")
	if r.status != 404 || r.header.Get("Content-Type") != "application/problem+json" || !strings.Contains(r.body, "No such pet") {
		t.Fatalf("GET /pets/77 = %d %s", r.status, r.body)
	}
	// query_params dispatcher.
	if r = do(t, srv, "GET", base+"/pets?status=sold", ""); r.header.Get(HeaderExample) != "sold" {
		t.Fatalf("sold = %s", r.header.Get(HeaderExample))
	}
	if r = do(t, srv, "GET", base+"/pets", ""); r.header.Get(HeaderExample) != "all" {
		t.Fatalf("all = %s", r.header.Get(HeaderExample))
	}
	// body_jsonpath dispatcher + templated response.
	r = do(t, srv, "POST", base+"/pets", `{"name":"Kiwi","kind":"bird"}`)
	if r.status != 422 || r.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("bird = %d %s", r.status, r.body)
	}
	r = do(t, srv, "POST", base+"/pets", `{"name":"Tom \"2\"","kind":"cat"}`)
	var pet map[string]any
	if r.status != 201 || json.Unmarshal([]byte(r.body), &pet) != nil {
		t.Fatalf("create = %d %s", r.status, r.body)
	}
	if pet["name"] != `Tom "2"` || pet["createdAt"] != "2025-01-01T12:00:00.000Z" || r.header.Get("X-Created-Name") != `Tom "2"` {
		t.Fatalf("templated = %s headers %v", r.body, r.header)
	}
	// Deterministic under seed: same request, same body.
	if again := do(t, srv, "POST", base+"/pets", `{"name":"Tom \"2\"","kind":"cat"}`); again.body != r.body {
		t.Fatalf("not deterministic:\n%s\n%s", r.body, again.body)
	}
	// sequence dispatcher.
	for _, want := range []string{"placed", "approved", "delivered", "delivered"} {
		r = do(t, srv, "GET", base+"/orders/o-1/status", "")
		if !strings.Contains(r.body, `"status": "`+want+`"`) || !strings.Contains(r.body, `"orderId": "o-1"`) {
			t.Fatalf("sequence want %s, got %s", want, r.body)
		}
	}
	// 204 without body or content type.
	r = do(t, srv, "DELETE", base+"/pets/1", "")
	if r.status != 204 && r.status != 503 {
		t.Fatalf("delete = %d", r.status)
	}
	if r.status == 204 && (r.body != "" || r.header.Get("Content-Type") != "") {
		t.Fatalf("204 carried a body: %q %q", r.body, r.header.Get("Content-Type"))
	}
}

func TestStrictValidation(t *testing.T) {
	srv := newServer(t, loadPetstore(t)...)
	r := do(t, srv, "POST", "/petstore/1.0/pets", `{"name":"","kind":"lizard"}`)
	if r.status != 400 {
		t.Fatalf("status = %d %s", r.status, r.body)
	}
	p := problemOf(t, r)
	errs, _ := p["errors"].([]any)
	if p["type"] != problem.TypeValidation || len(errs) != 2 {
		t.Fatalf("problem = %v", p)
	}
	r = do(t, srv, "POST", "/petstore/1.0/pets", `name=x`, "Content-Type", "application/x-www-form-urlencoded")
	if r.status != 415 || problemOf(t, r)["type"] != problem.TypeUnsupportedMedia {
		t.Fatalf("415 = %d %s", r.status, r.body)
	}
	r = do(t, srv, "GET", "/petstore/1.0/pets/abc", "")
	if r.status != 400 {
		t.Fatalf("bad path param = %d", r.status)
	}
	r = do(t, srv, "GET", "/petstore/1.0/pets?limit=500", "")
	if r.status != 400 {
		t.Fatalf("bad query param = %d", r.status)
	}
}

func TestWarnValidation(t *testing.T) {
	p := loadFS(t, map[string]string{
		"openapi.yaml":  specWithBody,
		"mockmint.yaml": "validation: warn\n",
	})
	srv := newServer(t, p)
	r := do(t, srv, "POST", "/t/1/items", `{"n":"x"}`)
	if r.status != 201 || r.header.Get(HeaderValidation) != "failed" {
		t.Fatalf("warn = %d %v", r.status, r.header)
	}
}

const specWithBody = `openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /items:
    post:
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {n: {type: integer}}}
      responses:
        "201":
          description: ok
          content:
            application/json:
              example: {ok: true}
`

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	srv := newServer(t, loadPetstore(t)...)
	r := do(t, srv, "GET", "/nope", "")
	if r.status != 404 || problemOf(t, r)["type"] != problem.TypeNotFound {
		t.Fatalf("404 = %d %s", r.status, r.body)
	}
	r = do(t, srv, "PUT", "/petstore/1.0/pets/1", "")
	if r.status != 405 || r.header.Get("Allow") != "DELETE, GET, HEAD" {
		t.Fatalf("405 = %d allow %q", r.status, r.header.Get("Allow"))
	}
	p := problemOf(t, r)
	if p["type"] != problem.TypeMethodNotAllowed || p["instance"] != "/petstore/1.0/pets/1" {
		t.Fatalf("problem = %v", p)
	}
	r = do(t, srv, "HEAD", "/petstore/1.0/pets/1", "")
	if r.status != 200 || r.body != "" || r.header.Get("Content-Length") == "" {
		t.Fatalf("HEAD = %d %q %v", r.status, r.body, r.header)
	}
}

func TestBodyTooLarge(t *testing.T) {
	srv := newServer(t, loadPetstore(t)...) // limit 1 KiB
	r := do(t, srv, "POST", "/petstore/1.0/pets", `{"name":"`+strings.Repeat("x", 2048)+`","kind":"cat"}`)
	if r.status != 413 || problemOf(t, r)["type"] != problem.TypeBodyTooLarge {
		t.Fatalf("413 = %d %s", r.status, r.body)
	}
}

const routingSpec = `openapi: 3.0.3
info: {title: r, version: "1"}
paths:
  /files/{name}.json:
    parameters: [{name: name, in: path, required: true, schema: {type: string}}]
    get:
      responses: {"200": {description: ok, content: {text/plain: {example: "{{.Request.Params.name}} as json"}}}}
  /files/{id}:
    parameters: [{name: id, in: path, required: true, schema: {type: string}}]
    get:
      responses: {"200": {description: ok, content: {text/plain: {example: "file {{.Request.Params.id}}"}}}}
  /v/{major}.{minor}/x:
    parameters:
      - {name: major, in: path, required: true, schema: {type: integer}}
      - {name: minor, in: path, required: true, schema: {type: integer}}
    get:
      responses: {"200": {description: ok, content: {text/plain: {example: "{{.Request.Params.major}}-{{.Request.Params.minor}}"}}}}
  /users/{user-id}/:
    parameters: [{name: user-id, in: path, required: true, schema: {type: string}}]
    get:
      responses: {"200": {description: ok, content: {text/plain: {example: "slash {{index .Request.Params \"user-id\"}}"}}}}
  /users/me:
    get:
      responses: {"200": {description: ok, content: {text/plain: {example: "me"}}}}
`

func TestRouting(t *testing.T) {
	p := loadFS(t, map[string]string{"openapi.yaml": routingSpec, "mockmint.yaml": "basePath: /\n"})
	srv := newServer(t, p)
	for path, want := range map[string]string{
		"/files/report.json": "report as json",
		"/files/report.xml":  "file report.xml",
		"/v/2.10/x":          "2-10",
		"/users/me":          "me",
		"/users/abc/":        "slash abc",
		"/files/a%20b.json":  "a b as json",
	} {
		if r := do(t, srv, "GET", path, ""); r.status != 200 || r.body != want {
			t.Errorf("%s = %d %q, want %q", path, r.status, r.body, want)
		}
	}
	if r := do(t, srv, "GET", "/users/abc", ""); r.status != 404 {
		t.Errorf("trailing-slash template matched without slash: %d", r.status)
	}
	if r := do(t, srv, "GET", "/v/2/x", ""); r.status != 404 {
		t.Errorf("mixed segment mismatch should 404, got %d", r.status)
	}
}

func TestRouterConflicts(t *testing.T) {
	a := loadFS(t, map[string]string{"openapi.yaml": specWithBody})
	b := loadFS(t, map[string]string{"openapi.yaml": specWithBody})
	if _, err := NewRouter([]*pkg.Package{a, b}, 1024, nil); err == nil || !strings.Contains(err.Error(), "are both t@1") {
		t.Fatalf("duplicate package err = %v", err)
	}
	c := loadFS(t, map[string]string{"openapi.yaml": `openapi: 3.0.3
info: {title: c, version: "1"}
paths:
  /{a}/x:
    parameters: [{name: a, in: path, required: true, schema: {type: string}}]
    get: {responses: {"204": {description: ok}}}
  /x/{b}:
    parameters: [{name: b, in: path, required: true, schema: {type: string}}]
    get: {responses: {"204": {description: ok}}}
`})
	if _, err := NewRouter([]*pkg.Package{c}, 1024, nil); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict err = %v", err)
	}
}

func TestFaultsAndRateLimit(t *testing.T) {
	p := loadFS(t, map[string]string{
		"openapi.yaml": specWithBody,
		"mockmint.yaml": `
validation: off
operations:
  POST /items:
    behavior:
      faults: [{probability: 1, status: 503}]
      latency: 30ms
`})
	srv := newServer(t, p)
	start := time.Now()
	r := do(t, srv, "POST", "/t/1/items", `{}`)
	if r.status != 503 || problemOf(t, r)["type"] != problem.TypeInjectedFault {
		t.Fatalf("fault = %d %s", r.status, r.body)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("latency not applied before fault")
	}

	drop := loadFS(t, map[string]string{
		"openapi.yaml":  specWithBody,
		"mockmint.yaml": "behavior: {faults: [{probability: 1, action: drop}]}\n",
	})
	dsrv := newServer(t, drop)
	resp, err := dsrv.Client().Post(dsrv.URL+"/t/1/items", "application/json", strings.NewReader(`{}`))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected dropped connection, got %d", resp.StatusCode)
	}

	limited := loadFS(t, map[string]string{
		"openapi.yaml":  specWithBody,
		"mockmint.yaml": "behavior: {rateLimit: {rps: 0.001, burst: 2}}\n",
	})
	lsrv := newServer(t, limited)
	for i := range 2 {
		if r := do(t, lsrv, "POST", "/t/1/items", `{}`); r.status != 201 {
			t.Fatalf("request %d = %d", i, r.status)
		}
	}
	r = do(t, lsrv, "POST", "/t/1/items", `{}`)
	if r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("429 = %d %v", r.status, r.header)
	}
}

func TestNoMatchingExample(t *testing.T) {
	p := loadFS(t, map[string]string{
		"openapi.yaml":  specWithBody,
		"mockmint.yaml": "operations: {POST /items: {dispatcher: {type: header, rules: [{when: {x-a: b}, example: \"201\"}], default: \"-\"}}}\n",
	})
	srv := newServer(t, p)
	r := do(t, srv, "POST", "/t/1/items", `{}`)
	if r.status != 404 || problemOf(t, r)["type"] != problem.TypeNoMatchingExample {
		t.Fatalf("no match = %d %s", r.status, r.body)
	}
	if r = do(t, srv, "POST", "/t/1/items", `{}`, "X-A", "b"); r.status != 201 {
		t.Fatalf("match = %d", r.status)
	}
}

func TestSwapIsAtomicUnderLoad(t *testing.T) {
	a := loadFS(t, map[string]string{"openapi.yaml": specWithBody})
	rtA, err := NewRouter([]*pkg.Package{a}, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	rtEmpty, err := NewRouter(nil, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(rtA, Options{}, discard())
	srv := httptest.NewServer(s)
	defer srv.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				s.Swap(rtEmpty)
			} else {
				s.Swap(rtA)
			}
		}
	})
	for range 200 {
		r := do(t, srv, "POST", "/t/1/items", `{}`)
		if r.status != 201 && r.status != 404 {
			t.Fatalf("unexpected status during swap: %d %s", r.status, r.body)
		}
	}
	close(stop)
	wg.Wait()
}

func TestServeAndShutdown(t *testing.T) {
	rt, err := NewRouter(loadPetstore(t), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(rt, Options{ReadHeaderTimeout: time.Second}, discard())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ln) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/petstore/1.0/pets/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v after Shutdown", err)
	}
}

func TestSeededHeaderTemplatesDeterministic(t *testing.T) {
	p := loadFS(t, map[string]string{
		"openapi.yaml":  specWithBody,
		"mockmint.yaml": "seed: 7\nvalidation: off\n",
		"examples/x.yaml": `operation: POST /items
examples:
  default:
    response:
      status: 201
      headers: {X-A: "{{.UUID}}", X-B: "{{.UUID}}", X-C: "{{.UUID}}", X-D: "{{.UUID}}"}
      body: '{"id":"{{.UUID}}"}'
`,
	})
	srv := newServer(t, p)
	first := do(t, srv, "POST", "/t/1/items", `{}`)
	for range 30 {
		r := do(t, srv, "POST", "/t/1/items", `{}`)
		for _, h := range []string{"X-A", "X-B", "X-C", "X-D"} {
			if r.header.Get(h) != first.header.Get(h) {
				t.Fatalf("%s differs across identical seeded requests: %s vs %s", h, r.header.Get(h), first.header.Get(h))
			}
		}
		if r.body != first.body {
			t.Fatal("body differs across identical seeded requests")
		}
	}
}
