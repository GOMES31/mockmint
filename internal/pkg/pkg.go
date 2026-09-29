package pkg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mockmint/mockmint/internal/behavior"
	"github.com/mockmint/mockmint/internal/dispatch"
	"github.com/mockmint/mockmint/internal/spec/asyncapi"
	"github.com/mockmint/mockmint/internal/spec/openapi"
	"github.com/mockmint/mockmint/internal/template"
)

// Defaults are server-level settings a manifest can override.
type Defaults struct {
	Validation string
	Seed       int64
}

// Package is a loaded, fully compiled mock package. It is immutable after
// Load except for dispatcher and rate-limiter state.
type Package struct {
	Name       string
	Version    string
	BasePath   string // "" when mounted unprefixed, else "/segment[/...]"
	Source     string
	Seed       int64
	Clock      *time.Time
	Spec       *openapi.Spec // nil for AsyncAPI-only packages
	Operations []*Operation

	Proxy *Proxy // nil: no upstream
	// InitialState is applied with SeedState: collection → key → value.
	InitialState map[string]map[string]any

	AsyncSpec *asyncapi.Spec // nil for OpenAPI-only packages
	Async     *Async
}

// Operation is an OpenAPI operation with everything needed to serve it.
type Operation struct {
	*openapi.Operation
	Validation string
	Dispatcher dispatch.Dispatcher
	Behavior   *behavior.Behavior
	Responses  map[string]*Response // by example name
	Fallback   *Response            // nil: 404 problem
	State      *StateOp             // nil: stateless
}

// Response is a compiled example response. BodyTemplate is nil when the
// body is static; HeaderTemplates lists templated headers sorted by name,
// so renders consume the request RNG in a fixed order.
type Response struct {
	Example         string
	Status          int
	MediaType       string
	Headers         map[string]string // static headers (templated ones excluded)
	HeaderTemplates []HeaderTemplate
	Body            []byte
	BodyTemplate    *template.Template
}

// HeaderTemplate is a templated response header.
type HeaderTemplate struct {
	Name     string
	Template *template.Template
}

// Now returns the package clock: frozen when configured, else wall time.
func (p *Package) Now() time.Time {
	if p.Clock != nil {
		return *p.Clock
	}
	return time.Now()
}

// LoadPath loads the package(s) at path: a package directory, an archive,
// or a directory whose entries are package directories and archives. A
// directory is a package when it contains mockmint.yaml or an OpenAPI
// document at its top level.
func LoadPath(ctx context.Context, p string, d Defaults, log *slog.Logger) ([]*Package, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if !IsArchive(p) {
			return nil, fmt.Errorf("%s: not a directory or supported archive", p)
		}
		pk, err := loadArchiveFile(ctx, p, d, log)
		if err != nil {
			return nil, err
		}
		return []*Package{pk}, nil
	}
	if pk, ok, err := loadDir(ctx, p, d, log); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return []*Package{pk}, nil
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	var pkgs []*Package
	var errs []error
	for _, e := range entries {
		full := filepath.Join(p, e.Name())
		switch {
		case e.IsDir():
			pk, ok, err := loadDir(ctx, full, d, log)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ok {
				pkgs = append(pkgs, pk)
			}
		case !e.IsDir() && IsArchive(e.Name()):
			pk, err := loadArchiveFile(ctx, full, d, log)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			pkgs = append(pkgs, pk)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("%s: no mock packages found", p)
	}
	return pkgs, nil
}

// loadDir loads dir as a package if it is one (ok reports whether it is).
// Reads go through os.Root, so symlinks cannot reach outside the package.
func loadDir(ctx context.Context, dir string, d Defaults, log *slog.Logger) (pk *Package, ok bool, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	fsys := root.FS()
	if !isPackageDir(fsys) {
		return nil, false, nil
	}
	pk, err = Load(ctx, fsys, dir, d, log)
	return pk, true, err
}

