package http

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mockmint/mockmint/internal/observability"
)

// recWriter captures what the handler writes, for metrics and the traffic
// buffer. Handlers annotate it with the package, operation and example.
type recWriter struct {
	http.ResponseWriter
	status  int
	body    bytes.Buffer // first MaxBodyPreview+1 bytes only
	pkg     string
	op      string
	example string
	outcome string // "proxied" when forwarded upstream
}

func (w *recWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if room := observability.MaxBodyPreview + 1 - w.body.Len(); room > 0 {
		w.body.Write(b[:min(len(b), room)])
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (fault drops,
// flushing in the reverse proxy).
func (w *recWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// annotate records which package/operation/example handled the request.
func annotate(w http.ResponseWriter, pkgName, op, example string) {
	if rw, ok := w.(*recWriter); ok {
		rw.pkg, rw.op, rw.example = pkgName, op, example
	}
}

func markProxied(w http.ResponseWriter) {
	if rw, ok := w.(*recWriter); ok {
		rw.outcome = "proxied"
	}
}

// observe wraps next with metrics and traffic recording. The recorded
// request body is whatever the handler read.
func (rt *Router) observe(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if rt.metrics == nil && rt.traffic == nil {
		next.ServeHTTP(w, r)
		return
	}
	start := time.Now()
	rw := &recWriter{ResponseWriter: w}
	cb := &captureBody{rc: r.Body}
	r.Body = cb
	defer func() {
		status := rw.status
		if status == 0 {
			status = 499 // client closed before a response (nginx convention)
		}
		d := time.Since(start)
		if m := rt.metrics; m != nil {
			m.HTTPRequests.Inc(rw.pkg, rw.op, strconv.Itoa(status))
			m.HTTPDuration.Observe(d.Seconds(), rw.pkg, rw.op)
		}
		rt.traffic.Record(observability.Exchange{
			Time: start, Protocol: "http", Package: rw.pkg, Operation: rw.op, Example: rw.example,
			Duration: d, Method: r.Method, Path: r.URL.RequestURI(), Status: status, Outcome: rw.outcome,
			Headers: observability.Headers(r.Header), Body: cb.buf.String(), RespBody: rw.body.String(),
		})
	}()
	next.ServeHTTP(rw, r)
}

// captureBody tees the first MaxBodyPreview+1 bytes of the request body.
type captureBody struct {
	rc  io.ReadCloser
	buf bytes.Buffer
}

func (c *captureBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if room := observability.MaxBodyPreview + 1 - c.buf.Len(); room > 0 && n > 0 {
		c.buf.Write(p[:min(n, room)])
	}
	return n, err
}

func (c *captureBody) Close() error { return c.rc.Close() }
