package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mockmint/mockmint/internal/app"
	"github.com/mockmint/mockmint/internal/config"
	"github.com/mockmint/mockmint/internal/problem"
	"github.com/mockmint/mockmint/internal/spec/openapi"
)

const token = "s3cret-token"

type env struct {
	app   *app.App
	admin *httptest.Server
	mock  *httptest.Server
}

func setup(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	cfg := config.Default()
	cfg.Packages.Paths = []string{"../../examples/notebook", "../../examples/orders-events"}
	cfg.Admin.Token = token
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := app.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &env{app: a,
		admin: httptest.NewServer(New(a, Options{Token: token, MaxUploadBytes: 1 << 20, Version: "test"})),
		mock:  httptest.NewServer(a.Server()),
	}
	t.Cleanup(e.admin.Close)
	t.Cleanup(e.mock.Close)
	return e
}

type res struct {
	status int
	header http.Header
	body   string
}

func call(t *testing.T, srv *httptest.Server, method, path, contentType string, body []byte, auth bool) res {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return res{resp.StatusCode, resp.Header, string(b)}
}

func (e *env) get(t *testing.T, path string) res { return call(t, e.admin, "GET", path, "", nil, true) }

func decode[T any](t *testing.T, r res) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return v
}

func problemType(t *testing.T, r res) string {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != problem.ContentType {
		t.Fatalf("content-type %q, body %s", ct, r.body)
	}
	return decode[map[string]any](t, r)["type"].(string)
}

func TestProbesAndAuth(t *testing.T) {
	e := setup(t)
	if r := call(t, e.admin, "GET", "/healthz", "", nil, false); r.status != 200 {
		t.Fatalf("healthz = %d", r.status)
	}
	if r := call(t, e.admin, "GET", "/readyz", "", nil, false); r.status != 200 || !strings.Contains(r.body, "ready") {
		t.Fatalf("readyz = %d %s", r.status, r.body)
	}
	r := call(t, e.admin, "GET", "/metrics", "", nil, false)
	if r.status != 200 || !strings.Contains(r.body, "mockmint_packages_loaded 2") || !strings.Contains(r.body, `mockmint_reloads_total{result="ok"} 1`) {
		t.Fatalf("metrics = %d\n%s", r.status, r.body)
	}
	for _, hdr := range []string{"", "Bearer wrong", "Basic " + token, "Bearer " + token + "x"} {
		req, _ := http.NewRequest(http.MethodGet, e.admin.URL+"/admin/packages", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		resp, err := e.admin.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("auth %q = %d", hdr, resp.StatusCode)
		}
	}
	if r := e.get(t, "/admin/info"); r.status != 200 || decode[map[string]any](t, r)["amqp"] != "disconnected" {
		t.Fatalf("info = %d %s", r.status, r.body)
	}
	// Not found and method not allowed are problems too.
	if r := e.get(t, "/admin/nope"); r.status != 404 || problemType(t, r) != problem.TypeNotFound {
		t.Fatalf("404 = %d %s", r.status, r.body)
	}
	if r := call(t, e.admin, "PATCH", "/admin/packages", "", nil, true); r.status != 405 || r.header.Get("Allow") != "GET" || problemType(t, r) != problem.TypeMethodNotAllowed {
		t.Fatalf("405 = %d %v %s", r.status, r.header, r.body)
	}
}

func TestPackagesListAndDetail(t *testing.T) {
	e := setup(t)
	list := decode[[]map[string]any](t, e.get(t, "/admin/packages"))
	if len(list) != 2 || list[0]["name"] != "notebook" || list[0]["origin"] != "static" || list[1]["asyncOperations"] != float64(3) {
		t.Fatalf("list = %v", list)
	}
	d := decode[map[string]any](t, e.get(t, "/admin/packages/notebook"))
	ops := d["http"].([]any)
	if len(ops) != 5 || ops[0].(map[string]any)["state"] != "list notes" {
		t.Fatalf("detail = %v", d)
	}
	a := decode[map[string]any](t, e.get(t, "/admin/packages/orders-events"))["async"].([]any)
	if len(a) != 3 {
		t.Fatalf("async = %v", a)
	}
	if r := e.get(t, "/admin/packages/nope"); r.status != 404 {
		t.Fatalf("missing package = %d", r.status)
	}
}