func loadArchiveFile(ctx context.Context, p string, d Defaults, log *slog.Logger) (*Package, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxArchiveBytes {
		return nil, fmt.Errorf("%s: archive larger than %d bytes", p, MaxArchiveBytes)
	}
	data, err := os.ReadFile(p) //nolint:gosec // G304: the operator chooses package paths
	if err != nil {
		return nil, err
	}
	fsys, err := OpenArchive(filepath.Base(p), data)
	if err != nil {
		return nil, err
	}
	return Load(ctx, fsys, p, d, log)
}

func isPackageDir(fsys fs.FS) bool {
	if _, err := fs.Stat(fsys, ManifestFile); err == nil {
		return true
	}
	found, err := findDocs(fsys)
	return err == nil && (len(found.openapi) > 0 || len(found.asyncapi) > 0)
}

type foundDocs struct{ openapi, asyncapi []string }

// findDocs lists the top-level OpenAPI and AsyncAPI documents in fsys.
func findDocs(fsys fs.FS) (foundDocs, error) {
	var f foundDocs
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return f, err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == ManifestFile || !isSpecExt(e.Name()) {
			continue
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			continue
		}
		switch {
		case openapi.IsDocument(b):
			f.openapi = append(f.openapi, e.Name())
		case asyncapi.IsDocument(b):
			f.asyncapi = append(f.asyncapi, e.Name())
		}
	}
	return f, nil
}

// pickDoc chooses the document of one kind: the manifest's choice, else the
// single one found. "" means the package has none of that kind.
func pickDoc(explicit string, found []string, kind, field string) (string, error) {
	switch {
	case explicit != "":
		return explicit, nil
	case len(found) <= 1:
		return strings.Join(found, ""), nil
	default:
		return "", fmt.Errorf("several %s documents found (%s); set %s in mockmint.yaml", kind, strings.Join(found, ", "), field)
	}
}

func isSpecExt(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// Load loads one package rooted at fsys. source names it in errors and logs.
func Load(ctx context.Context, fsys fs.FS, source string, d Defaults, log *slog.Logger) (*Package, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pk, err := load(ctx, fsys, source, d, log)
	if err != nil {
		return nil, fmt.Errorf("package %s: %w", source, err)
	}
	return pk, nil
}

func load(ctx context.Context, fsys fs.FS, source string, d Defaults, log *slog.Logger) (*Package, error) {
	var m Manifest
	if b, err := fs.ReadFile(fsys, ManifestFile); err == nil {
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", ManifestFile, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	found, err := findDocs(fsys)
	if err != nil {
		return nil, err
	}
	specName, err := pickDoc(m.Spec, found.openapi, "OpenAPI", "spec")
	if err != nil {
		return nil, err
	}
	asyncName, err := pickDoc(m.AsyncAPI, found.asyncapi, "AsyncAPI", "asyncapi")
	if err != nil {
		return nil, err
	}
	if specName == "" && asyncName == "" {
		return nil, errors.New("no OpenAPI or AsyncAPI document found (set spec or asyncapi in mockmint.yaml)")
	}

	pk := &Package{Source: source, Clock: m.Clock, Seed: d.Seed}
	var title, version string
	if specName != "" {
		if pk.Spec, err = openapi.Load(ctx, fsys, specName); err != nil {
			return nil, err
		}
		title, version = pk.Spec.Title, pk.Spec.Version
	}
	if asyncName != "" {
		if pk.AsyncSpec, err = asyncapi.Load(fsys, asyncName); err != nil {
			return nil, err
		}
		title, version = cmpOr(title, pk.AsyncSpec.Title), cmpOr(version, pk.AsyncSpec.Version)
	} else if m.Async != nil {
		return nil, errors.New("async is configured but the package has no AsyncAPI document")
	}
	if m.Seed != nil {
		pk.Seed = *m.Seed
	}
	pk.Name = cmpOr(m.Name, slug(title))
	pk.Version = cmpOr(m.Version, version)
	if pk.Name == "" {
		return nil, errors.New("package name is empty: set name in mockmint.yaml or info.title in the spec")
	}
	if !validSegment(pk.Name) || (pk.Version != "" && !validSegment(pk.Version)) {
		return nil, fmt.Errorf("name %q and version %q must be URL path segments ([A-Za-z0-9._~-])", pk.Name, pk.Version)
	}
	if pk.BasePath, err = basePath(m.BasePath, pk.Name, pk.Version); err != nil {
		return nil, err
	}
	validation := cmpOr(m.Validation, d.Validation, ValidationWarn)
	if err := checkValidation(validation); err != nil {
		return nil, err
	}
	templating := cmpOr(m.Templating, TemplatingAuto)
	switch templating {
	case TemplatingAuto, TemplatingOn, TemplatingOff:
	default:
		return nil, fmt.Errorf("templating %q: want auto, on or off", templating)
	}

	if pk.AsyncSpec != nil {
		for _, w := range pk.AsyncSpec.Warnings {
			log.Warn("asyncapi warning", "package", pk.Name, "warning", w)
		}
		if pk.Async, err = compileAsync(pk, m.Async, validation, templating); err != nil {
			return nil, fmt.Errorf("async: %w", err)
		}
	}
	if pk.Spec == nil {
		if m.Proxy != nil {
			return nil, errors.New("proxy needs an OpenAPI document")
		}
		if len(m.Operations) > 0 {
			return nil, errors.New("operations configure HTTP operations but the package has no OpenAPI document (use async.operations)")
		}
		log.Info("package loaded", "package", pk.Name, "version", pk.Version, "asyncOperations", len(pk.Async.Operations), "source", source)
		return pk, nil
	}

	if pk.Proxy, err = compileProxy(m.Proxy); err != nil {
		return nil, err
	}
	for col, entries := range m.InitialState {
		if pk.InitialState == nil {
			pk.InitialState = map[string]map[string]any{}
		}
		pk.InitialState[col] = map[string]any{}
		for k, v := range entries {
			pk.InitialState[col][k] = normalize(v)
		}
	}
	byKey := map[string]*openapi.Operation{}
	for _, o := range pk.Spec.Operations {
		byKey[o.ID] = o
		if o.OperationID != "" {
			byKey[o.OperationID] = o
		}
	}
	if err := loadExampleFiles(fsys, byKey); err != nil {
		return nil, err
	}
	for key := range m.Operations {
		if byKey[key] == nil {
			return nil, fmt.Errorf("operations: %q matches no operation (use \"METHOD /path\" or an operationId)", key)
		}
	}

	var errs []error
	for _, o := range pk.Spec.Operations {
		ov, err := overridesFor(m.Operations, o)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cop, err := compileOperation(pk, &m, o, ov, validation, templating)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.ID, err))
			continue
		}
		for _, w := range o.Warnings {
			log.Warn("example warning", "package", pk.Name, "operation", o.ID, "warning", w)
		}
		pk.Operations = append(pk.Operations, cop)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	attrs := []any{"package", pk.Name, "version", pk.Version, "basePath", cmpOr(pk.BasePath, "/"), "operations", len(pk.Operations)}
	if pk.Async != nil {
		attrs = append(attrs, "asyncOperations", len(pk.Async.Operations))
	}
	log.Info("package loaded", append(attrs, "source", source)...)
	return pk, nil
}

// overridesFor returns the manifest entry for o, keyed by "METHOD /path" or
// by operationId, and rejects configuring the same operation under both.
func overridesFor(all map[string]OperationOverrides, o *openapi.Operation) (OperationOverrides, error) {
	byPath, hasPath := all[o.ID]
	var byID OperationOverrides
	hasID := false
	if o.OperationID != "" {
		byID, hasID = all[o.OperationID]
	}
	switch {
	case hasPath && hasID:
		return OperationOverrides{}, fmt.Errorf("operations: %s is configured twice (as %q and %q)", o.ID, o.ID, o.OperationID)
	case hasID:
		return byID, nil
	default:
		return byPath, nil
	}
}

func compileOperation(pk *Package, m *Manifest, o *openapi.Operation, ov OperationOverrides, validation, templating string) (*Operation, error) {
	if err := o.Finalize(template.NewRand(cmpOr(pk.Seed, 1), []byte(o.ID))); err != nil {
		return nil, err
	}
	cop := &Operation{Operation: o, Validation: cmpOr(ov.Validation, validation), Responses: map[string]*Response{}}
	if err := checkValidation(cop.Validation); err != nil {
		return nil, err
	}
	var err error
	if cop.State, err = compileState(o.ID, ov.State); err != nil {
		return nil, err
	}

	bcfg := behavior.Merge(m.Behavior, ov.Behavior)
	for i, f := range bcfg.Faults {
		if f.Action == behavior.ActionDeadLetter {
			return nil, fmt.Errorf("behavior: faults[%d]: action deadletter is for async operations", i)
		}
	}
	if cop.Behavior, err = behavior.Compile(bcfg, nil); err != nil {
		return nil, fmt.Errorf("behavior: %w", err)
	}

	names := o.ExampleNames()
	cands := make([]dispatch.Candidate, 0, len(names))
	for _, n := range names {
		e := o.Examples[n]
		cands = append(cands, dispatch.Candidate{Name: n, PathParams: e.PathParams, Query: e.Query, Headers: e.Headers, Body: e.Body})
		r, err := compileResponse(o.ID, e.Name, e.Status, e.MediaType, e.ResponseHeaders, e.ResponseBody, templating)
		if err != nil {
			return nil, err
		}
		cop.Responses[n] = r
	}
	if cop.Dispatcher, err = dispatch.Build(ov.Dispatcher, dispatch.Inputs{Examples: names, DefaultExample: o.DefaultExample, Candidates: cands}); err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}

	fb, inherited := ov.Fallback, false
	if fb == nil {
		fb, inherited = m.Fallback, true
	}
	if fb != nil {
		if cop.Fallback, err = compileFallback(o, cop, fb, inherited, templating); err != nil {
			return nil, fmt.Errorf("fallback: %w", err)
		}
	}
	return cop, nil
}

// compileFallback compiles fb for one operation. An inherited (package-level)
// fallback may name an example that only some operations have; the others
// keep the default 404 problem. An operation's own fallback must name one of
// its examples.
func compileFallback(o *openapi.Operation, cop *Operation, fb *Fallback, inherited bool, templating string) (*Response, error) {
	if fb.Example != "" {
		if fb.Status != 0 || fb.Body.Set || fb.MediaType != "" || fb.Headers != nil {
			return nil, errors.New("set either example or an inline response, not both")
		}
		r, ok := cop.Responses[fb.Example]
		switch {
		case ok:
			return r, nil
		case inherited:
			return nil, nil
		default:
			return nil, fmt.Errorf("unknown example %q", fb.Example)
		}
	}
	status := fb.Status
	if status == 0 {
		status = http.StatusNotFound
	}
	if status < 100 || status > 599 {
		return nil, fmt.Errorf("invalid status %d", status)
	}
	mt := fb.MediaType
	if mt == "" && fb.Body.Set {
		mt = "application/json"
		if fb.Body.Value == nil {
			mt = "text/plain; charset=utf-8"
		}
	}
	return compileResponse(o.ID, "fallback", status, mt, fb.Headers, fb.Body.Raw, templating)
}

