package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/pets/1", nil)
	Write(rec, req, New(TypeNotFound, http.StatusNotFound, "no route").With("method", "GET").With("status", 999))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ContentType {
		t.Fatalf("content-type = %q", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type":     TypeNotFound,
		"title":    "Not Found",
		"status":   float64(404), // standard member wins over the colliding extension
		"detail":   "no route",
		"instance": "/pets/1",
		"method":   "GET",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestMarshalOmitsEmptyAndDefaultsType(t *testing.T) {
	b, err := json.Marshal(&Detail{Status: 500})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"status":500,"type":"about:blank"}` {
		t.Fatalf("got %s", b)
	}
}
