package pkg

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mockmint/mockmint/internal/state"
)

func loadManifest(manifest string) (*Package, error) {
	return Load(context.Background(), fstest.MapFS{
		"openapi.yaml":  {Data: []byte(miniSpec)},
		"mockmint.yaml": {Data: []byte(manifest)},
	}, "t", Defaults{}, nil)
}

func TestStateConfig(t *testing.T) {
	p, err := loadManifest(`operations:
  GET /ping:
    state: {action: list, collection: pings, where: {b: "{{.Request.Query.b}}", a: x}}
`)
	if err != nil {
		t.Fatal(err)
	}
	s := p.Operations[0].State
	if s.Action != StateList || s.Collection != "pings" || len(s.Where) != 2 || s.Where[0].Field != "a" {
		t.Fatalf("state = %+v", s)
	}
	p, err = loadManifest("operations: {GET /ping: {state: {action: update, collection: c, key: k}}}\n")
	if err != nil || p.Operations[0].State.Value != UpdateMerge {
		t.Fatalf("update default = %+v, %v", p.Operations[0].State, err)
	}

	for _, tt := range []struct{ state, want string }{
		{"{action: fly, collection: c}", "want create, read"},
		{"{action: read, collection: c}", "needs a key"},
		{"{action: read, key: k}", "collection is required"},
		{"{action: list, collection: c, key: k}", "list takes no key"},
		{"{action: read, collection: c, key: '{{ .X '}", "key"},
		{"{action: read, collection: c, key: k, value: merge}", "update only"},
		{"{action: update, collection: c, key: k, value: patch}", "want merge, request or response"},
		{"{action: read, collection: c, key: k, where: {a: b}}", "list only"},
	} {
		_, err := loadManifest("operations: {GET /ping: {state: " + tt.state + "}}\n")
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.state, err, tt.want)
		}
	}
}

func TestProxyConfig(t *testing.T) {
	p, err := loadManifest("proxy: {url: 'https://api.example.com/v1/', timeout: 2s, record: false}\n")
	if err != nil {
		t.Fatal(err)
	}
	px := p.Proxy
	if px.URL.String() != "https://api.example.com/v1" || !px.OnUnmatchedRoute || !px.OnNoExample || px.Timeout != 2*time.Second || px.Record {
		t.Fatalf("proxy = %+v", px)
	}
	for _, tt := range []struct{ proxy, want string }{
		{"{url: '/relative'}", "absolute http(s) URL"},
		{"{url: 'ftp://x'}", "absolute http(s) URL"},
		{"{url: 'http://x?a=1'}", "query or fragment"},
		{"{url: 'http://x', on: [always]}", "proxy.on"},
	} {
		if _, err := loadManifest("proxy: " + tt.proxy + "\n"); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.proxy, err, tt.want)
		}
	}
}

func TestSeedState(t *testing.T) {
	p, err := loadManifest("initialState: {notes: {'2': {id: 2}, '1': {id: 1}}}\n")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := state.NewMemory()
	if err := p.SeedState(ctx, s); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List(ctx, p.Name, "notes")
	if len(entries) != 2 || entries[0].Value.(map[string]any)["id"] != float64(1) {
		t.Fatalf("seeded = %+v", entries)
	}
	// A changed collection is not re-seeded.
	_, _ = s.Delete(ctx, p.Name, "notes", "1")
	_ = p.SeedState(ctx, s)
	if entries, _ = s.List(ctx, p.Name, "notes"); len(entries) != 1 {
		t.Fatalf("re-seeded a non-empty collection: %+v", entries)
	}
	// Nor is one that requests emptied.
	_, _ = s.Delete(ctx, p.Name, "notes", "2")
	_ = p.SeedState(ctx, s)
	if entries, _ = s.List(ctx, p.Name, "notes"); len(entries) != 0 {
		t.Fatalf("re-seeded an emptied collection: %+v", entries)
	}
}
