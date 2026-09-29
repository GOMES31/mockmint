// Package admin implements mockmint's admin API: health, readiness,
// Prometheus metrics, package management, traffic, state, recordings and
// manual RabbitMQ publishes. See docs/admin-openapi.yaml.
package admin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/mockmint/mockmint/internal/app"
	"github.com/mockmint/mockmint/internal/observability"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/problem"
	mamqp "github.com/mockmint/mockmint/internal/protocol/amqp"
	"github.com/mockmint/mockmint/internal/state"
)

// Options configure the admin handler.
type Options struct {
	Token          string // "" disables auth
	MaxUploadBytes int64
	Version        string
	Log            *slog.Logger
}

type handler struct {
	app       *app.App
	opts      Options
	tokenHash [32]byte
	mux       *http.ServeMux
}

// New returns the admin API handler.
func New(a *app.App, o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	h := &handler{app: a, opts: o, tokenHash: sha256.Sum256([]byte(o.Token)), mux: http.NewServeMux()}

	h.route("/healthz", false, methods{http.MethodGet: h.healthz})
	h.route("/readyz", false, methods{http.MethodGet: h.readyz})
	h.route("/metrics", false, methods{http.MethodGet: h.metrics})
	h.route("/admin/info", true, methods{http.MethodGet: h.info})
	h.route("/admin/packages", true, methods{http.MethodGet: h.listPackages})
	h.route("/admin/packages/{name}", true, methods{
		http.MethodGet: h.getPackage, http.MethodPut: h.putPackage, http.MethodDelete: h.deletePackage,
	})
	h.route("/admin/reload", true, methods{http.MethodPost: h.reload})
	h.route("/admin/traffic", true, methods{http.MethodGet: h.traffic, http.MethodDelete: h.clearTraffic})
	h.route("/admin/state/{package}", true, methods{http.MethodGet: h.getState, http.MethodDelete: h.clearState})
	h.route("/admin/recordings/{package}", true, methods{http.MethodGet: h.recordings, http.MethodDelete: h.clearRecordings})
	h.route("/admin/async/{package}/{channel}/publish", true, methods{http.MethodPost: h.publish})
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, problem.New(problem.TypeNotFound, http.StatusNotFound, "no admin endpoint "+r.URL.Path))
	})
	return h.mux
}

type methods map[string]http.HandlerFunc

// route registers pattern for several methods, answering others with a
// 405 problem, and optionally requiring the bearer token.
func (h *handler) route(pattern string, auth bool, ms methods) {
	allow := make([]string, 0, len(ms))
	for m := range ms {
		allow = append(allow, m)
	}
	slices.Sort(allow)
	h.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if auth && !h.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mockmint"`)
			problem.Write(w, r, problem.New(problem.TypeUnauthorized, http.StatusUnauthorized, "a valid bearer token is required"))
			return
		}
		fn, ok := ms[r.Method]
		if !ok && r.Method == http.MethodHead {
			fn, ok = ms[http.MethodGet]
		}
		if !ok {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			problem.Write(w, r, problem.New(problem.TypeMethodNotAllowed, http.StatusMethodNotAllowed, r.Method+" is not allowed on "+pattern))
			return
		}
		fn(w, r)
	})
}

// authorized compares SHA-256 digests in constant time, so neither the
// token's content nor its length leaks through timing.
func (h *handler) authorized(r *http.Request) bool {
	if h.opts.Token == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(got)))
	return subtle.ConstantTimeCompare(sum[:], h.tokenHash[:]) == 1
}

// --- probes and metrics --------------------------------------------------------

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) readyz(w http.ResponseWriter, _ *http.Request) {
	ready, reason := h.app.Ready()
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *handler) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", observability.ContentType)
	_ = h.app.Metrics.Registry.WriteText(w)
}

func (h *handler) info(w http.ResponseWriter, _ *http.Request) {
	ready, reason := h.app.Ready()
	amqp := "disabled"
	if e := h.app.Engine(); e != nil && e.HasOperations() {
		amqp = "disconnected"
		if e.Connected() {
			amqp = "connected"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": h.opts.Version, "ready": ready, "reason": reason,
		"packages": len(h.app.Packages()), "amqp": amqp,
	})
}

// --- packages --------------------------------------------------------------------

