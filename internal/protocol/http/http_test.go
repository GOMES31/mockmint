package http

import (
	"context"
	"encoding/json"
	"fmt"
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

func loadNotebook(t *testing.T) []*pkg.Package {
	t.Helper()
	pkgs, err := pkg.LoadPath(context.Background(), "../../../examples/notebook", pkg.Defaults{Validation: "warn"}, nil)
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
	rt, err := NewRouter(pkgs, RouterOptions{MaxBodyBytes: 1 << 10})
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

func TestNotebookEndToEnd(t *testing.T) {
	srv := newServer(t, loadNotebook(t)...)
	base := "/notebook/1.0"

	// Seeded state: GET reads the stored note.
	r := do(t, srv, "GET", base+"/notes/2", "")
	if r.status != 200 || jsonField(t, r.body, "title") != "Ideas" || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET /notes/2 = %d %s", r.status, r.body)
	}
	// Missing key: the operation's fallback example.
	r = do(t, srv, "GET", base+"/notes/77", "")
	if r.status != 404 || r.header.Get("Content-Type") != "application/problem+json" || !strings.Contains(r.body, "No such note") {
		t.Fatalf("GET /notes/77 = %d %s", r.status, r.body)
	}
	// List with a where filter from the query.
	if r = do(t, srv, "GET", base+"/notes?status=published", ""); countItems(t, r.body) != 1 {
		t.Fatalf("published = %s", r.body)
	}
	if r = do(t, srv, "GET", base+"/notes", ""); countItems(t, r.body) != 2 {
		t.Fatalf("all = %s", r.body)
	}
	// body_jsonpath dispatcher: an error example leaves state untouched.
	r = do(t, srv, "POST", base+"/notes", `{"title":"News","content":"Ready","status":"published"}`)
	if r.status != 422 || r.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("publish-first = %d %s", r.status, r.body)
	}
	if r = do(t, srv, "GET", base+"/notes", ""); countItems(t, r.body) != 2 {
		t.Fatalf("422 changed state: %s", r.body)
	}
	// Create: templated response, stored under its id.
	r = do(t, srv, "POST", base+"/notes", `{"title":"Meeting \"2\"","content":"Agenda","status":"draft"}`)
	var note map[string]any
	if r.status != 201 || json.Unmarshal([]byte(r.body), &note) != nil {
		t.Fatalf("create = %d %s", r.status, r.body)
	}
	if note["title"] != `Meeting "2"` || note["createdAt"] != "2025-01-01T12:00:00.000Z" || r.header.Get("X-Created-Title") != `Meeting "2"` {
		t.Fatalf("templated = %s headers %v", r.body, r.header)
	}
	id := fmt.Sprint(note["id"])
	// Deterministic under seed: same request, same body (and same id).
	if again := do(t, srv, "POST", base+"/notes", `{"title":"Meeting \"2\"","content":"Agenda","status":"draft"}`); again.body != r.body {
		t.Fatalf("not deterministic:\n%s\n%s", r.body, again.body)
	}
	if got := do(t, srv, "GET", base+"/notes/"+id, ""); got.status != 200 || jsonField(t, got.body, "title") != `Meeting "2"` {
		t.Fatalf("read after create = %d %s", got.status, got.body)
	}
	// Update merges the request into the stored note (id and createdAt stay).
	r = do(t, srv, "PUT", base+"/notes/"+id, `{"title":"Meeting 3","content":"Revised","status":"published"}`)
	if r.status != 200 || jsonField(t, r.body, "title") != "Meeting 3" || jsonField(t, r.body, "createdAt") != "2025-01-01T12:00:00.000Z" {
		t.Fatalf("update = %d %s", r.status, r.body)
	}
	if r = do(t, srv, "GET", base+"/notes?status=published", ""); countItems(t, r.body) != 2 {
		t.Fatalf("list after update = %s", r.body)
	}
	if r = do(t, srv, "PUT", base+"/notes/404", `{"title":"x","content":"y","status":"draft"}`); r.status != 404 || problemOf(t, r)["type"] != problem.TypeNotFound {
		t.Fatalf("update missing = %d %s", r.status, r.body)
	}
	// Delete: 204 without body, then the note is gone. deleteNote injects
	// a seeded 10% 503; this request's seed does not trigger it.
	r = do(t, srv, "DELETE", base+"/notes/"+id, "")
	if r.status != 204 || r.body != "" || r.header.Get("Content-Type") != "" {
		t.Fatalf("delete = %d %q %q", r.status, r.body, r.header.Get("Content-Type"))
	}
	if r = do(t, srv, "GET", base+"/notes/"+id, ""); r.status != 404 {
		t.Fatalf("read after delete = %d", r.status)
	}
}

