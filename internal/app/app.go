// Package app owns mockmint's live package set: loading, atomic hot reload,
// uploads, and the protocol engines serving the packages.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mockmint/mockmint/internal/config"
	"github.com/mockmint/mockmint/internal/observability"
	"github.com/mockmint/mockmint/internal/pkg"
	mamqp "github.com/mockmint/mockmint/internal/protocol/amqp"
	mhttp "github.com/mockmint/mockmint/internal/protocol/http"
	"github.com/mockmint/mockmint/internal/spec/asyncapi"
	"github.com/mockmint/mockmint/internal/state"
)

// Errors the admin API maps to status codes.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// LoadError is a failed load or reload; nothing live changed.
type LoadError struct{ Err error }

func (e *LoadError) Error() string { return e.Err.Error() }
func (e *LoadError) Unwrap() error { return e.Err }

// App is the running mockmint instance.
type App struct {
	cfg config.Config
	log *slog.Logger

	State    state.Store
	Traffic  *observability.Traffic
	Metrics  *observability.Metrics
	Recorder *mhttp.Recorder
	server   *mhttp.Server

	mu       sync.Mutex // serializes reloads, uploads and deletes
	uploads  map[string]upload
	pkgs     atomic.Pointer[[]*pkg.Package]
	origin   map[string]string // package name → static|uploaded
	engine   *mamqp.Engine
	stopAMQP func()
	loaded   atomic.Bool
}

type upload struct {
	filename string // decides the archive format
	data     []byte
}

// Origins of a package.
const (
	OriginStatic   = "static"   // packages.paths
	OriginUploaded = "uploaded" // admin API
)

// New creates an app with no packages; call Reload to load them.
func New(cfg config.Config, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &App{
		cfg:      cfg,
		log:      log,
		State:    state.NewMemory(),
		Traffic:  observability.NewTraffic(cfg.Admin.TrafficSize, cfg.Admin.RedactHeaders...),
		Metrics:  observability.NewMetrics(),
		Recorder: mhttp.NewRecorder(cfg.Admin.RecordLimit),
		uploads:  map[string]upload{},
		origin:   map[string]string{},
	}
	empty, err := mhttp.NewRouter(nil, a.routerOptions())
	if err != nil {
		return nil, err
	}
	a.server = mhttp.NewServer(empty, mhttp.Options{
		Addr:              cfg.HTTP.Addr,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout.D(),
		IdleTimeout:       cfg.HTTP.IdleTimeout.D(),
	}, log)
	a.pkgs.Store(&[]*pkg.Package{})
	if err := a.restoreUploads(); err != nil {
		return nil, err
	}
	return a, nil
}

// Server is the mock traffic server.
func (a *App) Server() *mhttp.Server { return a.server }

// Packages returns the live packages.
func (a *App) Packages() []*pkg.Package { return *a.pkgs.Load() }

// Origin reports where a live package came from.
func (a *App) Origin(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.origin[name]
}

// Engine returns the running AMQP engine, or nil.
func (a *App) Engine() *mamqp.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engine
}

func (a *App) routerOptions() mhttp.RouterOptions {
	return mhttp.RouterOptions{
		MaxBodyBytes: a.cfg.HTTP.MaxBodyBytes, Log: a.log,
		State: a.State, Traffic: a.Traffic, Metrics: a.Metrics, Recorder: a.Recorder,
	}
}

// Reload reloads every package (configured paths and uploads) and swaps
// them in atomically. On error nothing changes.
func (a *App) Reload(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.apply(ctx, a.uploads)
}

// apply loads a candidate package set and, if all of it is valid, makes it
// live. a.mu must be held.
func (a *App) apply(ctx context.Context, uploads map[string]upload) (err error) {
	start := time.Now()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
			err = &LoadError{Err: err}
		}
		a.Metrics.Reloads.Inc(result)
	}()

	d := pkg.Defaults{Validation: a.cfg.Defaults.Validation, Seed: a.cfg.Defaults.Seed}
	var pkgs []*pkg.Package
	origin := map[string]string{}
	var errs []error
	for _, path := range a.cfg.Packages.Paths {
		loaded, err := pkg.LoadPath(ctx, path, d, a.log)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range loaded {
			origin[p.Name] = OriginStatic
		}
		pkgs = append(pkgs, loaded...)
	}
	for _, name := range sortedNames(uploads) {
		u := uploads[name]
		p, err := loadUpload(ctx, name, u, d, a.log)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if origin[p.Name] == OriginStatic {
			errs = append(errs, fmt.Errorf("uploaded package %q has the name of a package in packages.paths", name))
			continue
		}
		origin[p.Name] = OriginUploaded
		pkgs = append(pkgs, p)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := uniqueNames(pkgs); err != nil {
		return err
	}
	// Build everything before touching anything live.
	engine, err := mamqp.New(pkgs, a.amqpOptions(), a.log)
	if err != nil {
		return err
	}
	rt, err := mhttp.NewRouter(pkgs, a.routerOptions())
	if err != nil {
		return err
	}

	a.server.Swap(rt)
	a.pkgs.Store(&pkgs)
	a.origin = origin
	a.uploads = uploads
	a.swapEngine(engine)
	a.loaded.Store(true)
	a.Metrics.Packages.Set(float64(len(pkgs)))
	a.log.Info("packages loaded", "packages", len(pkgs), "took", time.Since(start).String())
	return nil
}