func (h *handler) listPackages(w http.ResponseWriter, _ *http.Request) {
	pkgs := h.app.Packages()
	out := make([]packageSummary, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, summarize(p, h.app.Origin(p.Name)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) getPackage(w http.ResponseWriter, r *http.Request) {
	p := h.find(r.PathValue("name"))
	if p == nil {
		problem.Write(w, r, problem.New(problem.TypeNotFound, http.StatusNotFound, "no package "+strconv.Quote(r.PathValue("name"))))
		return
	}
	writeJSON(w, http.StatusOK, describe(p, h.app.Origin(p.Name)))
}

// archiveTypes maps upload media types to the archive name used to decide
// the format.
var archiveTypes = map[string]string{
	"application/zip":              "upload.zip",
	"application/x-zip-compressed": "upload.zip",
	"application/gzip":             "upload.tar.gz",
	"application/x-gzip":           "upload.tar.gz",
	"application/x-tar+gzip":       "upload.tar.gz",
	"application/x-compressed-tar": "upload.tar.gz",
}

func (h *handler) putPackage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	filename, ok := archiveTypes[mt]
	if !ok {
		problem.Write(w, r, problem.New(problem.TypeUnsupportedMedia, http.StatusUnsupportedMediaType,
			"upload a .zip (application/zip) or .tar.gz (application/gzip) package archive"))
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.opts.MaxUploadBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			problem.Write(w, r, problem.New(problem.TypeBodyTooLarge, http.StatusRequestEntityTooLarge,
				"archive exceeds "+strconv.FormatInt(tooBig.Limit, 10)+" bytes"))
		}
		return
	}
	p, created, err := h.app.Upload(r.Context(), name, filename, data)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, describe(p, app.OriginUploaded))
}

func (h *handler) deletePackage(w http.ResponseWriter, r *http.Request) {
	if err := h.app.Delete(r.Context(), r.PathValue("name")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) reload(w http.ResponseWriter, r *http.Request) {
	if err := h.app.Reload(r.Context()); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.listPackages(w, r)
}

// --- traffic, state, recordings --------------------------------------------------

func (h *handler) traffic(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if q.Get("limit") != "" && (err != nil || limit < 0) {
		problem.Write(w, r, problem.New(problem.TypeValidation, http.StatusBadRequest, "limit must be a non-negative integer"))
		return
	}
	writeJSON(w, http.StatusOK, h.app.Traffic.List(observability.Filter{Package: q.Get("package"), Protocol: q.Get("protocol"), Limit: limit}))
}

func (h *handler) clearTraffic(w http.ResponseWriter, _ *http.Request) {
	h.app.Traffic.Clear()
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) getState(w http.ResponseWriter, r *http.Request) {
	entries, err := h.app.State.List(r.Context(), r.PathValue("package"), r.URL.Query().Get("collection"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if entries == nil {
		entries = []state.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// clearState empties a package's state; the package's initialState is
// seeded again on the next reload.
func (h *handler) clearState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("package")
	if err := h.app.State.Clear(r.Context(), name); err != nil {
		h.writeError(w, r, err)
		return
	}
	if p := h.find(name); p != nil {
		if err := p.SeedState(r.Context(), h.app.State); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// recordings exports proxied exchanges as mockmint example files: YAML
// documents (one per operation) by default, or JSON with ?format=json.
func (h *handler) recordings(w http.ResponseWriter, r *http.Request) {
	files := h.app.Recorder.ExampleFiles(r.PathValue("package"))
	ops := make([]string, 0, len(files))
	for op := range files {
		ops = append(ops, op)
	}
	slices.Sort(ops)
	if r.URL.Query().Get("format") == "json" {
		list := make([]any, 0, len(ops))
		for _, op := range ops {
			list = append(list, files[op])
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	for _, op := range ops {
		_ = enc.Encode(files[op])
	}
	_ = enc.Close()
}

func (h *handler) clearRecordings(w http.ResponseWriter, r *http.Request) {
	h.app.Recorder.Clear(r.PathValue("package"))
	w.WriteHeader(http.StatusNoContent)
}

// --- async -----------------------------------------------------------------------

type publishRequest struct {
	Example   string `json:"example"`
	Operation string `json:"operation"`
}

func (h *handler) publish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10)); len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			problem.Write(w, r, problem.New(problem.TypeValidation, http.StatusBadRequest, "body must be JSON: "+err.Error()))
			return
		}
	}
	op, example, err := h.app.Publish(r.Context(), r.PathValue("package"), r.PathValue("channel"), req.Operation, req.Example)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"operation": op, "example": example, "status": "published"})
}

// --- helpers -----------------------------------------------------------------------

func (h *handler) find(name string) *pkg.Package {
	for _, p := range h.app.Packages() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var le *app.LoadError
	switch {
	case errors.Is(err, app.ErrNotFound):
		problem.Write(w, r, problem.New(problem.TypeNotFound, http.StatusNotFound, err.Error()))
	case errors.Is(err, app.ErrConflict):
		problem.Write(w, r, problem.New(problem.TypeConflict, http.StatusConflict, err.Error()))
	case errors.Is(err, mamqp.ErrNotConnected):
		problem.Write(w, r, problem.New(problem.TypeBadGateway, http.StatusServiceUnavailable, err.Error()))
	case errors.As(err, &le):
		problem.Write(w, r, problem.New(problem.TypeInvalidPackage, http.StatusUnprocessableEntity,
			"nothing was changed: "+le.Error()).With("errors", splitErrors(le.Err)))
	default:
		h.opts.Log.Error("admin request failed", "path", r.URL.Path, "error", err)
		problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError, err.Error()))
	}
}

// splitErrors lists joined errors individually.
func splitErrors(err error) []string {
	var multi interface{ Unwrap() []error }
	if errors.As(err, &multi) {
		var out []string
		for _, e := range multi.Unwrap() {
			out = append(out, splitErrors(e)...)
		}
		return out
	}
	return []string{err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
