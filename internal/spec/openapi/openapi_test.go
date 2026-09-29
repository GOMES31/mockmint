package openapi

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

func load(t *testing.T) *Spec {
	t.Helper()
	s, err := Load(context.Background(), os.DirFS("testdata"), "pets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func op(t *testing.T, s *Spec, id string) *Operation {
	t.Helper()
	for _, o := range s.Operations {
		if o.ID == id {
			if err := o.Finalize(rand.New(rand.NewPCG(1, 2))); err != nil {
				t.Fatal(err)
			}
			return o
		}
	}
	t.Fatalf("operation %s not found", id)
	return nil
}

func TestLoadOperations(t *testing.T) {
	s := load(t)
	if s.Title != "Pet Store" || s.Version != "1.2" {
		t.Fatalf("info = %q %q", s.Title, s.Version)
	}
	var ids []string
	for _, o := range s.Operations {
		ids = append(ids, o.ID)
	}
	want := []string{"GET /pets", "POST /pets", "DELETE /pets/{pet-id}", "GET /pets/{pet-id}"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestSpecExamplesPairedByName(t *testing.T) {
	o := op(t, load(t), "GET /pets/{pet-id}")
	if got := o.ExampleNames(); !reflect.DeepEqual(got, []string{"404", "broken", "cat", "dog"}) {
		t.Fatalf("names = %v", got)
	}
	cat, dog := o.Examples["cat"], o.Examples["dog"]
	if !reflect.DeepEqual(cat.PathParams, map[string]string{"pet-id": "1"}) || cat.Query != nil {
		t.Errorf("cat request = %+v %+v", cat.PathParams, cat.Query)
	}
	if !reflect.DeepEqual(dog.Query, map[string]string{"verbose": "true"}) {
		t.Errorf("dog query = %+v", dog.Query)
	}
	if cat.Status != 200 || cat.MediaType != "application/json" || string(cat.ResponseBody) != `{"id":1,"kind":"cat","name":"Tom"}` {
		t.Errorf("cat response = %d %s %s", cat.Status, cat.MediaType, cat.ResponseBody)
	}
	if cat.ResponseHeaders["X-Rate-Remaining"] != "99" {
		t.Errorf("headers = %v", cat.ResponseHeaders)
	}
	nf := o.Examples["404"]
	if nf.Status != 404 || nf.MediaType != "application/problem+json" {
		t.Errorf("404 example = %+v", nf)
	}
	if o.DefaultExample != "broken" {
		t.Errorf("default = %q, want alphabetically first 2xx", o.DefaultExample)
	}
	if len(o.Warnings) != 1 || !strings.Contains(o.Warnings[0], `example "broken" does not match`) {
		t.Errorf("warnings = %v", o.Warnings)
	}
	if !o.HasRequestExamples() {
		t.Error("HasRequestExamples = false")
	}
}

func TestRequestBodyExampleAndDefaultStatus(t *testing.T) {
	o := op(t, load(t), "POST /pets")
	created := o.Examples["created"]
	want := map[string]any{"name": "Nemo", "kind": "fish"}
	if !reflect.DeepEqual(created.Body, want) {
		t.Fatalf("request body = %#v", created.Body)
	}
	boom := o.Examples["500"]
	if boom == nil || boom.MediaType != "text/plain" || string(boom.ResponseBody) != "boom" {
		t.Fatalf("default response example = %+v", boom)
	}
	if o.DefaultExample != "created" {
		t.Fatalf("default = %q", o.DefaultExample)
	}
}

func TestSynthesis(t *testing.T) {
	o := op(t, load(t), "GET /pets")
	g := o.Examples["generated"]
	if g == nil || o.DefaultExample != "generated" || g.Status != 200 {
		t.Fatalf("generated = %+v, default %q", g, o.DefaultExample)
	}
	var list []map[string]any
	if err := json.Unmarshal(g.ResponseBody, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("minItems/maxItems ignored: %s", g.ResponseBody)
	}
	schema := o.Op.Responses.Status(200).Value.Content["application/json"].Schema.Value
	var v any
	_ = json.Unmarshal(g.ResponseBody, &v)
	if err := schema.VisitJSON(v, openapi3.VisitAsResponse()); err != nil {
		t.Fatalf("generated body violates its schema: %v\n%s", err, g.ResponseBody)
	}
	if len(o.Warnings) != 0 {
		t.Fatalf("warnings = %v", o.Warnings)
	}

	// Same rng state → same output.
	again := op(t, load(t), "GET /pets").Examples["generated"]
	if string(again.ResponseBody) != string(g.ResponseBody) {
		t.Fatal("synthesis is not deterministic")
	}

	d := op(t, load(t), "DELETE /pets/{pet-id}")
	if e := d.Examples[d.DefaultExample]; e.Status != 204 || len(e.ResponseBody) != 0 {
		t.Fatalf("204 example = %+v", e)
	}
}

func TestGenerateConstraints(t *testing.T) {
	doc := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/x":{"get":{"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{
	  "type":"object","required":["n","m","e","s","u","all","one","c"],
	  "properties":{
	    "n":{"type":"integer","exclusiveMinimum":10,"exclusiveMaximum":12},
	    "m":{"type":"integer","minimum":7,"maximum":40,"multipleOf":5},
	    "e":{"type":"number","minimum":0.5,"maximum":0.6},
	    "s":{"type":"string","minLength":30,"maxLength":32},
	    "u":{"type":"string","format":"uuid"},
	    "all":{"allOf":[{"type":"object","properties":{"a":{"const":1}}},{"type":"object","properties":{"b":{"const":2}}}]},
	    "one":{"oneOf":[{"type":"boolean"},{"type":"string"}]},
	    "c":{"type":["null","string"],"format":"date-time"},
	    "w":{"type":"string","writeOnly":true}
	  }}}}}}}}}}`
	s, err := Load(context.Background(), fstest.MapFS{"x.json": {Data: []byte(doc)}}, "x.json")
	if err != nil {
		t.Fatal(err)
	}
	schema := s.Operations[0].Op.Responses.Status(200).Value.Content["application/json"].Schema.Value
	for seed := range uint64(300) {
		v, err := Generate(schema, rand.New(rand.NewPCG(seed, seed)))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.VisitJSON(v, openapi3.VisitAsResponse(), openapi3.EnableFormatValidation()); err != nil {
			b, _ := json.Marshal(v)
			t.Fatalf("seed %d: %v\n%s", seed, err, b)
		}
		m := v.(map[string]any)
		if m["n"] != float64(11) {
			t.Fatalf("exclusive bounds: n = %v", m["n"])
		}
		if _, ok := m["w"]; ok {
			t.Fatal("writeOnly property generated in a response")
		}
		if all := m["all"].(map[string]any); all["a"] != float64(1) || all["b"] != float64(2) {
			t.Fatalf("allOf = %v", all)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	mk := func(spec string, extra ...string) fstest.MapFS {
		fs := fstest.MapFS{"s.yaml": {Data: []byte(spec)}}
		for i := 0; i+1 < len(extra); i += 2 {
			fs[extra[i]] = &fstest.MapFile{Data: []byte(extra[i+1])}
		}
		return fs
	}
	head := "openapi: 3.0.3\ninfo: {title: t, version: '1'}\n"
	for _, tt := range []struct {
		name string
		fs   fstest.MapFS
		want string
	}{
		{"no paths", mk(head + "paths: {}\n"), "no paths"},
		{"invalid", mk(head + "paths:\n  /a/{id}:\n    get:\n      responses: {'200': {description: ok}}\n"), "invalid OpenAPI document"},
		{"remote ref", mk(head + "paths:\n  /a:\n    get:\n      responses: {'200': {$ref: 'http://169.254.169.254/x'}}\n"), "remote $ref"},
		{"escaping ref", mk(head + "paths:\n  /a:\n    get:\n      responses: {'200': {$ref: '../../etc/passwd'}}\n"), "escapes the package"},
		{"missing file", mk(head), "file does not exist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name := "s.yaml"
			if tt.name == "missing file" {
				name = "nope.yaml"
			}
			_, err := Load(context.Background(), tt.fs, name)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateRequest(t *testing.T) {
	s := load(t)
	post := op(t, s, "POST /pets")
	get := op(t, s, "GET /pets/{pet-id}")

	mkReq := func(method, target, body string) *http.Request {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		return r
	}
	if v := post.ValidateRequest(context.Background(), mkReq("POST", "/pets", `{"name":"Nemo","kind":"fish"}`), nil); v != nil {
		t.Fatalf("valid body rejected: %+v", v)
	}
	v := post.ValidateRequest(context.Background(), mkReq("POST", "/pets", `{"name":"N","kind":"bird"}`), nil)
	if len(v) != 2 || v[0].In != "body" || v[0].Name != "/kind" || v[1].Name != "/name" {
		t.Fatalf("violations = %+v", v)
	}
	v = get.ValidateRequest(context.Background(), mkReq("GET", "/pets/x?verbose=maybe", ""), map[string]string{"pet-id": "x"})
	if len(v) != 2 || v[0].In != "path" || v[0].Name != "pet-id" || v[1].In != "query" || v[1].Name != "verbose" {
		t.Fatalf("violations = %+v", v)
	}
}

func TestAcceptsContentType(t *testing.T) {
	post := op(t, load(t), "POST /pets")
	for ct, want := range map[string]bool{
		"application/json":                true,
		"Application/JSON; charset=utf-8": true,
		"text/plain":                      false,
		"":                                false,
	} {
		if got := post.AcceptsContentType(ct); got != want {
			t.Errorf("%q = %v", ct, got)
		}
	}
	if !op(t, load(t), "GET /pets").AcceptsContentType("text/plain") {
		t.Error("operation without request body must accept anything")
	}
	if !mediaTypeMatches("application/*", "application/xml") || mediaTypeMatches("application/*", "text/xml") {
		t.Error("wildcard matching wrong")
	}
}

func TestAddExample(t *testing.T) {
	o := op(t, load(t), "GET /pets/{pet-id}")
	err := o.AddExample(&Example{Name: "cat", Source: "examples/cat.yaml", ResponseBody: []byte(`{"id":1,"name":"Tommy","kind":"cat"}`)})
	if err != nil {
		t.Fatal(err)
	}
	cat := o.Examples["cat"]
	if cat.Status != 200 || cat.MediaType != "application/json" || cat.ResponseHeaders["X-Rate-Remaining"] != "99" {
		t.Fatalf("defaults not applied: %+v", cat)
	}
	if !strings.Contains(strings.Join(o.Warnings, "\n"), `replaces the one from spec`) {
		t.Fatalf("warnings = %v", o.Warnings)
	}
	if err := o.AddExample(&Example{Name: "x", Status: 42}); err == nil {
		t.Fatal("invalid status accepted")
	}
	if err := o.AddExample(&Example{Source: "f"}); err == nil {
		t.Fatal("nameless example accepted")
	}
}

func TestIsDocument(t *testing.T) {
	if !IsDocument([]byte("# c\nopenapi: 3.1.0\n")) || !IsDocument([]byte(`{"openapi":"3.0.0"}`)) {
		t.Fatal("OpenAPI not detected")
	}
	if IsDocument([]byte("asyncapi: 3.0.0\n")) || IsDocument([]byte("x:\n  openapi: 1\n")) {
		t.Fatal("false positive")
	}
}

func TestGenerateCapsSizes(t *testing.T) {
	huge := uint64(1 << 40)
	s := &openapi3.Schema{Type: &openapi3.Types{"array"}, MinItems: huge, Items: &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{"string"}, MinLength: huge,
	}}}
	out, err := Generate(s, rand.New(rand.NewPCG(1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	v := out.([]any)
	if len(v) != maxGenItems {
		t.Fatalf("items = %d, want capped at %d", len(v), maxGenItems)
	}
	if n := len(v[0].(string)); n < maxGenLength || n > maxGenLength+20 {
		t.Fatalf("string length = %d, want about %d", n, maxGenLength)
	}
}

func TestGenerateNestedBudget(t *testing.T) {
	// Six nested arrays of minItems 100 would be 10^12 values.
	leaf := &openapi3.Schema{Type: &openapi3.Types{"integer"}}
	s := leaf
	for range 6 {
		s = &openapi3.Schema{Type: &openapi3.Types{"array"}, MinItems: 100, Items: &openapi3.SchemaRef{Value: s}}
	}
	start := time.Now()
	if _, err := Generate(s, rand.New(rand.NewPCG(1, 1))); !errors.Is(err, ErrSchemaTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("budget did not stop generation promptly")
	}
	// Within budget: 3 levels of 20 = 8000 leaves plus 421 arrays.
	s = leaf
	for range 3 {
		s = &openapi3.Schema{Type: &openapi3.Types{"array"}, MinItems: 20, Items: &openapi3.SchemaRef{Value: s}}
	}
	if _, err := Generate(s, rand.New(rand.NewPCG(1, 1))); err != nil {
		t.Fatalf("in-budget schema rejected: %v", err)
	}
}