// swapEngine stops the running engine (its in-flight messages are acked or
// requeued) and starts the new one, so no message is handled by two
// configurations. a.mu must be held.
func (a *App) swapEngine(next *mamqp.Engine) {
	if a.stopAMQP != nil {
		a.stopAMQP()
		a.stopAMQP, a.engine = nil, nil
	}
	a.engine = next
	if a.cfg.AMQP.URL == "" || !next.HasOperations() {
		if next.HasOperations() {
			a.log.Warn("packages have AsyncAPI operations but amqp.url is not set; serving HTTP only")
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		next.Run(ctx)
	}()
	a.stopAMQP = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(a.cfg.HTTP.ShutdownTimeout.D()):
			a.log.Error("previous RabbitMQ engine did not stop in time")
		}
	}
}

func (a *App) amqpOptions() mamqp.Options {
	c := a.cfg.AMQP
	return mamqp.Options{
		URL: c.URL, Prefetch: c.Prefetch, Heartbeat: c.Heartbeat.D(),
		ReconnectMin: c.ReconnectMin.D(), ReconnectMax: c.ReconnectMax.D(), ConfirmTimeout: c.ConfirmTimeout.D(),
		Traffic: a.Traffic, Metrics: a.Metrics,
	}
}

// Close stops the AMQP engine.
func (a *App) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopAMQP != nil {
		a.stopAMQP()
		a.stopAMQP = nil
	}
}

// Upload adds or replaces the uploaded package name from an archive
// (filename decides the format) and reloads. created reports a new package.
func (a *App) Upload(ctx context.Context, name, filename string, data []byte) (p *pkg.Package, created bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.origin[name] == OriginStatic {
		return nil, false, fmt.Errorf("%w: package %q comes from packages.paths and cannot be replaced through the API", ErrConflict, name)
	}
	u := upload{filename: filename, data: data}
	// Check the archive on its own first, for a precise error.
	d := pkg.Defaults{Validation: a.cfg.Defaults.Validation, Seed: a.cfg.Defaults.Seed}
	if _, err := loadUpload(ctx, name, u, d, nil); err != nil {
		return nil, false, &LoadError{Err: err}
	}
	next := make(map[string]upload, len(a.uploads)+1)
	for k, v := range a.uploads {
		next[k] = v
	}
	_, existed := next[name]
	next[name] = u
	tmp, err := a.stageUpload(u)
	if err != nil {
		return nil, false, err
	}
	if tmp != "" {
		defer os.Remove(tmp)
	}
	previous := a.uploads
	if err := a.apply(ctx, next); err != nil {
		return nil, false, err
	}
	if tmp != "" {
		path := filepath.Join(a.uploadDir(), name+archiveExt(u.filename))
		if err := os.Rename(tmp, path); err != nil {
			_ = a.apply(ctx, previous)
			return nil, false, err
		}
		if existed && archiveExt(previous[name].filename) != archiveExt(u.filename) {
			old := filepath.Join(a.uploadDir(), name+archiveExt(previous[name].filename))
			if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(path)
				_ = a.apply(ctx, previous)
				return nil, false, err
			}
		}
	}
	for _, lp := range a.Packages() {
		if lp.Name == name {
			p = lp
		}
	}
	return p, !existed, nil
}

// Delete removes the uploaded package name and reloads.
func (a *App) Delete(ctx context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.origin[name] == OriginStatic {
		return fmt.Errorf("%w: package %q comes from packages.paths and cannot be deleted through the API", ErrConflict, name)
	}
	if _, ok := a.uploads[name]; !ok {
		return fmt.Errorf("%w: no uploaded package %q", ErrNotFound, name)
	}
	next := make(map[string]upload, len(a.uploads))
	for k, v := range a.uploads {
		if k != name {
			next[k] = v
		}
	}
	previous := a.uploads
	if err := a.apply(ctx, next); err != nil {
		return err
	}
	if err := a.unpersist(name, previous[name]); err != nil {
		_ = a.apply(ctx, previous)
		return err
	}
	return nil
}

