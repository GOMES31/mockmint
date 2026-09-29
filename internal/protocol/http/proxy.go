package http

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/problem"
)

// maxRecordedBody bounds a recorded upstream response body.
const maxRecordedBody = 1 << 20

// newProxy returns a reverse proxy to p.Proxy that strips the package base
// path.
func (rt *Router) newProxy(p *pkg.Package) *httputil.ReverseProxy {
	up := p.Proxy.URL
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			// SetURL joins the upstream path with the incoming one; drop the
			// mockmint base path in between.
			rest := strings.TrimPrefix(pr.In.URL.Path, p.BasePath)
			pr.Out.URL.Path = up.Path + rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = up.Host
			pr.SetXForwarded()
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: p.Proxy.Timeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   16,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rt.log.Warn("proxy failed", "package", p.Name, "upstream", up.Redacted(), "error", err)
			problem.Write(w, r, problem.New(problem.TypeBadGateway, http.StatusBadGateway, "upstream request failed"))
		},
	}
}

// forward proxies r upstream. body is the already-read request body; op
// (nil for unmatched routes) receives a recording of the exchange.
func (rt *Router) forward(w http.ResponseWriter, r *http.Request, p *pkg.Package, op *pkg.Operation, params map[string]string, body []byte) {
	markProxied(w)
	proxy := rt.proxies[p]
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	if op == nil || !p.Proxy.Record || rt.recorder == nil {
		proxy.ServeHTTP(w, r)
		return
	}
	cw := &captureWriter{ResponseWriter: w}
	proxy.ServeHTTP(cw, r)
	if cw.status == 0 || cw.status >= 500 || cw.truncated {
		return
	}
	rt.recorder.add(p.Name, Recording{
		Operation:   op.ID,
		PathParams:  params,
		Query:       firstQueryValues(r.URL.Query()),
		Headers:     declaredHeaders(op, r.Header),
		Body:        jsonOrNil(body),
		Status:      cw.status,
		ContentType: cw.Header().Get("Content-Type"),
		Response:    cw.body.Bytes(),
	})
}

// captureWriter tees the proxied response for recording.
type captureWriter struct {
	http.ResponseWriter
	status    int
	body      bytes.Buffer
	truncated bool
}

func (c *captureWriter) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
	c.ResponseWriter.WriteHeader(s)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len()+len(b) > maxRecordedBody {
		c.truncated = true
	} else {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Recording is one proxied exchange, convertible to a mockmint example.
type Recording struct {
	Operation   string
	PathParams  map[string]string
	Query       map[string]string
	Headers     map[string]string
	Body        any // decoded JSON request body, if any
	Status      int
	ContentType string
	Response    []byte
}

// Recorder keeps the latest recordings per package (bounded), across reloads.
type Recorder struct {
	mu    sync.Mutex
	limit int
	byPkg map[string][]Recording
}

// NewRecorder keeps up to limit recordings per package.
func NewRecorder(limit int) *Recorder {
	return &Recorder{limit: max(limit, 1), byPkg: map[string][]Recording{}}
}

func (r *Recorder) add(pkgName string, rec Recording) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byPkg[pkgName] = append(r.byPkg[pkgName], rec)
	if list := r.byPkg[pkgName]; len(list) > r.limit {
		r.byPkg[pkgName] = slices.Clone(list[len(list)-r.limit:])
	}
}

// Recordings returns a package's recordings, oldest first.
func (r *Recorder) Recordings(pkgName string) []Recording {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.byPkg[pkgName])
}

// Clear drops a package's recordings.
func (r *Recorder) Clear(pkgName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byPkg, pkgName)
}

// ExampleFiles converts a package's recordings into mockmint example files
// (examples/*.yaml content), one per operation, keyed by operation.
// Examples are named recorded-1, recorded-2, … in recording order.
func (r *Recorder) ExampleFiles(pkgName string) map[string]ExampleFile {
	out := map[string]ExampleFile{}
	for _, rec := range r.Recordings(pkgName) {
		f, ok := out[rec.Operation]
		if !ok {
			f = ExampleFile{Operation: rec.Operation, Examples: map[string]ExampleEntry{}}
		}
		name := "recorded-" + itoa(len(f.Examples)+1)
		e := ExampleEntry{Response: ExampleResponse{Status: rec.Status, MediaType: mediaType(rec.ContentType)}}
		if len(rec.PathParams) > 0 || len(rec.Query) > 0 || len(rec.Headers) > 0 || rec.Body != nil {
			e.Request = &ExampleRequest{Params: rec.PathParams, Query: rec.Query, Headers: rec.Headers, Body: rec.Body}
		}
		if v := jsonOrNil(rec.Response); v != nil {
			e.Response.Body = v
		} else if len(rec.Response) > 0 {
			e.Response.Body = string(rec.Response)
		}
		f.Examples[name] = e
		out[rec.Operation] = f
	}
	return out
}

// ExampleFile mirrors pkg.ExampleFile for YAML/JSON output.
type ExampleFile struct {
	Operation string                  `yaml:"operation" json:"operation"`
	Examples  map[string]ExampleEntry `yaml:"examples" json:"examples"`
}

// ExampleEntry is one recorded example.
type ExampleEntry struct {
	Request  *ExampleRequest `yaml:"request,omitempty" json:"request,omitempty"`
	Response ExampleResponse `yaml:"response" json:"response"`
}

// ExampleRequest is a recorded request half.
type ExampleRequest struct {
	Params  map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
	Query   map[string]string `yaml:"query,omitempty" json:"query,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Body    any               `yaml:"body,omitempty" json:"body,omitempty"`
}

func firstQueryValues(values map[string][]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for name, all := range values {
		if len(all) > 0 {
			out[name] = all[0]
		}
	}
	return out
}

func declaredHeaders(op *pkg.Operation, headers http.Header) map[string]string {
	var out map[string]string
	for _, ref := range op.Parameters() {
		if ref.Value == nil || ref.Value.In != "header" {
			continue
		}
		name := http.CanonicalHeaderKey(ref.Value.Name)
		switch strings.ToLower(name) {
		case "authorization", "cookie", "proxy-authorization", "x-api-key":
			continue
		}
		if value := headers.Get(name); value != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = value
		}
	}
	return out
}

// ExampleResponse is a recorded response.
type ExampleResponse struct {
	Status    int    `yaml:"status" json:"status"`
	MediaType string `yaml:"mediaType,omitempty" json:"mediaType,omitempty"`
	Body      any    `yaml:"body,omitempty" json:"body,omitempty"`
}

func mediaType(ct string) string {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(mt)
}
