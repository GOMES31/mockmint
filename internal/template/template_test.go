package template

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var frozen = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

func render(t *testing.T, text string, req Request, seed int64, parts ...[]byte) string {
	t.Helper()
	tpl, err := Parse("t", text)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tpl.Execute(NewData(req, "GET /pets/{id}", "cat", NewRand(seed, parts...), frozen))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestRequestEcho(t *testing.T) {
	req := Request{
		Method:  "POST",
		Path:    "/pets/7",
		Params:  map[string]string{"id": "7"},
		Query:   map[string]string{"q": `a"b`},
		Headers: map[string]string{"X-Trace": "t1"},
		Body:    map[string]any{"name": "rex", "tags": []any{"a", "b"}},
	}
	// Header names with '-' are not valid field names; templates use index.
	got := render(t, `{"id":{{.Request.Params.id}},"q":{{json .Request.Query.q}},"name":{{json .Request.Body.name}},"tag":{{jsonPath .Request.Body "$.tags[1]" | json}},"trace":"{{index .Request.Headers "X-Trace"}}","missing":{{.Request.Query.nope | default "none" | json}},"op":"{{.Operation}}","ex":"{{.Example}}"}`, req, 1)
	want := `{"id":7,"q":"a\"b","name":"rex","tag":"b","trace":"t1","missing":"none","op":"GET /pets/{id}","ex":"cat"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestClockHelpers(t *testing.T) {
	got := render(t, `{{.Timestamp}} {{.Unix}} {{.UnixMilli}} {{.Now.Format "2006-01-02"}}`, Request{}, 1)
	if got != "2025-01-02T03:04:05.000Z 1735787045 1735787045000 2025-01-02" {
		t.Fatalf("got %s", got)
	}
}

const randTpl = `{{.UUID}} {{.RandInt 1 6}} {{.RandFloat 0 1}} {{.RandBool}} {{.RandString 8}} {{.RandItem "a" "b" "c"}} {{.Fake.Name}} {{.Fake.Email}} {{.Fake.Sentence 3}} {{.Fake.Phone}} {{.Fake.Zip}}`

func TestDeterministicUnderSeed(t *testing.T) {
	a := render(t, randTpl, Request{}, 42, []byte("GET"), []byte("/pets/1"))
	b := render(t, randTpl, Request{}, 42, []byte("GET"), []byte("/pets/1"))
	if a != b {
		t.Fatalf("same seed and request differ:\n%s\n%s", a, b)
	}
	if c := render(t, randTpl, Request{}, 42, []byte("GET"), []byte("/pets/2")); c == a {
		t.Fatal("different requests rendered identically")
	}
	if c := render(t, randTpl, Request{}, 43, []byte("GET"), []byte("/pets/1")); c == a {
		t.Fatal("different seeds rendered identically")
	}
	if c := render(t, randTpl, Request{}, 42, []byte("GE"), []byte("T/pets/1")); c == a {
		t.Fatal("part boundaries must matter")
	}
	uuid := strings.Fields(a)[0]
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(uuid) {
		t.Fatalf("bad uuid %s", uuid)
	}
}

func TestUnseededVaries(t *testing.T) {
	if first, second := render(t, "{{.UUID}}", Request{}, 0), render(t, "{{.UUID}}", Request{}, 0); first == second {
		t.Fatal("unseeded renders must differ")
	}
}

func TestSharedTemplateConcurrent(t *testing.T) {
	tpl, err := Parse("t", randTpl)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := tpl.Execute(NewData(Request{}, "", "", NewRand(7, []byte("x")), frozen))
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 50 {
				got, err := tpl.Execute(NewData(Request{}, "", "", NewRand(7, []byte("x")), frozen))
				if err != nil || string(got) != string(want) {
					t.Errorf("concurrent render diverged: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestBounds(t *testing.T) {
	if got := render(t, `{{len (.RandString 100000)}}`, Request{}, 1); got != "4096" {
		t.Fatalf("RandString not capped: %s", got)
	}
	for i := range 200 {
		got := render(t, `{{.RandInt 6 1}}`, Request{}, int64(i+1))
		if got < "1" || got > "6" || len(got) != 1 {
			t.Fatalf("RandInt out of range: %s", got)
		}
	}
	tpl, _ := Parse("big", `{{range 5000}}{{$.RandString 4096}}{{end}}`)
	if _, err := tpl.Execute(NewData(Request{}, "", "", NewRand(1), frozen)); err == nil {
		t.Fatal("expected output limit error")
	}
}

func TestParseError(t *testing.T) {
	if _, err := Parse("bad", "{{ .Nope "); err == nil {
		t.Fatal("expected parse error")
	}
	if IsTemplate(`{"a":1}`) || !IsTemplate(`{{.UUID}}`) {
		t.Fatal("IsTemplate wrong")
	}
}
