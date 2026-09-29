package jsonpath

import (
	"encoding/json"
	"testing"
)

func TestGet(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"a":{"b":[10,{"c":"x"},true]},"odd key":null,"n":1.5e2}`), &doc); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		expr string
		want string
		ok   bool
	}{
		{"$.a.b[0]", "10", true},
		{"$.a.b[1].c", "x", true},
		{"$['a'][\"b\"][-1]", "true", true},
		{"$['odd key']", "null", true},
		{"$.n", "150", true},
		{"$.a.b[3]", "", false},
		{"$.a.b[-4]", "", false},
		{"$.missing", "", false},
		{"$.a.b.c", "", false},
		{"$.a[0]", "", false},
	}
	for _, tt := range tests {
		v, ok := MustCompile(tt.expr).Get(doc)
		if ok != tt.ok {
			t.Errorf("%s: ok = %v", tt.expr, ok)
			continue
		}
		if !ok {
			continue
		}
		s, _ := Stringify(v)
		if s != tt.want {
			t.Errorf("%s = %q, want %q", tt.expr, s, tt.want)
		}
	}
	if v, ok := MustCompile("$").Get(doc); !ok || v == nil {
		t.Error("root not selected")
	}
	if _, ok := Stringify(map[string]any{}); ok {
		t.Error("objects must not stringify")
	}
}

func TestCompileErrors(t *testing.T) {
	for _, expr := range []string{"a.b", "$.", "$..a", "$.*", "$[*]", "$[1", "$x", "$[?(@.a)]"} {
		if _, err := Compile(expr); err == nil {
			t.Errorf("%q: expected error", expr)
		}
	}
}
