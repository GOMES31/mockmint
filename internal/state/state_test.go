package state

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

func TestMemoryCRUD(t *testing.T) {
	m := NewMemory()
	v := map[string]any{"id": float64(1), "tags": []any{"a"}}
	if err := m.Put(ctx, "p", "notes", "1", v); err != nil {
		t.Fatal(err)
	}
	v["id"] = float64(99) // caller mutation must not leak into the store
	got, ok, err := m.Get(ctx, "p", "notes", "1")
	if err != nil || !ok || got.(map[string]any)["id"] != float64(1) {
		t.Fatalf("get = %v %v %v", got, ok, err)
	}
	got.(map[string]any)["tags"] = nil // nor mutation of a returned value
	again, _, _ := m.Get(ctx, "p", "notes", "1")
	if again.(map[string]any)["tags"] == nil {
		t.Fatal("returned value aliases the store")
	}
	if _, ok, _ := m.Get(ctx, "other", "notes", "1"); ok {
		t.Fatal("namespaces leak")
	}
	if ok, _ := m.Delete(ctx, "p", "notes", "1"); !ok {
		t.Fatal("delete existing = false")
	}
	if ok, _ := m.Delete(ctx, "p", "notes", "1"); ok {
		t.Fatal("delete missing = true")
	}
}

func TestMemoryListOrderAndClear(t *testing.T) {
	m := NewMemory()
	for _, k := range []string{"10", "2", "b", "a", "1"} {
		_ = m.Put(ctx, "p", "notes", k, k)
	}
	_ = m.Put(ctx, "p", "tags", "x", "x")
	entries, err := m.List(ctx, "p", "notes")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	if !reflect.DeepEqual(keys, []string{"1", "2", "10", "a", "b"}) {
		t.Fatalf("order = %v", keys)
	}
	if all, _ := m.List(ctx, "p", ""); len(all) != 6 || all[5].Collection != "tags" {
		t.Fatalf("namespace list = %+v", all)
	}
	_ = m.Clear(ctx, "p")
	if all, _ := m.List(ctx, "p", ""); len(all) != 0 {
		t.Fatal("not cleared")
	}
}

func TestMemoryLimits(t *testing.T) {
	m := NewMemory()
	if err := m.Put(ctx, "p", "c", "k", strings.Repeat("x", MaxValueBytes)); err == nil {
		t.Fatal("oversized value accepted")
	}
	if err := m.Put(ctx, "p", "c", "k", func() {}); err == nil {
		t.Fatal("non-JSON value accepted")
	}
	for i := range MaxKeysPerNamespace {
		if err := m.Put(ctx, "p", "c", strconv.Itoa(i), i); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Put(ctx, "p", "other", "new", 1); !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v", err)
	}
	if err := m.Put(ctx, "p", "c", "0", "overwrite"); err != nil {
		t.Fatalf("overwriting at the limit must work: %v", err)
	}
}

func TestMemoryConcurrent(t *testing.T) {
	m := NewMemory()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 200 {
				k := strconv.Itoa(w*1000 + i)
				_ = m.Put(ctx, "p", "c", k, i)
				_, _, _ = m.Get(ctx, "p", "c", k)
				_, _ = m.List(ctx, "p", "c")
				if i%3 == 0 {
					_, _ = m.Delete(ctx, "p", "c", k)
				}
			}
		})
	}
	wg.Wait()
	all, _ := m.List(ctx, "p", "c")
	if len(all) != 8*(200-67) {
		t.Fatalf("entries = %d", len(all))
	}
}

func TestMemorySeed(t *testing.T) {
	m := NewMemory()
	seed := map[string]map[string]any{"notes": {"1": "a", "2": "b"}}
	if err := m.Seed(ctx, "p", seed); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.List(ctx, "p", "notes"); len(got) != 2 {
		t.Fatalf("seeded %d entries, want 2", len(got))
	}
	// Emptying a seeded collection is a change requests made; seeding again
	// (a reload) must keep it empty.
	_, _ = m.Delete(ctx, "p", "notes", "1")
	_, _ = m.Delete(ctx, "p", "notes", "2")
	if err := m.Seed(ctx, "p", seed); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.List(ctx, "p", "notes"); len(got) != 0 {
		t.Fatalf("re-seeded an emptied collection: %+v", got)
	}
	// Clearing the namespace forgets that it was seeded.
	_ = m.Clear(ctx, "p")
	_ = m.Seed(ctx, "p", seed)
	if got, _ := m.List(ctx, "p", "notes"); len(got) != 2 {
		t.Fatalf("after clear seeded %d entries, want 2", len(got))
	}
	// A collection that requests wrote before it was ever seeded is kept.
	_ = m.Put(ctx, "p", "tags", "x", "mine")
	_ = m.Seed(ctx, "p", map[string]map[string]any{"tags": {"y": "seed"}})
	if got, _ := m.List(ctx, "p", "tags"); len(got) != 1 || got[0].Key != "x" {
		t.Fatalf("tags = %+v", got)
	}
}

func TestMemorySeedIsAllOrNothing(t *testing.T) {
	m := NewMemory()
	for i := range MaxKeysPerNamespace - 1 {
		_ = m.Put(ctx, "p", "filler", strconv.Itoa(i), i)
	}
	err := m.Seed(ctx, "p", map[string]map[string]any{"a": {"1": 1}, "b": {"1": 1}})
	if !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v, want ErrFull", err)
	}
	if got, _ := m.List(ctx, "p", "a"); len(got) != 0 {
		t.Fatalf("failed seed stored %+v", got)
	}
	// Nothing was marked seeded either: once there is room, it seeds.
	_ = m.Clear(ctx, "p")
	_ = m.Seed(ctx, "p", map[string]map[string]any{"a": {"1": 1}})
	if got, _ := m.List(ctx, "p", "a"); len(got) != 1 {
		t.Fatalf("a = %+v", got)
	}
	if err := m.Seed(ctx, "q", map[string]map[string]any{"big": {"1": strings.Repeat("x", MaxValueBytes)}}); err == nil {
		t.Fatal("oversized seed value accepted")
	}
}

func TestMemoryUpdate(t *testing.T) {
	m := NewMemory()
	if _, ok, err := m.Update(ctx, "p", "c", "missing", func(any) (any, error) {
		t.Fatal("fn called for a missing key")
		return nil, nil
	}); ok || err != nil {
		t.Fatalf("missing key: ok=%v err=%v", ok, err)
	}
	_ = m.Put(ctx, "p", "c", "k", map[string]any{})
	// Concurrent merges of different fields must all survive.
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			_, _, err := m.Update(ctx, "p", "c", "k", func(cur any) (any, error) {
				obj := cur.(map[string]any)
				obj["f"+strconv.Itoa(i)] = float64(i)
				return obj, nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, _, _ := m.Get(ctx, "p", "c", "k")
	if n := len(got.(map[string]any)); n != 50 {
		t.Fatalf("merged %d fields, want 50 (lost updates)", n)
	}
	wantErr := errors.New("boom")
	if _, _, err := m.Update(ctx, "p", "c", "k", func(any) (any, error) { return nil, wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v", err)
	}
}
