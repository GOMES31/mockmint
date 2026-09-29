package pkg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const petstore = "../../examples/petstore"

func loadOne(t *testing.T, dir string) *Package {
	t.Helper()
	pkgs, err := LoadPath(context.Background(), dir, Defaults{Validation: "warn"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("got %d packages", len(pkgs))
	}
	return pkgs[0]
}

func opByID(t *testing.T, p *Package, id string) *Operation {
	t.Helper()
	for _, o := range p.Operations {
		if o.ID == id || o.OperationID == id {
			return o
		}
	}
	t.Fatalf("operation %s not found", id)
	return nil
}

func TestLoadPetstore(t *testing.T) {
	p := loadOne(t, petstore)
	if p.Name != "petstore" || p.Version != "1.0" || p.BasePath != "/petstore/1.0" || p.Seed != 42 {
		t.Fatalf("package = %s %s %q seed %d", p.Name, p.Version, p.BasePath, p.Seed)
	}
	if want := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC); !p.Now().Equal(want) {
		t.Fatalf("clock = %v", p.Now())
	}
	if len(p.Operations) != 5 {
		t.Fatalf("operations = %d", len(p.Operations))
	}

	get := opByID(t, p, "getPet")
	if get.Validation != "strict" || get.Fallback == nil || get.Fallback.Status != 404 {
		t.Fatalf("getPet = %+v", get)
	}

	create := opByID(t, p, "createPet")
	created := create.Responses["created"]
	if created.Status != 201 || created.BodyTemplate == nil || len(created.HeaderTemplates) != 1 || created.HeaderTemplates[0].Name != "X-Created-Name" {
		t.Fatalf("created = %+v", created)
	}
	nb := create.Responses["no-birds"]
	if nb.Status != 422 || nb.MediaType != "application/problem+json" || nb.BodyTemplate != nil {
		t.Fatalf("no-birds = %+v", nb)
	}

	seq := opByID(t, p, "getOrderStatus")
	if got := seq.ExampleNames(); !reflect.DeepEqual(got, []string{"approved", "delivered", "placed"}) {
		t.Fatalf("sequence examples = %v", got)
	}
	if seq.Responses["placed"].MediaType != "application/json" {
		t.Fatalf("media type not inferred: %q", seq.Responses["placed"].MediaType)
	}
}

func TestLoadDirectoryOfPackages(t *testing.T) {
	root := t.TempDir()
	copyDir(t, petstore, filepath.Join(root, "petstore"))
	writeArchive(t, filepath.Join(root, "mini.zip"), "zip", map[string]string{
		"mini/openapi.yaml": miniSpec,
	})
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	pkgs, err := LoadPath(context.Background(), root, Defaults{Validation: "off"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages", len(pkgs))
	}
	var mini *Package
	for _, p := range pkgs {
		if p.Name == "mini-api" {
			mini = p
		}
	}
	if mini == nil || mini.BasePath != "/mini-api/2" || mini.Operations[0].Validation != "off" {
		t.Fatalf("mini = %+v", mini)
	}
}

const miniSpec = `openapi: 3.0.3
info: {title: "Mini API!", version: "2"}
paths:
  /ping:
    get:
      responses:
        "200":
          description: ok
          content:
            text/plain:
              example: pong
`

func TestArchives(t *testing.T) {
	for _, kind := range []string{"zip", "tgz"} {
		t.Run(kind, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "p."+kind)
			writeArchive(t, p, kind, map[string]string{
				"openapi.yaml":   miniSpec,
				"mockmint.yaml":  "basePath: /\n",
				"examples/x.txt": "ignored",
			})
			pk := loadOne(t, p)
			if pk.BasePath != "" || string(pk.Operations[0].Responses["200"].Body) != "pong" {
				t.Fatalf("package = %+v", pk)
			}
		})
	}
}

func TestArchiveRejectsTraversal(t *testing.T) {
	for _, kind := range []string{"zip", "tgz"} {
		var buf bytes.Buffer
		writeArchiveTo(t, &buf, kind, map[string]string{"../evil.yaml": "x"})
		name := "a." + kind
		if _, err := OpenArchive(name, buf.Bytes()); err == nil || !strings.Contains(err.Error(), "unsafe archive entry") {
			t.Errorf("%s: err = %v", kind, err)
		}
	}
}