func jsonField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return fmt.Sprint(m[field])
}

func countItems(t *testing.T, body string) int {
	t.Helper()
	var items []any
	if err := json.Unmarshal([]byte(body), &items); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return len(items)
}

func TestStrictValidation(t *testing.T) {
	srv := newServer(t, loadNotebook(t)...)
	r := do(t, srv, "POST", "/notebook/1.0/notes", `{"title":"","content":"Text","status":"missing"}`)
	if r.status != 400 {
		t.Fatalf("status = %d %s", r.status, r.body)
	}
	p := problemOf(t, r)
	errs, _ := p["errors"].([]any)
	if p["type"] != problem.TypeValidation || len(errs) != 2 {
		t.Fatalf("problem = %v", p)
	}
	r = do(t, srv, "POST", "/notebook/1.0/notes", `title=x`, "Content-Type", "application/x-www-form-urlencoded")
	if r.status != 415 || problemOf(t, r)["type"] != problem.TypeUnsupportedMedia {
		t.Fatalf("415 = %d %s", r.status, r.body)
	}
	r = do(t, srv, "GET", "/notebook/1.0/notes/abc", "")
	if r.status != 400 {
		t.Fatalf("bad path param = %d", r.status)
	}
	r = do(t, srv, "GET", "/notebook/1.0/notes?limit=500", "")
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
	srv := newServer(t, loadNotebook(t)...)
	r := do(t, srv, "GET", "/nope", "")
	if r.status != 404 || problemOf(t, r)["type"] != problem.TypeNotFound {
		t.Fatalf("404 = %d %s", r.status, r.body)
	}
	r = do(t, srv, "PATCH", "/notebook/1.0/notes/1", "")
	if r.status != 405 || r.header.Get("Allow") != "DELETE, GET, HEAD, PUT" {
		t.Fatalf("405 = %d allow %q", r.status, r.header.Get("Allow"))
	}
	p := problemOf(t, r)
	if p["type"] != problem.TypeMethodNotAllowed || p["instance"] != "/notebook/1.0/notes/1" {
		t.Fatalf("problem = %v", p)
	}
	r = do(t, srv, "HEAD", "/notebook/1.0/notes/1", "")
	if r.status != 200 || r.body != "" || r.header.Get("Content-Length") == "" {
		t.Fatalf("HEAD = %d %q %v", r.status, r.body, r.header)
	}
}

func TestBodyTooLarge(t *testing.T) {
	srv := newServer(t, loadNotebook(t)...) // limit 1 KiB
	r := do(t, srv, "POST", "/notebook/1.0/notes", `{"title":"`+strings.Repeat("x", 2048)+`","content":"Text","status":"draft"}`)
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
	if _, err := NewRouter([]*pkg.Package{a, b}, RouterOptions{MaxBodyBytes: 1024}); err == nil || !strings.Contains(err.Error(), "are both t@1") {
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
	if _, err := NewRouter([]*pkg.Package{c}, RouterOptions{MaxBodyBytes: 1024}); err == nil || !strings.Contains(err.Error(), "conflicts") {
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
	rtA, err := NewRouter([]*pkg.Package{a}, RouterOptions{MaxBodyBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	rtEmpty, err := NewRouter(nil, RouterOptions{MaxBodyBytes: 1024})
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
	rt, err := NewRouter(loadNotebook(t), RouterOptions{MaxBodyBytes: 1024})
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
	resp, err := http.Get("http://" + ln.Addr().String() + "/notebook/1.0/notes/1")
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
