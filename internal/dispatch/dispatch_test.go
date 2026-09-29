package dispatch

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"
)

var names = []string{"cat", "dog", "fish", "default"}

func req(params map[string]string, query string, header http.Header, body string) *Request {
	q, _ := url.ParseQuery(query)
	if header == nil {
		header = http.Header{}
	}
	return &Request{Method: "POST", Path: "/pets", PathParams: params, Query: q, Header: header, Body: []byte(body)}
}

func mustBuild(t *testing.T, yml string, in Inputs) Dispatcher {
	t.Helper()
	var cfg Config
	if err := yaml.Unmarshal([]byte(yml), &cfg); err != nil {
		t.Fatal(err)
	}
	if in.Examples == nil {
		in.Examples = names
	}
	d, err := Build(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func expect(t *testing.T, d Dispatcher, r *Request, want string) {
	t.Helper()
	got, ok, err := d.Dispatch(r)
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if want == "" {
		if ok {
			t.Fatalf("got %q, want no match", got)
		}
		return
	}
	if !ok || got != want {
		t.Fatalf("got %q (ok=%v), want %q", got, ok, want)
	}
}

func TestStatic(t *testing.T) {
	expect(t, mustBuild(t, "type: static\nexample: dog", Inputs{}), req(nil, "", nil, ""), "dog")
	expect(t, mustBuild(t, "type: static", Inputs{DefaultExample: "cat"}), req(nil, "", nil, ""), "cat")
}

func TestPathParams(t *testing.T) {
	d := mustBuild(t, `
type: path_params
rules:
  - when: {petId: "1"}
    example: cat
  - when: {petId: {regex: "^9\\d*$"}}
    example: dog
  - when: {petId: {in: ["5", "6"]}, kind: {exists: false}}
    example: fish
default: default`, Inputs{})
	expect(t, d, req(map[string]string{"petId": "1"}, "", nil, ""), "cat")
	expect(t, d, req(map[string]string{"petId": "99"}, "", nil, ""), "dog")
	expect(t, d, req(map[string]string{"petId": "5"}, "", nil, ""), "fish")
	expect(t, d, req(map[string]string{"petId": "5", "kind": "x"}, "", nil, ""), "default")
	expect(t, d, req(map[string]string{"petId": "2"}, "", nil, ""), "default")
}

func TestQueryAndHeader(t *testing.T) {
	q := mustBuild(t, `
type: query_params
rules:
  - when: {status: sold}
    example: cat
default: "-"`, Inputs{DefaultExample: "default"})
	expect(t, q, req(nil, "status=available&status=sold", nil, ""), "cat")
	expect(t, q, req(nil, "status=available", nil, ""), "")

	h := mustBuild(t, `
type: header
rules:
  - when: {x-tier: gold}
    example: dog`, Inputs{})
	expect(t, h, req(nil, "", http.Header{"X-Tier": {"gold"}}, ""), "dog")
	expect(t, h, req(nil, "", http.Header{"X-Tier": {"silver"}}, ""), "")
}

func TestBodyJSONPath(t *testing.T) {
	d := mustBuild(t, `
type: body_jsonpath
rules:
  - when: {"$.pet.kind": cat, "$.pet.age": {regex: "^[0-9]$"}}
    example: cat
  - when: {"$.pet.kind": dog}
    example: dog
  - when: {"$.vip": "true"}
    example: fish`, Inputs{})
	expect(t, d, req(nil, "", nil, `{"pet":{"kind":"cat","age":3}}`), "cat")
	expect(t, d, req(nil, "", nil, `{"pet":{"kind":"cat","age":12}}`), "")
	expect(t, d, req(nil, "", nil, `{"pet":{"kind":"dog"}}`), "dog")
	expect(t, d, req(nil, "", nil, `{"vip":true}`), "fish")
	expect(t, d, req(nil, "", nil, `not json`), "")
	expect(t, d, req(nil, "", nil, ``), "")
}

func TestSequence(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"", "cat dog fish fish fish"},
		{"cycle", "cat dog fish cat dog"},
		{"fallback", "cat dog fish - -"},
	} {
		d := mustBuild(t, "type: sequence\nexamples: [cat, dog, fish]\nexhausted: "+tt.mode, Inputs{})
		var got []string
		for range 5 {
			n, ok, _ := d.Dispatch(req(nil, "", nil, ""))
			if !ok {
				n = "-"
			}
			got = append(got, n)
		}
		if s := strings.Join(got, " "); s != tt.want {
			t.Errorf("mode %q: got %s, want %s", tt.mode, s, tt.want)
		}
	}
}

