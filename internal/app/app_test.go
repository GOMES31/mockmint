package app

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mockmint/mockmint/internal/config"
	"github.com/mockmint/mockmint/internal/observability"
	"github.com/mockmint/mockmint/internal/pkg"
	mamqp "github.com/mockmint/mockmint/internal/protocol/amqp"
)

func newApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Default()
	cfg.Packages.Paths = []string{"../../examples/notebook"}
	a, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a
}

func pingZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("openapi.yaml")
	_, _ = w.Write([]byte("openapi: 3.0.3\ninfo: {title: ping, version: '1'}\npaths:\n  /ping:\n    get:\n      responses:\n        '200': {description: ok, content: {text/plain: {example: pong}}}\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestConcurrentServingAndReloads runs traffic against the mock server while
// packages are reloaded, uploaded and deleted, and while the admin-side
// readers (traffic, metrics, state) run. Under -race this audits the
// locking; functionally, no request may fail or hit a half-built router.
func TestConcurrentServingAndReloads(t *testing.T) {
	a := newApp(t)
	srv := httptest.NewServer(a.Server())
	defer srv.Close()
	client := srv.Client()
	ctx := context.Background()
	archive := pingZip(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var requests, failures atomic.Int64
	var firstFailure atomic.Value

	fail := func(format string, args ...any) {
		failures.Add(1)
		firstFailure.CompareAndSwap(nil, fmt.Sprintf(format, args...))
	}
	for w := range 8 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				var resp *http.Response
				var err error
				if i%3 == 0 {
					body := fmt.Sprintf(`{"title":"w%d-%d","content":"c","status":"draft"}`, w, i)
					resp, err = client.Post(srv.URL+"/notebook/1.0/notes", "application/json", strings.NewReader(body))
				} else {
					resp, err = client.Get(srv.URL + "/notebook/1.0/notes/1")
				}
				requests.Add(1)
				if err != nil {
					fail("request error: %v", err)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				// 503 is deleteNote's injected fault; not used here.
				if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
					fail("unexpected status %d", resp.StatusCode)
				}
			}
		})
	}
	wg.Go(func() { // reloads, uploads, deletes
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 3 {
			case 0:
				if err := a.Reload(ctx); err != nil {
					fail("reload: %v", err)
				}
			case 1:
				if _, _, err := a.Upload(ctx, "ping", "ping.zip", archive); err != nil {
					fail("upload: %v", err)
				}
			case 2:
				if err := a.Delete(ctx, "ping"); err != nil && !errors.Is(err, ErrNotFound) {
					fail("delete: %v", err)
				}
			}
		}
	})
	wg.Go(func() { // admin-side readers
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = a.Traffic.List(observability.Filter{Limit: 5})
			_ = a.Metrics.Registry.WriteText(io.Discard)
			_, _ = a.State.List(ctx, "notebook", "notes")
			_ = a.Packages()
			_, _ = a.Ready()
		}
	})

	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if n := failures.Load(); n > 0 {
		t.Fatalf("%d of %d requests failed; first: %v", n, requests.Load(), firstFailure.Load())
	}
	if requests.Load() < 100 {
		t.Fatalf("only %d requests ran", requests.Load())
	}
	if got := a.Metrics.Reloads.Value("error"); got != 0 {
		t.Fatalf("%d reloads failed", got)
	}
}

func TestUploadNameConflictsWithStatic(t *testing.T) {
	a := newApp(t)
	if _, _, err := a.Upload(context.Background(), "notebook", "x.zip", pingZip(t)); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := a.Upload(context.Background(), "ping", "x.zip", []byte("not a zip")); err == nil {
		t.Fatal("garbage archive accepted")
	}
	if len(a.Packages()) != 1 {
		t.Fatal("failed uploads changed the package set")
	}
}

func TestUploadPersistenceFailureLeavesPackageUnchanged(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Admin.DataDir = filepath.Join(t.TempDir(), "data")
	a, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	archive := pingZip(t)
	if _, _, err := a.Upload(ctx, "ping", "ping.zip", archive); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Admin.DataDir, "packages", "ping.zip")
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("persisted = %d bytes, %v", len(got), err)
	}
	if err := os.RemoveAll(filepath.Join(cfg.Admin.DataDir, "packages")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Admin.DataDir, "packages"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Upload(ctx, "ping", "ping.zip", archive); err == nil {
		t.Fatal("upload succeeded without persistence")
	}
	if len(a.Packages()) != 1 || a.Packages()[0].Name != "ping" {
		t.Fatalf("live packages = %+v", a.Packages())
	}
}

func TestDeleteReturnsPersistenceError(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Admin.DataDir = t.TempDir()
	a, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Upload(ctx, "ping", "ping.zip", pingZip(t)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Admin.DataDir, "packages", "ping.zip")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, "ping"); err == nil {
		t.Fatal("delete ignored file removal error")
	}
	if len(a.Packages()) != 1 || a.Packages()[0].Name != "ping" {
		t.Fatalf("failed delete changed live packages: %+v", a.Packages())
	}
}

func TestUploadReloadFailureKeepsPersistedArchive(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Admin.DataDir = t.TempDir()
	a, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	archive := pingZip(t)
	if _, _, err := a.Upload(ctx, "ping", "ping.zip", archive); err != nil {
		t.Fatal(err)
	}
	a.cfg.Packages.Paths = []string{filepath.Join(t.TempDir(), "missing")}
	if _, _, err := a.Upload(ctx, "ping", "ping.zip", archive); err == nil {
		t.Fatal("reload succeeded with missing configured package")
	}
	dir := filepath.Join(cfg.Admin.DataDir, "packages")
	got, err := os.ReadFile(filepath.Join(dir, "ping.zip"))
	if err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("old archive changed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staged upload left behind: %v, %v", entries, err)
	}
}

func TestPublishUsesOneReloadGeneration(t *testing.T) {
	load := func(keep string) (*pkg.Package, string) {
		t.Helper()
		ps, err := pkg.LoadPath(context.Background(), "../../examples/orders-events", pkg.Defaults{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := ps[0]
		for _, op := range p.Async.Operations {
			if op.Templates == nil {
				continue
			}
			op.Schedule = nil
			op.Templates = map[string]*pkg.MessageTemplate{keep: op.Templates[keep]}
			return p, op.Channel.ID
		}
		t.Fatal("no send operation")
		return nil, ""
	}
	p1, channel := load("widget")
	p2, _ := load("gadget")
	logger := slog.New(slog.DiscardHandler)
	e1, err := mamqp.New([]*pkg.Package{p1}, mamqp.Options{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := mamqp.New([]*pkg.Package{p2}, mamqp.Options{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: config.Default(), engine: e1}
	a.cfg.AMQP.URL = "amqp://unconnected"
	a.pkgs.Store(&[]*pkg.Package{p1})
	var failed atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 10000; i++ {
			a.mu.Lock()
			if i%2 == 0 {
				a.engine = e2
				a.pkgs.Store(&[]*pkg.Package{p2})
			} else {
				a.engine = e1
				a.pkgs.Store(&[]*pkg.Package{p1})
			}
			a.mu.Unlock()
		}
	})
	wg.Go(func() {
		for i := 0; i < 10000; i++ {
			_, _, err := a.Publish(context.Background(), p1.Name, channel, "", "")
			if !errors.Is(err, mamqp.ErrNotConnected) {
				failed.Store(true)
				return
			}
		}
	})
	wg.Wait()
	if failed.Load() {
		t.Fatal("publish selected an operation from a different engine generation")
	}
}
