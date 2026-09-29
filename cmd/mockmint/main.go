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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
  mockmint serve    [-config file] [-addr :8080] [-amqp-url amqp://...] [package paths...]
  mockmint validate [-config file] [package paths...]
  mockmint version

Package paths may be package directories, .zip/.tar.gz archives, or
directories of packages. They are added to packages.paths from the config.
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
	var addr, amqpURL string
	c, code := setup("serve", args, environ, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&addr, "addr", "", "listen address (overrides http.addr)")
		fs.StringVar(&amqpURL, "amqp-url", "", "RabbitMQ URL (overrides amqp.url); empty serves HTTP only")
	})
	if code >= 0 {
		return code
	}
	if addr != "" {
		c.cfg.HTTP.Addr = addr
	}
	if amqpURL != "" {
		c.cfg.AMQP.URL = amqpURL
		if err := c.cfg.Validate(); err != nil {
			c.log.Error("invalid configuration", "error", err)
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pkgs, err := loadPackages(ctx, c)
	if err != nil {
		c.log.Error("loading packages failed", "error", err)
		return 1
	}
	if len(pkgs) == 0 {
		c.log.Warn("no packages configured; every request will get 404")
	}
	rt, err := mhttp.NewRouter(pkgs, c.cfg.HTTP.MaxBodyBytes, c.log)
	if err != nil {
		c.log.Error("building routes failed", "error", err)
		return 1
	}
	engine, err := mamqp.New(pkgs, amqpOptions(c.cfg.AMQP), c.log)
	if err != nil {
		c.log.Error("planning RabbitMQ topology failed", "error", err)
		return 1
	}
	srv := mhttp.NewServer(rt, mhttp.Options{
		Addr:              c.cfg.HTTP.Addr,
		ReadHeaderTimeout: c.cfg.HTTP.ReadHeaderTimeout.D(),
		IdleTimeout:       c.cfg.HTTP.IdleTimeout.D(),
	}, c.log)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.cfg.HTTP.Addr)
	if err != nil {
		c.log.Error("listen failed", "addr", c.cfg.HTTP.Addr, "error", err)
		return 1
	}
	c.log.Info("mockmint ready", "version", version, "addr", ln.Addr().String(), "packages", len(pkgs), "startup", time.Since(start).String())

	// The AMQP engine never blocks startup: an unreachable broker is retried
	// in the background while HTTP serves.
	amqpDone := make(chan struct{})
	actx, stopAMQP := context.WithCancel(context.Background())
	defer stopAMQP()
	switch {
	case !engine.HasOperations():
		close(amqpDone)
	case c.cfg.AMQP.URL == "":
		c.log.Warn("packages have AsyncAPI operations but amqp.url is not set; serving HTTP only")
		close(amqpDone)
	default:
		go func() {
			defer close(amqpDone)
			engine.Run(actx)
		}()
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	exit := 0
	select {
	case err := <-errc:
		if err != nil {
			c.log.Error("server failed", "error", err)
			exit = 1
		}
	case <-ctx.Done():
	}
	c.log.Info("shutting down", "timeout", c.cfg.HTTP.ShutdownTimeout.D().String())
	sctx, cancel := context.WithTimeout(context.Background(), c.cfg.HTTP.ShutdownTimeout.D())
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		c.log.Error("shutdown incomplete", "error", err)
		exit = 1
	}
	stopAMQP() // stops consumers; in-flight messages are acked or requeued
	select {
	case <-amqpDone:
	case <-sctx.Done():
		c.log.Error("RabbitMQ shutdown incomplete")
		exit = 1
	}
	return exit
}

func amqpOptions(a config.AMQP) mamqp.Options {
	return mamqp.Options{
		URL:            a.URL,
		Prefetch:       a.Prefetch,
		Heartbeat:      a.Heartbeat.D(),
		ReconnectMin:   a.ReconnectMin.D(),
		ReconnectMax:   a.ReconnectMax.D(),
		ConfirmTimeout: a.ConfirmTimeout.D(),
	}
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
		_, err = mhttp.NewRouter(pkgs, c.cfg.HTTP.MaxBodyBytes, c.log)
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
