package http

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mockmint/mockmint/internal/behavior"
	"github.com/mockmint/mockmint/internal/pkg"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// loadNotebookB loads the sample package with latency disabled so
// benchmarks measure mockmint, not injected sleeps.
func loadNotebookB(b *testing.B) []*pkg.Package {
	b.Helper()
	pkgs, err := pkg.LoadPath(context.Background(), "../../../examples/notebook", pkg.Defaults{Validation: "warn"}, nil)
	if err != nil {
		b.Fatal(err)
	}
	for _, p := range pkgs {
		for _, op := range p.Operations {
			op.Behavior = noBehavior()
		}
	}
	return pkgs
}

func noBehavior() *behavior.Behavior {
	b, _ := behavior.Compile(behavior.Config{}, nil)
	return b
}
