package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mockmint.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadFileThenEnv(t *testing.T) {
	p := writeFile(t, `
http:
  addr: ":9000"
  shutdownTimeout: 3s
log:
  format: text
packages:
  paths: [a, b]
`)
	cfg, err := Load(p, []string{
		"MOCKMINT_HTTP_ADDR=:7000",
		"MOCKMINT_PACKAGES_PATHS= c , d,",
		"MOCKMINT_DEFAULTS_SEED=42",
		"MOCKMINT_HTTP_IDLETIMEOUT=2m",
		"PATH=/usr/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != ":7000" {
		t.Errorf("addr = %q", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ShutdownTimeout.D() != 3*time.Second {
		t.Errorf("shutdown = %v", cfg.HTTP.ShutdownTimeout.D())
	}
	if cfg.HTTP.IdleTimeout.D() != 2*time.Minute {
		t.Errorf("idle = %v", cfg.HTTP.IdleTimeout.D())
	}
	if cfg.Log.Format != "text" || cfg.Log.Level != "info" {
		t.Errorf("log = %+v", cfg.Log)
	}
	if !reflect.DeepEqual(cfg.Packages.Paths, []string{"c", "d"}) {
		t.Errorf("paths = %v", cfg.Packages.Paths)
	}
	if cfg.Defaults.Seed != 42 {
		t.Errorf("seed = %d", cfg.Defaults.Seed)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	if _, err := Load(writeFile(t, ""), nil); err != nil {
		t.Fatal(err)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, file string
		env        []string
		want       string
	}{
		{"unknown yaml field", "http:\n  adr: x\n", nil, "field adr not found"},
		{"bad duration", "http:\n  idleTimeout: soon\n", nil, "invalid duration"},
		{"bad level", "log:\n  level: loud\n", nil, "log.level"},
		{"bad validation", "", []string{"MOCKMINT_DEFAULTS_VALIDATION=maybe"}, "defaults.validation"},
		{"unknown env", "", []string{"MOCKMINT_HTTP_PORT=1"}, "MOCKMINT_HTTP_PORT: unknown"},
		{"bad env int", "", []string{"MOCKMINT_HTTP_MAXBODYBYTES=big"}, "MOCKMINT_HTTP_MAXBODYBYTES"},
		{"nonpositive body", "", []string{"MOCKMINT_HTTP_MAXBODYBYTES=0"}, "maxBodyBytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tt.file), tt.env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
