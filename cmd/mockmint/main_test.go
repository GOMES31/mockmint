package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, nil, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionAndHelp(t *testing.T) {
	if code, out, _ := runCLI("version"); code != 0 || !strings.HasPrefix(out, "mockmint ") {
		t.Fatalf("version = %d %q", code, out)
	}
	if code, out, _ := runCLI("help"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("help = %d", code)
	}
	if code, _, errs := runCLI("frobnicate"); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown = %d", code)
	}
	if code, _, _ := runCLI(); code != 2 {
		t.Fatalf("no args = %d", code)
	}
}

func TestValidate(t *testing.T) {
	code, out, errs := runCLI("validate", "../../examples/notebook")
	if code != 0 || !strings.Contains(out, "ok  notebook 1.0  mounted at /notebook/1.0  5 operations, 0 warnings") {
		t.Fatalf("validate = %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openapi.yaml"), []byte("openapi: 3.0.3\ninfo: {title: x, version: '1'}\npaths: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runCLI("validate", dir); code != 1 || !strings.Contains(errs, "no paths") {
		t.Fatalf("invalid = %d %s", code, errs)
	}
	if code, _, _ := runCLI("validate"); code != 2 {
		t.Fatalf("no paths = %d", code)
	}
	if code, _, errs := runCLI("validate", "-config", filepath.Join(dir, "missing.yaml"), dir); code != 1 || !strings.Contains(errs, "read config") {
		t.Fatalf("missing config = %d %s", code, errs)
	}
}

// TestServeProcess builds the binary and runs it as a subprocess: startup,
// a real request, and graceful shutdown on interrupt.
func TestServeProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "mockmint")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "serve", "-addr", "127.0.0.1:0", "../../examples/notebook")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			var line map[string]any
			if json.Unmarshal(sc.Bytes(), &line) == nil && line["msg"] == "mockmint ready" {
				addr <- line["addr"].(string)
			}
		}
	}()
	var a string
	select {
	case a = <-addr:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not become ready")
	}
	resp, err := http.Get("http://" + a + "/notebook/1.0/notes/1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "Welcome") {
		t.Fatalf("GET = %d %s", resp.StatusCode, b)
	}
	if runtime.GOOS == "windows" {
		return // no SIGINT delivery to child processes; shutdown is covered by the server tests
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no graceful shutdown")
	}
}