func zipPackage(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const pingSpec = `openapi: 3.0.3
info: {title: ping, version: "1"}
paths:
  /ping:
    get:
      responses:
        "200": {description: ok, content: {text/plain: {example: %s}}}
`

func pingArchive(t *testing.T, answer string) []byte {
	return zipPackage(t, map[string]string{"openapi.yaml": strings.Replace(pingSpec, "%s", answer, 1)})
}

func TestUploadReplaceDelete(t *testing.T) {
	dir := t.TempDir()
	e := setup(t, func(c *config.Config) { c.Admin.DataDir = dir })

	put := func(name string, data []byte, ct string) res {
		return call(t, e.admin, "PUT", "/admin/packages/"+name, ct, data, true)
	}
	if r := put("ping", pingArchive(t, "pong"), "application/zip"); r.status != 201 {
		t.Fatalf("create = %d %s", r.status, r.body)
	}
	if r := call(t, e.mock, "GET", "/ping/1/ping", "", nil, false); r.body != "pong" {
		t.Fatalf("uploaded package not served: %d %s", r.status, r.body)
	}
	if r := put("ping", pingArchive(t, "pong2"), "application/zip"); r.status != 200 {
		t.Fatalf("replace = %d %s", r.status, r.body)
	}
	if r := call(t, e.mock, "GET", "/ping/1/ping", "", nil, false); r.body != "pong2" {
		t.Fatalf("replacement not served: %s", r.body)
	}
	if _, err := os.Stat(filepath.Join(dir, "packages", "ping.zip")); err != nil {
		t.Fatalf("upload not persisted: %v", err)
	}

	// Invalid uploads change nothing.
	for _, tc := range []struct {
		name, ct, wantType string
		data               []byte
		status             int
	}{
		{"ping", "application/zip", problem.TypeInvalidPackage, zipPackage(t, map[string]string{"openapi.yaml": "openapi: 3.0.3\ninfo: {title: ping, version: '1'}\npaths: {}\n"}), 422},
		{"other", "application/zip", problem.TypeInvalidPackage, pingArchive(t, "x"), 422}, // archive is "ping", URL says "other"
		{"notebook", "application/zip", problem.TypeConflict, pingArchive(t, "x"), 409},
		{"ping", "text/plain", problem.TypeUnsupportedMedia, []byte("x"), 415},
		{"ping", "application/zip", problem.TypeBodyTooLarge, bytes.Repeat([]byte{0}, 2<<20), 413},
	} {
		r := put(tc.name, tc.data, tc.ct)
		if r.status != tc.status || problemType(t, r) != tc.wantType {
			t.Errorf("%s %s: %d %s", tc.name, tc.ct, r.status, r.body)
		}
	}
	if r := call(t, e.mock, "GET", "/ping/1/ping", "", nil, false); r.body != "pong2" {
		t.Fatalf("failed upload changed the live set: %s", r.body)
	}

	// A new app with the same dataDir restores the upload.
	cfg := config.Default()
	cfg.Admin.DataDir = dir
	a2, err := app.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a2.Reload(context.Background()); err != nil || len(a2.Packages()) != 1 || a2.Origin("ping") != app.OriginUploaded {
		t.Fatalf("restore: %v %d", err, len(a2.Packages()))
	}

	if r := call(t, e.admin, "DELETE", "/admin/packages/notebook", "", nil, true); r.status != 409 {
		t.Fatalf("delete static = %d", r.status)
	}
	if r := call(t, e.admin, "DELETE", "/admin/packages/ping", "", nil, true); r.status != 204 {
		t.Fatalf("delete = %d %s", r.status, r.body)
	}
	if r := call(t, e.mock, "GET", "/ping/1/ping", "", nil, false); r.status != 404 {
		t.Fatalf("deleted package still served: %d", r.status)
	}
	if _, err := os.Stat(filepath.Join(dir, "packages", "ping.zip")); !os.IsNotExist(err) {
		t.Fatal("deleted upload still persisted")
	}
	if r := call(t, e.admin, "DELETE", "/admin/packages/ping", "", nil, true); r.status != 404 {
		t.Fatalf("delete twice = %d", r.status)
	}
}

func TestReloadIsAtomic(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../examples/notebook", filepath.Join(dir, "notebook"))
	e := setup(t, func(c *config.Config) { c.Packages.Paths = []string{filepath.Join(dir, "notebook")} })

	// Break the package on disk: reload fails, the old set keeps serving.
	manifest := filepath.Join(dir, "notebook", "mockmint.yaml")
	if err := os.WriteFile(manifest, []byte("validation: loud\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := call(t, e.admin, "POST", "/admin/reload", "", nil, true)
	if r.status != 422 || problemType(t, r) != problem.TypeInvalidPackage || !strings.Contains(r.body, "validation") {
		t.Fatalf("broken reload = %d %s", r.status, r.body)
	}
	if got := call(t, e.mock, "GET", "/notebook/1.0/notes/1", "", nil, false); got.status != 200 {
		t.Fatalf("old set not serving after failed reload: %d", got.status)
	}
	// Fix it with a different base path: the reload swaps it in.
	if err := os.WriteFile(manifest, []byte("basePath: /nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r = call(t, e.admin, "POST", "/admin/reload", "", nil, true); r.status != 200 {
		t.Fatalf("reload = %d %s", r.status, r.body)
	}
	if got := call(t, e.mock, "GET", "/nb/notes", "", nil, false); got.status != 200 {
		t.Fatalf("new base path not served: %d", got.status)
	}
	if got := call(t, e.mock, "GET", "/notebook/1.0/notes", "", nil, false); got.status != 404 {
		t.Fatalf("old base path still served: %d", got.status)
	}
}

func TestTrafficStateRecordings(t *testing.T) {
	e := setup(t)
	call(t, e.mock, "DELETE", "/notebook/1.0/notes/2", "", nil, false)
	call(t, e.mock, "GET", "/notebook/1.0/notes/1", "", nil, false)

	tr := decode[[]map[string]any](t, e.get(t, "/admin/traffic?package=notebook&limit=1"))
	if len(tr) != 1 || tr[0]["method"] != "GET" || tr[0]["status"] != float64(200) {
		t.Fatalf("traffic = %v", tr)
	}
	if r := e.get(t, "/admin/traffic?limit=x"); r.status != 400 {
		t.Fatalf("bad limit = %d", r.status)
	}
	if r := call(t, e.admin, "DELETE", "/admin/traffic", "", nil, true); r.status != 204 || len(decode[[]any](t, e.get(t, "/admin/traffic"))) != 0 {
		t.Fatal("traffic not cleared")
	}

	st := decode[[]map[string]any](t, e.get(t, "/admin/state/notebook?collection=notes"))
	if len(st) != 1 || st[0]["key"] != "1" {
		t.Fatalf("state after delete = %v", st)
	}
	// Clearing re-seeds initialState.
	if r := call(t, e.admin, "DELETE", "/admin/state/notebook", "", nil, true); r.status != 204 {
		t.Fatalf("clear state = %d", r.status)
	}
	if st = decode[[]map[string]any](t, e.get(t, "/admin/state/notebook")); len(st) != 2 {
		t.Fatalf("state after clear = %v", st)
	}
	if r := e.get(t, "/admin/state/unknown"); r.status != 200 || strings.TrimSpace(r.body) != "[]" {
		t.Fatalf("unknown namespace = %d %s", r.status, r.body)
	}

	if r := e.get(t, "/admin/recordings/notebook"); r.status != 200 || r.header.Get("Content-Type") != "application/yaml" {
		t.Fatalf("recordings = %d %v", r.status, r.header)
	}
	if r := e.get(t, "/admin/recordings/notebook?format=json"); strings.TrimSpace(r.body) != "[]" {
		t.Fatalf("json recordings = %s", r.body)
	}
}

func TestPublishWithoutBroker(t *testing.T) {
	e := setup(t)
	r := call(t, e.admin, "POST", "/admin/async/orders-events/order.created/publish", "application/json", []byte(`{"example":"widget"}`), true)
	if r.status != 503 {
		t.Fatalf("publish without broker = %d %s", r.status, r.body)
	}
	for path, want := range map[string]int{
		"/admin/async/orders-events/nope/publish":           404,
		"/admin/async/orders-events/orderRequests/publish":  404, // a receive channel
		"/admin/async/notebook/order.created/publish":       404,
		"/admin/async/orders-events/orderEvents/publish?x=": 503, // by channel id
	} {
		if r := call(t, e.admin, "POST", path, "", nil, true); r.status != want {
			t.Errorf("%s = %d %s", path, r.status, r.body)
		}
	}
	if r := call(t, e.admin, "POST", "/admin/async/orders-events/orderEvents/publish", "application/json", []byte("{"), true); r.status != 400 {
		t.Fatalf("bad JSON = %d", r.status)
	}
}

func TestPublishUnknownExampleIsNotFound(t *testing.T) {
	e := setup(t)
	r := call(t, e.admin, "POST", "/admin/async/orders-events/order.created/publish", "application/json", []byte(`{"example":"missing"}`), true)
	if r.status != http.StatusNotFound {
		t.Fatalf("unknown example = %d %s", r.status, r.body)
	}
}

// TestSpecMatchesRoutes loads docs/admin-openapi.yaml with mockmint's own
// loader and checks that every documented operation is served.
func TestSpecMatchesRoutes(t *testing.T) {
	spec, err := openapi.Load(context.Background(), os.DirFS("../../docs"), "admin-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	for _, op := range spec.Operations {
		path := strings.NewReplacer("{name}", "nope", "{package}", "nope", "{channel}", "nope").Replace(op.Path)
		r := call(t, e.admin, op.Method, path, "application/zip", nil, true)
		if r.status == 405 || (r.status == 404 && strings.Contains(r.body, "no admin endpoint")) {
			t.Errorf("%s %s is documented but not served (%d)", op.Method, op.Path, r.status)
		}
	}
	if len(spec.Operations) < 15 {
		t.Fatalf("spec has only %d operations", len(spec.Operations))
	}
}

func TestReadyzBeforeLoad(t *testing.T) {
	a, err := app.New(config.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(a, Options{}))
	defer srv.Close()
	if r := call(t, srv, "GET", "/readyz", "", nil, false); r.status != 503 || !strings.Contains(r.body, "not loaded") {
		t.Fatalf("readyz before load = %d %s", r.status, r.body)
	}
	// Without a token, /admin is open.
	if r := call(t, srv, "GET", "/admin/packages", "", nil, false); r.status != 200 {
		t.Fatalf("open admin = %d", r.status)
	}
}

func TestReadyzWithDisconnectedBroker(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.AMQP.URL = "amqp://guest:guest@127.0.0.1:1/"
		c.AMQP.ReconnectMin = config.Duration(50 * time.Millisecond)
		c.AMQP.ReconnectMax = config.Duration(100 * time.Millisecond)
	})
	r := call(t, e.admin, "GET", "/readyz", "", nil, false)
	if r.status != 503 || !strings.Contains(r.body, "RabbitMQ") {
		t.Fatalf("readyz = %d %s", r.status, r.body)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
