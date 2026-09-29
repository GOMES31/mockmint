package http

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mockmint/mockmint/internal/problem"
)

// Server is the mock traffic server. Its Router can be replaced at any time
// with Swap; in-flight requests finish on the router they started with.
type Server struct {
	router atomic.Pointer[Router]
	srv    *http.Server
	log    *slog.Logger
}

// Options configure the listener.
type Options struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
}

// NewServer returns a server that routes through rt.
func NewServer(rt *Router, o Options, log *slog.Logger) *Server {
	s := &Server{log: log}
	s.router.Store(rt)
	s.srv = &http.Server{
		Addr:              o.Addr,
		Handler:           s,
		ReadHeaderTimeout: o.ReadHeaderTimeout,
		IdleTimeout:       o.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// Swap atomically replaces the router and returns the previous one.
func (s *Server) Swap(rt *Router) *Router { return s.router.Swap(rt) }

// Router returns the active router.
func (s *Server) Router() *Router { return s.router.Load() }

// ServeHTTP routes through the active router, converting handler panics
// (other than deliberate aborts) into 500 problems.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				panic(v)
			}
			s.log.Error("handler panic", "panic", v, "method", r.Method, "path", r.URL.Path)
			problem.Write(w, r, problem.New(problem.TypeInternal, http.StatusInternalServerError, "internal error"))
		}
	}()
	s.router.Load().ServeHTTP(w, r)
}

// Serve accepts connections on ln until Shutdown.
func (s *Server) Serve(ln net.Listener) error {
	err := s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }
