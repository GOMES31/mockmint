package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/mockmint/mockmint/internal/observability"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/problem"
	"github.com/mockmint/mockmint/internal/state"
)

const proxiedSpec = `openapi: 3.0.3
info: {title: shop, version: "1"}
paths:
  /items/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: string}, examples: {one: {value: "1"}}}
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              examples: {one: {value: {id: "1", source: mock}}}
`

// upstream echoes what it received so tests can check the forwarded request.
func upstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/missing" {
			w.WriteHeader(http.StatusNotFound)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"source": "upstream", "method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
			"forwardedHost": r.Header.Get("X-Forwarded-Host"), "body": string(body),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestProxyAndRecorder(t *testing.T) {
	up, hits := upstream(t)
	p := loadFS(t, map[string]string{
		"openapi.yaml":  proxiedSpec,
		"mockmint.yaml": "validation: off\nproxy: {url: \"" + up.URL + "/api\"}\n",
	})
	rec := NewRecorder(10)
	rt, err := NewRouter([]*pkg.Package{p}, RouterOptions{MaxBodyBytes: 1 << 20, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	srv := newServerWith(t, rt)

	// A matching example is still served by the mock.
	r := do(t, srv, "GET", "/shop/1/items/1", "")
	if r.status != 200 || jsonField(t, r.body, "source") != "mock" {
		t.Fatalf("mocked = %d %s", r.status, r.body)
	}
	// noExample: known operation, no matching example → upstream, recorded.
	r = do(t, srv, "GET", "/shop/1/items/2?full=1", "")
	if r.status != 200 || jsonField(t, r.body, "source") != "upstream" || jsonField(t, r.body, "path") != "/api/items/2" || jsonField(t, r.body, "query") != "full=1" {
		t.Fatalf("noExample proxy = %d %s", r.status, r.body)
	}
	// unmatchedRoute: nothing matches under the base path → upstream.
	r = do(t, srv, "POST", "/shop/1/orders", `{"a":1}`)
	if jsonField(t, r.body, "path") != "/api/orders" || jsonField(t, r.body, "body") != `{"a":1}` || jsonField(t, r.body, "forwardedHost") == "" {
		t.Fatalf("unmatchedRoute proxy = %d %s", r.status, r.body)
	}
	// Method not declared on a known path → unmatched → upstream too.
	if r = do(t, srv, "DELETE", "/shop/1/items/1", ""); jsonField(t, r.body, "method") != "DELETE" {
		t.Fatalf("405 not proxied: %d %s", r.status, r.body)
	}
	// Outside the base path is still mockmint's 404.
	if r = do(t, srv, "GET", "/elsewhere", ""); r.status != 404 || problemOf(t, r)["type"] != problem.TypeNotFound {
		t.Fatalf("outside base path = %d %s", r.status, r.body)
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream hits = %d", hits.Load())
	}

	// Only the known operation was recorded, as a mockmint example file.
	files := rec.ExampleFiles("shop")
	f, ok := files["GET /items/{id}"]
	if len(files) != 1 || !ok {
		t.Fatalf("recordings = %+v", files)
	}
	e := f.Examples["recorded-1"]
	if e.Request == nil || e.Request.Params["id"] != "2" || e.Response.Status != 200 || e.Response.MediaType != "application/json" {
		t.Fatalf("example = %+v", e)
	}
	if body, _ := e.Response.Body.(map[string]any); body["source"] != "upstream" {
		t.Fatalf("recorded body = %v", e.Response.Body)
	}
}

func TestRecordedParametersRoundTrip(t *testing.T) {
	up, _ := upstream(t)
	spec := `openapi: 3.0.3
info: {title: shop, version: "1"}
paths:
  /items/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: string}, examples: {one: {value: "1"}}}
      - {name: color, in: query, schema: {type: string}}
      - {name: X-Region, in: header, schema: {type: string}}
      - {name: X-Api-Key, in: header, schema: {type: string}}
    get:
      responses:
        "200": {description: ok, content: {application/json: {examples: {one: {value: {id: "1", source: mock}}}}}}
`
	p := loadFS(t, map[string]string{"openapi.yaml": spec, "mockmint.yaml": "validation: off\nproxy: {url: \"" + up.URL + "/api\"}\n"})
	rec := NewRecorder(10)
	rt, err := NewRouter([]*pkg.Package{p}, RouterOptions{MaxBodyBytes: 1 << 20, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	srv := newServerWith(t, rt)
	r := do(t, srv, "GET", "/shop/1/items/2?color=blue&ignored=yes", "", "X-Region", "west", "X-Api-Key", "secret", "Authorization", "Bearer secret", "X-Other", "omit")
	if r.status != 200 {
		t.Fatalf("proxy = %d %s", r.status, r.body)
	}
	f := rec.ExampleFiles("shop")["GET /items/{id}"]
	req := f.Examples["recorded-1"].Request
	if req == nil || req.Query["color"] != "blue" || req.Query["ignored"] != "yes" || req.Headers["X-Region"] != "west" || len(req.Headers) != 1 {
		t.Fatalf("recorded request = %+v; response = %s; files = %+v", req, r.body, rec.ExampleFiles("shop"))
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	loaded := loadFS(t, map[string]string{"openapi.yaml": spec, "examples/recorded.yaml": string(b)})
	ex := loaded.Operations[0].Examples["recorded-1"]
	if ex == nil || ex.Query["color"] != "blue" || ex.Headers["X-Region"] != "west" {
		t.Fatalf("loaded example = %+v", ex)
	}
}

func TestProxyUpstreamDown(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	url := up.URL
	up.Close()
	p := loadFS(t, map[string]string{
		"openapi.yaml":  proxiedSpec,
		"mockmint.yaml": "validation: off\nproxy: {url: \"" + url + "\", on: [unmatchedRoute]}\n",
	})
	srv := newServer(t, p)
	r := do(t, srv, "GET", "/shop/1/other", "")
	if r.status != 502 || problemOf(t, r)["type"] != problem.TypeBadGateway {
		t.Fatalf("down upstream = %d %s", r.status, r.body)
	}
	// on: [unmatchedRoute] only: no example → the usual 404 problem.
	if r = do(t, srv, "GET", "/shop/1/items/9", ""); r.status != 404 || problemOf(t, r)["type"] != problem.TypeNoMatchingExample {
		t.Fatalf("noExample without trigger = %d %s", r.status, r.body)
	}
}

func TestRootProxyConflict(t *testing.T) {
	cfg := "basePath: /\nproxy: {url: \"http://127.0.0.1:1\"}\n"
	a := loadFS(t, map[string]string{"openapi.yaml": proxiedSpec, "mockmint.yaml": cfg})
	other := strings.NewReplacer("title: shop", "title: other", "/items/{id}", "/things/{id}").Replace(proxiedSpec)
	b := loadFS(t, map[string]string{"openapi.yaml": other, "mockmint.yaml": cfg})
	_, err := NewRouter([]*pkg.Package{a, b}, RouterOptions{MaxBodyBytes: 1024})
	if err == nil || !strings.Contains(err.Error(), "both proxy unmatched routes at /") {
		t.Fatalf("err = %v", err)
	}
}

func TestTrafficAndMetrics(t *testing.T) {
	traffic := observability.NewTraffic(10)
	metrics := observability.NewMetrics()
	rt, err := NewRouter(loadNotebook(t), RouterOptions{MaxBodyBytes: 1 << 20, Traffic: traffic, Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	srv := newServerWith(t, rt)
	do(t, srv, "GET", "/notebook/1.0/notes/1", "", "Authorization", "Bearer secret")
	do(t, srv, "POST", "/notebook/1.0/notes", `{"title":"T","content":"C","status":"draft"}`)
	do(t, srv, "GET", "/nope", "")

	got := traffic.List(observability.Filter{})
	if len(got) != 3 {
		t.Fatalf("traffic = %+v", got)
	}
	nf, post, get := got[0], got[1], got[2]
	if get.Package != "notebook" || get.Operation != "GET /notes/{noteId}" || get.Status != 200 || get.Headers["Authorization"] != observability.Redacted {
		t.Fatalf("GET exchange = %+v", get)
	}
	if !strings.Contains(get.RespBody, "Welcome") || get.Example == "" {
		t.Fatalf("response not captured: %+v", get)
	}
	if post.Status != 201 || post.Body != `{"title":"T","content":"C","status":"draft"}` || post.Example != "created" {
		t.Fatalf("POST exchange = %+v", post)
	}
	if nf.Package != "" || nf.Status != 404 {
		t.Fatalf("unmatched exchange = %+v", nf)
	}
	if metrics.HTTPRequests.Value("notebook", "POST /notes", "201") != 1 || metrics.HTTPRequests.Value("", "", "404") != 1 {
		t.Fatal("request counters wrong")
	}
	var buf bytes.Buffer
	_ = metrics.Registry.WriteText(&buf)
	if !strings.Contains(buf.String(), `mockmint_http_request_duration_seconds_count{package="notebook",operation="GET /notes/{noteId}"} 1`) {
		t.Fatalf("histogram missing:\n%s", buf.String())
	}
}

func TestStateSurvivesRouterRebuild(t *testing.T) {
	store := rtState(t)
	for i := range 2 {
		rt, err := NewRouter(loadNotebook(t), RouterOptions{MaxBodyBytes: 1 << 20, State: store})
		if err != nil {
			t.Fatal(err)
		}
		srv := newServerWith(t, rt)
		if i == 0 {
			if r := do(t, srv, "DELETE", "/notebook/1.0/notes/2", ""); r.status != 204 {
				t.Fatalf("delete = %d", r.status)
			}
			continue
		}
		// A rebuilt router (reload) must not re-seed a changed collection.
		if r := do(t, srv, "GET", "/notebook/1.0/notes/2", ""); r.status != 404 {
			t.Fatalf("deleted note came back after rebuild: %d %s", r.status, r.body)
		}
	}
}

// A router build that fails must not seed state: a rejected reload has to
// leave the live store untouched.
func TestNewRouterFailureDoesNotSeedState(t *testing.T) {
	files := map[string]string{
		"openapi.yaml":  proxiedSpec,
		"mockmint.yaml": "initialState: {items: {'1': {id: \"1\"}}}\n",
	}
	store := state.NewMemory()
	// Two packages with the same name and version: the build fails.
	_, err := NewRouter([]*pkg.Package{loadFS(t, files), loadFS(t, files)}, RouterOptions{State: store})
	if err == nil {
		t.Fatal("expected duplicate packages to fail")
	}
	got, err := store.List(context.Background(), "shop", "items")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("failed build seeded %d entries", len(got))
	}
}