// Publish triggers a send operation on channel (id or address) of package
// pkgName. operation disambiguates channels with several send operations;
// example "" picks the schedule's first example, else the first by name.
func (a *App) Publish(ctx context.Context, pkgName, channel, operation, example string) (string, string, error) {
	a.mu.Lock()
	engine := a.engine
	var target *pkg.AsyncOperation
	var candidates []string
	for _, p := range a.Packages() {
		if p.Name != pkgName || p.Async == nil {
			continue
		}
		for _, op := range p.Async.Operations {
			if op.Action != asyncapi.ActionSend || (op.Channel.ID != channel && op.Channel.Address != channel) {
				continue
			}
			if operation == "" || op.ID == operation {
				target = op
				candidates = append(candidates, op.ID)
			}
		}
	}
	a.mu.Unlock()
	switch {
	case len(candidates) == 0:
		return "", "", fmt.Errorf("%w: package %q has no send operation on channel %q", ErrNotFound, pkgName, channel)
	case len(candidates) > 1:
		return "", "", fmt.Errorf("%w: channel %q has several send operations (%s); pass operation", ErrConflict, channel, strings.Join(candidates, ", "))
	}
	if example == "" {
		if target.Schedule != nil {
			example = target.Schedule.Examples[0]
		} else if names := pkg.ExampleNames(target.Templates); len(names) > 0 {
			example = names[0]
		}
	}
	if _, ok := target.Templates[example]; !ok {
		return target.ID, example, fmt.Errorf("%w: operation %q has no example %q", ErrNotFound, target.ID, example)
	}
	if engine == nil || a.cfg.AMQP.URL == "" {
		return target.ID, example, mamqp.ErrNotConnected
	}
	return target.ID, example, engine.Publish(ctx, pkgName, target.ID, example)
}

// Ready reports readiness and, when not ready, why.
func (a *App) Ready() (bool, string) {
	if !a.loaded.Load() {
		return false, "packages not loaded yet"
	}
	e := a.Engine()
	if a.cfg.AMQP.URL != "" && e != nil && e.HasOperations() && !e.Connected() {
		return false, "not connected to RabbitMQ"
	}
	return true, ""
}

func loadUpload(ctx context.Context, name string, u upload, d pkg.Defaults, log *slog.Logger) (*pkg.Package, error) {
	fsys, err := pkg.OpenArchive(u.filename, u.data)
	if err != nil {
		return nil, err
	}
	p, err := pkg.Load(ctx, fsys, "upload:"+name, d, log)
	if err != nil {
		return nil, err
	}
	if p.Name != name {
		return nil, fmt.Errorf("uploaded archive is package %q, not %q (set name in mockmint.yaml)", p.Name, name)
	}
	return p, nil
}

func uniqueNames(pkgs []*pkg.Package) error {
	seen := map[string]string{}
	for _, p := range pkgs {
		if prev, dup := seen[p.Name]; dup {
			return fmt.Errorf("package name %q is used by both %s and %s", p.Name, prev, p.Source)
		}
		seen[p.Name] = p.Source
	}
	return nil
}

func sortedNames(m map[string]upload) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// --- persistence ---------------------------------------------------------------

func (a *App) uploadDir() string {
	if a.cfg.Admin.DataDir == "" {
		return ""
	}
	return filepath.Join(a.cfg.Admin.DataDir, "packages")
}

// stageUpload writes and syncs an archive before making it live.
func (a *App) stageUpload(u upload) (string, error) {
	dir := a.uploadDir()
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(u.data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	complete = true
	return tmp.Name(), nil
}

func (a *App) unpersist(name string, u upload) error {
	dir := a.uploadDir()
	if dir == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(dir, name+archiveExt(u.filename))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// restoreUploads reads packages persisted by an earlier run. They are
// loaded by the first Reload.
func (a *App) restoreUploads() error {
	dir := a.uploadDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !pkg.IsArchive(n) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // G304: files in the operator's dataDir
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimSuffix(n, ".zip"), ".tar.gz")
		a.uploads[name] = upload{filename: n, data: data}
	}
	return nil
}

func archiveExt(filename string) string {
	if strings.HasSuffix(strings.ToLower(filename), ".zip") {
		return ".zip"
	}
	return ".tar.gz"
}