func TestSequenceConcurrent(t *testing.T) {
	d := mustBuild(t, "type: sequence\nexamples: [cat, dog, fish]\nexhausted: fallback", Inputs{})
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			n, ok, _ := d.Dispatch(req(nil, "", nil, ""))
			if !ok {
				n = "-"
			}
			mu.Lock()
			seen[n]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if seen["cat"] != 1 || seen["dog"] != 1 || seen["fish"] != 1 || seen["-"] != 47 {
		t.Fatalf("each example must be served exactly once: %v", seen)
	}
}

func TestScript(t *testing.T) {
	d := mustBuild(t, `
type: script
script: 'body?.kind == "cat" ? "cat" : headers["X-Tier"] == "gold" ? "dog" : query["q"] == "bad" ? "nope" : query["q"] == "num" ? 1 : ""'
default: default`, Inputs{})
	expect(t, d, req(nil, "", nil, `{"kind":"cat"}`), "cat")
	expect(t, d, req(nil, "", http.Header{"X-Tier": {"gold"}}, ""), "dog")
	expect(t, d, req(nil, "", nil, ""), "default")
	if _, _, err := d.Dispatch(req(nil, "q=bad", nil, "")); err == nil || !strings.Contains(err.Error(), "unknown example") {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := d.Dispatch(req(nil, "q=num", nil, "")); err == nil || !strings.Contains(err.Error(), "must be a string") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuto(t *testing.T) {
	d := mustBuild(t, "", Inputs{
		DefaultExample: "default",
		Candidates: []Candidate{
			{Name: "cat", PathParams: map[string]string{"id": "1"}},
			{Name: "dog", PathParams: map[string]string{"id": "1"}, Query: map[string]string{"full": "true"}},
			{Name: "fish", Body: map[string]any{"kind": "fish", "tags": []any{"a"}}},
			{Name: "default"},
		},
	})
	expect(t, d, req(map[string]string{"id": "1"}, "", nil, ""), "cat")
	expect(t, d, req(map[string]string{"id": "1"}, "full=true", nil, ""), "dog") // more constraints win
	expect(t, d, req(nil, "", nil, `{"kind":"fish","tags":["a"],"extra":1}`), "fish")
	expect(t, d, req(nil, "", nil, `{"kind":"fish","tags":["a","b"]}`), "default")
	expect(t, d, req(map[string]string{"id": "2"}, "", nil, ""), "default")

	// A request-bound default must not answer requests it does not match…
	bound := []Candidate{{Name: "cat", PathParams: map[string]string{"id": "1"}}, {Name: "dog", PathParams: map[string]string{"id": "2"}}}
	d = mustBuild(t, "", Inputs{DefaultExample: "dog", Candidates: bound})
	expect(t, d, req(map[string]string{"id": "1"}, "", nil, ""), "cat")
	expect(t, d, req(map[string]string{"id": "77"}, "", nil, ""), "")
	// …unless it is configured explicitly.
	d = mustBuild(t, "default: dog", Inputs{DefaultExample: "dog", Candidates: bound})
	expect(t, d, req(map[string]string{"id": "77"}, "", nil, ""), "dog")
}

func TestBuildErrors(t *testing.T) {
	for _, tt := range []struct{ yml, want string }{
		{"type: nope", "unknown dispatcher type"},
		{"type: static", "no example and no default"},
		{"type: static\nexample: bird", "unknown example"},
		{"type: header\nrules: [{when: {a: b}, example: bird}]", "unknown example"},
		{"type: header\nrules: [{when: {}, example: cat}]", "when must not be empty"},
		{"type: header\nrules: [{when: {a: {regex: '('}}, example: cat}]", "regex"},
		{"type: header\nrules: [{when: {a: {regex: x, in: [y]}}, example: cat}]", "exactly one"},
		{"type: body_jsonpath\nrules: [{when: {'kind': x}, example: cat}]", "must start with $"},
		{"type: sequence", "must not be empty"},
		{"type: sequence\nexamples: [cat]\nexhausted: forever", "exhausted"},
		{"type: script", "must not be empty"},
		{"type: script\nscript: 'nosuchvar'", "unknown name"},
		{"type: script\nscript: 'now()'", "script"},
		{"default: bird", "unknown example"},
	} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(tt.yml), &cfg); err != nil {
			t.Fatal(err)
		}
		_, err := Build(cfg, Inputs{Examples: names})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.yml, err, tt.want)
		}
	}
}
