// Command mockmint serves HTTP mocks from OpenAPI mock packages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mockmint/mockmint/internal/admin"
	"github.com/mockmint/mockmint/internal/app"
	"github.com/mockmint/mockmint/internal/config"
	"github.com/mockmint/mockmint/internal/observability"
	"github.com/mockmint/mockmint/internal/pkg"
	mamqp "github.com/mockmint/mockmint/internal/protocol/amqp"
	mhttp "github.com/mockmint/mockmint/internal/protocol/http"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `mockmint - lightweight API mocking

Usage:
  mockmint serve    [-config file] [-addr :8080] [-admin-addr 127.0.0.1:9090|off] [-amqp-url amqp://...] [package paths...]
  mockmint validate [-config file] [package paths...]
  mockmint version

Package paths may be package directories, .zip/.tar.gz archives, or
directories of packages. They are added to packages.paths from the config.
SIGHUP (or POST /admin/reload) reloads them atomically.
Configuration can also be set with MOCKMINT_* environment variables, e.g.
MOCKMINT_HTTP_ADDR=:9000 or MOCKMINT_PACKAGES_PATHS=a,b.
`

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}

func run(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], environ, stderr)
	case "validate":
		return validate(args[1:], environ, stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "mockmint", version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

type common struct {
	cfg config.Config
	log *slog.Logger
}

// setup parses flags and config. It returns code >= 0 when the command
// should exit immediately with that code.
func setup(name string, args, environ []string, stderr io.Writer, extra func(*flag.FlagSet)) (*common, int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "YAML config file")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0
		}
		return nil, 2
	}
	cfg, err := config.Load(*cfgPath, environ)
	if err != nil {
		fmt.Fprintln(stderr, "mockmint:", err)
		return nil, 1
	}
	cfg.Packages.Paths = append(cfg.Packages.Paths, fs.Args()...)
	log, err := observability.NewLogger(stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		fmt.Fprintln(stderr, "mockmint:", err)
		return nil, 1
	}
	return &common{cfg: cfg, log: log}, -1
}

func loadPackages(ctx context.Context, c *common) ([]*pkg.Package, error) {
	d := pkg.Defaults{Validation: c.cfg.Defaults.Validation, Seed: c.cfg.Defaults.Seed}
	var all []*pkg.Package
	var errs []error
	for _, p := range c.cfg.Packages.Paths {
		pkgs, err := pkg.LoadPath(ctx, p, d, c.log)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		all = append(all, pkgs...)
	}
	return all, errors.Join(errs...)
}