func compileResponse(opID, name string, status int, mt string, headers map[string]string, body []byte, templating string) (*Response, error) {
	r := &Response{Example: name, Status: status, MediaType: mt, Body: body}
	render := func(s string) bool {
		return templating == TemplatingOn || (templating == TemplatingAuto && template.IsTemplate(s))
	}
	if render(string(body)) {
		t, err := template.Parse(opID+" "+name, string(body))
		if err != nil {
			return nil, fmt.Errorf("example %q body template: %w", name, err)
		}
		r.BodyTemplate = t
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		v := headers[k]
		if !render(v) {
			if r.Headers == nil {
				r.Headers = map[string]string{}
			}
			r.Headers[k] = v
			continue
		}
		t, err := template.Parse(opID+" "+name+" "+k, v)
		if err != nil {
			return nil, fmt.Errorf("example %q header %s template: %w", name, k, err)
		}
		r.HeaderTemplates = append(r.HeaderTemplates, HeaderTemplate{Name: k, Template: t})
	}
	return r, nil
}

func loadExampleFiles(fsys fs.FS, byKey map[string]*openapi.Operation) error {
	entries, err := fs.ReadDir(fsys, "examples")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !isSpecExt(e.Name()) {
			continue
		}
		name := "examples/" + e.Name()
		if err := loadExampleFile(fsys, name, byKey); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func loadExampleFile(fsys fs.FS, name string, byKey map[string]*openapi.Operation) error {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	var f ExampleFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return err
	}
	o := byKey[f.Operation]
	if o == nil {
		return fmt.Errorf("operation %q not found", f.Operation)
	}
	names := make([]string, 0, len(f.Examples))
	for n := range f.Examples {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		fe := f.Examples[n]
		ex := &openapi.Example{
			Name:            n,
			Source:          name,
			PathParams:      fe.Request.Params,
			Query:           fe.Request.Query,
			Status:          fe.Response.Status,
			MediaType:       fe.Response.MediaType,
			ResponseHeaders: canonicalHeaders(fe.Response.Headers),
			ResponseBody:    fe.Response.Body.Raw,
		}
		ex.Headers = canonicalHeaders(fe.Request.Headers)
		if fe.Request.Body.Set {
			if fe.Request.Body.Value != nil {
				ex.Body = normalize(fe.Request.Body.Value)
			} else {
				var v any
				if json.Unmarshal(fe.Request.Body.Raw, &v) != nil {
					return fmt.Errorf("example %q: request.body must be JSON (structured YAML or a JSON string)", n)
				}
				ex.Body = v
			}
		}
		if err := o.AddExample(ex); err != nil {
			return err
		}
	}
	return nil
}

func canonicalHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[http.CanonicalHeaderKey(k)] = v
	}
	return out
}

func checkValidation(v string) error {
	switch v {
	case ValidationStrict, ValidationWarn, ValidationOff:
		return nil
	}
	return fmt.Errorf("validation %q: want strict, warn or off", v)
}

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

func validSegment(s string) bool { return segmentRE.MatchString(s) && s != "." && s != ".." }

func basePath(explicit, name, version string) (string, error) {
	if explicit == "" {
		if version == "" {
			return "/" + name, nil
		}
		return "/" + name + "/" + version, nil
	}
	if explicit == "/" {
		return "", nil
	}
	if !strings.HasPrefix(explicit, "/") {
		return "", fmt.Errorf("basePath %q must start with /", explicit)
	}
	clean := strings.TrimSuffix(explicit, "/")
	for seg := range strings.SplitSeq(clean[1:], "/") {
		if !validSegment(seg) {
			return "", fmt.Errorf("basePath %q: segment %q must match [A-Za-z0-9._~-]+", explicit, seg)
		}
	}
	return clean, nil
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// slug lowercases s and collapses runs of other characters into "-".
func slug(s string) string {
	return strings.Trim(slugRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func cmpOr[T comparable](vals ...T) T {
	var zero T
	for _, v := range vals {
		if v != zero {
			return v
		}
	}
	return zero
}

func marshalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(normalize(v))
	if err != nil {
		return nil, fmt.Errorf("body is not JSON-encodable: %w", err)
	}
	return b, nil
}

// normalize converts YAML-decoded values to JSON-compatible ones.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalize(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	default:
		return v
	}
}