func TestArchiveSizeLimit(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("big.bin")
	chunk := bytes.Repeat([]byte{0}, 1<<20)
	for range MaxArchiveBytes>>20 + 1 {
		_, _ = w.Write(chunk)
	}
	_ = zw.Close()
	if _, err := OpenArchive("big.zip", buf.Bytes()); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemFS(t *testing.T) {
	m := newMemFS(map[string][]byte{"a/b/c.txt": []byte("c"), "a/d.txt": []byte("d"), "e.txt": []byte("e")})
	if err := fstest.TestFS(m, "a/b/c.txt", "a/d.txt", "e.txt"); err != nil {
		t.Fatal(err)
	}
	sub := stripCommonRoot(newMemFS(map[string][]byte{"root/x.yaml": nil, "root/y/z.yaml": nil}))
	if _, err := fs.Stat(sub, "y/z.yaml"); err != nil {
		t.Fatalf("common root not stripped: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	spec := miniSpec
	for _, tt := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no spec", map[string]string{"mockmint.yaml": "name: x\n"}, "no OpenAPI document"},
		{"two specs", map[string]string{"a.yaml": spec, "b.yaml": spec}, "several OpenAPI documents"},
		{"unknown manifest field", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "nameee: x\n"}, "field nameee not found"},
		{"unknown operation", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "operations: {getNope: {}}\n"}, `"getNope" matches no operation`},
		{"bad dispatcher", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "operations: {'GET /ping': {dispatcher: {type: static, example: nope}}}\n"}, "unknown example"},
		{"bad fallback", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "operations: {'GET /ping': {fallback: {example: nope}}}\n"}, "fallback: unknown example"},
		{"bad behavior", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "behavior: {faults: [{probability: 2, status: 500}]}\n"}, "probability"},
		{"bad validation", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "validation: loud\n"}, "validation"},
		{"bad basePath", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "basePath: api\n"}, "must start with /"},
		{"bad name", map[string]string{"openapi.yaml": spec, "mockmint.yaml": "name: 'a/b'\n"}, "URL path segments"},
		{"bad template", map[string]string{"openapi.yaml": spec, "examples/x.yaml": "operation: GET /ping\nexamples:\n  x:\n    response: {body: '{{ .Nope '}\n"}, "body template"},
		{"example op missing", map[string]string{"openapi.yaml": spec, "examples/x.yaml": "operation: nope\nexamples: {}\n"}, `operation "nope" not found`},
		{"example bad field", map[string]string{"openapi.yaml": spec, "examples/x.yaml": "operation: GET /ping\nexamples:\n  x: {respnse: {}}\n"}, "field respnse not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for k, v := range tt.files {
				fsys[k] = &fstest.MapFile{Data: []byte(v)}
			}
			_, err := Load(context.Background(), fsys, "test", Defaults{}, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestOperationConfiguredTwice(t *testing.T) {
	spec := strings.Replace(miniSpec, "get:", "get:\n      operationId: ping", 1)
	fsys := fstest.MapFS{
		"openapi.yaml":  {Data: []byte(spec)},
		"mockmint.yaml": {Data: []byte("operations:\n  ping: {}\n  GET /ping: {}\n")},
	}
	if _, err := Load(context.Background(), fsys, "t", Defaults{}, nil); err == nil || !strings.Contains(err.Error(), "configured twice") {
		t.Fatalf("err = %v", err)
	}
}

func TestPackageFallbackInheritedOnlyWhereExampleExists(t *testing.T) {
	fsys := fstest.MapFS{
		"openapi.yaml":  {Data: []byte(strings.Replace(miniSpec, "example: pong", "examples: {pong: {value: pong}, gone: {value: gone}}", 1))},
		"mockmint.yaml": {Data: []byte("fallback: {example: gone}\n")},
	}
	p, err := Load(context.Background(), fsys, "t", Defaults{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fb := p.Operations[0].Fallback; fb == nil || string(fb.Body) != "gone" {
		t.Fatalf("fallback = %+v", fb)
	}
}

func TestSlugAndBasePath(t *testing.T) {
	if got := slug("  Pet Store API (v2)! "); got != "pet-store-api-v2" {
		t.Fatalf("slug = %q", got)
	}
	for _, tt := range []struct{ explicit, want string }{
		{"", "/n/v"}, {"/", ""}, {"/api/v1/", "/api/v1"},
	} {
		got, err := basePath(tt.explicit, "n", "v")
		if err != nil || got != tt.want {
			t.Errorf("basePath(%q) = %q, %v", tt.explicit, got, err)
		}
	}
	if _, err := basePath("/a/../b", "n", "v"); err == nil {
		t.Error("dot-dot segment accepted")
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeArchive(t *testing.T, path, kind string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	writeArchiveTo(t, &buf, kind, files)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeArchiveTo(t *testing.T, buf *bytes.Buffer, kind string, files map[string]string) {
	t.Helper()
	switch kind {
	case "zip":
		zw := zip.NewWriter(buf)
		for name, content := range files {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(content))
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	case "tgz":
		gz := gzip.NewWriter(buf)
		tw := tar.NewWriter(gz)
		for name, content := range files {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write([]byte(content))
		}
		_ = tw.Close()
		_ = gz.Close()
	}
}

func TestDirectoryPackageSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	secret := `type: object
properties: {leaked: {type: string, example: secret}}
`
	if err := os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(dir, "schema.yaml")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	spec := `openapi: 3.0.3
info: {title: s, version: "1"}
paths:
  /x:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {$ref: "schema.yaml"}
`
	if err := os.WriteFile(filepath.Join(dir, "openapi.yaml"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPath(context.Background(), dir, Defaults{}, nil)
	if err == nil || !strings.Contains(err.Error(), "path escapes") {
		t.Fatalf("symlink outside the package was followed: err = %v", err)
	}
}