func serve(args, environ []string, stderr io.Writer) int {
	start := time.Now()
	var addr, adminAddr, amqpURL string
	c, code := setup("serve", args, environ, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&addr, "addr", "", "listen address (overrides http.addr)")
		fs.StringVar(&adminAddr, "admin-addr", "", "admin API listen address (overrides admin.addr); \"off\" disables it")
		fs.StringVar(&amqpURL, "amqp-url", "", "RabbitMQ URL (overrides amqp.url); empty serves HTTP only")
	})
	if code >= 0 {
		return code
	}
	if addr != "" {
		c.cfg.HTTP.Addr = addr
	}
	switch adminAddr {
	case "":
	case "off":
		c.cfg.Admin.Addr = ""
	default:
		c.cfg.Admin.Addr = adminAddr
	}
	if amqpURL != "" {
		c.cfg.AMQP.URL = amqpURL
	}
	if err := c.cfg.Validate(); err != nil {
		c.log.Error("invalid configuration", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(c.cfg, c.log)
	if err != nil {
		c.log.Error("starting failed", "error", err)
		return 1
	}
	defer a.Close() // stops the RabbitMQ engine; in-flight messages are acked or requeued
	// A bad package set at startup is fatal; later reloads keep the old set.
	// The RabbitMQ engine never blocks startup: an unreachable broker is
	// retried in the background while HTTP serves.
	if err := a.Reload(ctx); err != nil {
		c.log.Error("loading packages failed", "error", err)
		return 1
	}
	if len(a.Packages()) == 0 {
		c.log.Warn("no packages configured; every request will get 404 until one is uploaded")
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", c.cfg.HTTP.Addr)
	if err != nil {
		c.log.Error("listen failed", "addr", c.cfg.HTTP.Addr, "error", err)
		return 1
	}
	errc := make(chan error, 2)
	go func() { errc <- a.Server().Serve(ln) }()

	var adminSrv *http.Server
	adminListen := ""
	if c.cfg.Admin.Addr != "" {
		aln, err := lc.Listen(ctx, "tcp", c.cfg.Admin.Addr)
		if err != nil {
			c.log.Error("admin listen failed", "addr", c.cfg.Admin.Addr, "error", err)
			return 1
		}
		adminListen = aln.Addr().String()
		adminSrv = &http.Server{
			Handler: admin.New(a, admin.Options{
				Token: c.cfg.Admin.Token, MaxUploadBytes: c.cfg.Admin.MaxUploadBytes, Version: version, Log: c.log,
			}),
			ReadHeaderTimeout: c.cfg.HTTP.ReadHeaderTimeout.D(),
			IdleTimeout:       c.cfg.HTTP.IdleTimeout.D(),
			ErrorLog:          slog.NewLogLogger(c.log.Handler(), slog.LevelWarn),
		}
		go func() {
			if err := adminSrv.Serve(aln); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
		if c.cfg.Admin.Token == "" && c.cfg.Admin.Exposed() {
			c.log.Warn("admin API has no token; anyone who can reach it can change packages", "addr", adminListen)
		}
	}
	c.log.Info("mockmint ready", "version", version, "addr", ln.Addr().String(), "admin", adminListen,
		"packages", len(a.Packages()), "startup", time.Since(start).String())

	hup := make(chan os.Signal, 1)
	notifyReload(hup)
	defer signal.Stop(hup)

	exit := 0
loop:
	for {
		select {
		case err := <-errc:
			if err != nil {
				c.log.Error("server failed", "error", err)
				exit = 1
			}
			break loop
		case <-hup:
			if err := a.Reload(ctx); err != nil {
				c.log.Error("reload failed; still serving the previous packages", "error", err)
			}
		case <-ctx.Done():
			break loop
		}
	}
	c.log.Info("shutting down", "timeout", c.cfg.HTTP.ShutdownTimeout.D().String())
	sctx, cancel := context.WithTimeout(context.Background(), c.cfg.HTTP.ShutdownTimeout.D())
	defer cancel()
	if adminSrv != nil {
		_ = adminSrv.Shutdown(sctx)
	}
	if err := a.Server().Shutdown(sctx); err != nil {
		c.log.Error("shutdown incomplete", "error", err)
		exit = 1
	}
	return exit
}

func validate(args, environ []string, stdout, stderr io.Writer) int {
	c, code := setup("validate", args, environ, stderr, nil)
	if code >= 0 {
		return code
	}
	if len(c.cfg.Packages.Paths) == 0 {
		fmt.Fprintln(stderr, "mockmint validate: no package paths given")
		return 2
	}
	pkgs, err := loadPackages(context.Background(), c)
	if err == nil {
		_, err = mhttp.NewRouter(pkgs, mhttp.RouterOptions{MaxBodyBytes: c.cfg.HTTP.MaxBodyBytes, Log: c.log})
	}
	if err == nil {
		_, err = mamqp.Plan(pkgs)
	}
	if err != nil {
		fmt.Fprintln(stderr, "invalid:", err)
		return 1
	}
	for _, p := range pkgs {
		warnings := 0
		for _, op := range p.Operations {
			warnings += len(op.Warnings)
		}
		async := ""
		if p.Async != nil {
			warnings += len(p.AsyncSpec.Warnings)
			async = fmt.Sprintf(", %d async operations", len(p.Async.Operations))
		}
		fmt.Fprintf(stdout, "ok  %s %s  mounted at %s  %d operations%s, %d warnings\n",
			p.Name, p.Version, orSlash(p.BasePath), len(p.Operations), async, warnings)
	}
	return 0
}

func orSlash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "/"
	}
	return s
}
