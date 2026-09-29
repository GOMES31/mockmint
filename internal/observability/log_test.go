package observability

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewLogger(&buf, "warn", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown", "k", "v")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, `"msg":"shown","k":"v"`) {
		t.Fatalf("output = %s", out)
	}
	if _, err := NewLogger(&buf, "loud", "json"); err == nil {
		t.Fatal("bad level accepted")
	}
	if _, err := NewLogger(&buf, "info", "xml"); err == nil {
		t.Fatal("bad format accepted")
	}
}
