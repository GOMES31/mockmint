// Package state stores mock state: JSON values addressed by namespace (a
// package), collection and key.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Limits of the in-memory store.
const (
	MaxKeysPerNamespace = 10_000
	MaxValueBytes       = 1 << 20
)

// ErrFull is returned when a namespace holds MaxKeysPerNamespace keys.
var ErrFull = errors.New("state namespace is full")

// Entry is a stored value.
type Entry struct {
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Value      any    `json:"value"`
}

// Store is a mock state backend. Values are JSON-compatible; stores keep
// their own copies, so callers may mutate what they pass in or get back.
type Store interface {
	Get(ctx context.Context, ns, collection, key string) (any, bool, error)
	Put(ctx context.Context, ns, collection, key string, value any) error
	Delete(ctx context.Context, ns, collection, key string) (bool, error)
	// List returns a collection's entries sorted by key; collection ""
	// lists the whole namespace.
	List(ctx context.Context, ns, collection string) ([]Entry, error)
	// Update atomically replaces an existing value with fn(current). It
	// reports false, without calling fn, when the key is missing. fn must
	// not call the store.
	Update(ctx context.Context, ns, collection, key string, fn func(current any) (any, error)) (any, bool, error)
	// Seed stores initial entries in each collection that has not been
	// seeded since the namespace was last cleared and is empty, so reloads
	// keep what requests changed (including emptied collections). It stores
	// all of them or none.
	Seed(ctx context.Context, ns string, collections map[string]map[string]any) error
	// Clear removes a namespace and forgets that it was seeded.
	Clear(ctx context.Context, ns string) error
}

// Memory is an in-memory Store. The zero value is not usable; use NewMemory.
type Memory struct {
	mu     sync.RWMutex
	nss    map[string]map[string]map[string][]byte // ns → collection → key → JSON
	seeded map[string]map[string]bool              // ns → collections Seed has handled
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{nss: map[string]map[string]map[string][]byte{}, seeded: map[string]map[string]bool{}}
}

var _ Store = (*Memory)(nil)

// Get returns a copy of the value.
func (m *Memory) Get(_ context.Context, ns, collection, key string) (any, bool, error) {
	m.mu.RLock()
	raw, ok := m.nss[ns][collection][key]
	m.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	v, err := decode(raw)
	return v, err == nil, err
}

// Put stores a copy of value.
func (m *Memory) Put(_ context.Context, ns, collection, key string, value any) error {
	raw, err := encode(value)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.put(ns, collection, key, raw)
}

// put stores raw. m.mu must be held.
func (m *Memory) put(ns, collection, key string, raw []byte) error {
	cols := m.nss[ns]
	if cols == nil {
		cols = map[string]map[string][]byte{}
		m.nss[ns] = cols
	}
	keys := cols[collection]
	if keys == nil {
		keys = map[string][]byte{}
		cols[collection] = keys
	}
	if _, exists := keys[key]; !exists && count(cols) >= MaxKeysPerNamespace {
		return ErrFull
	}
	keys[key] = raw
	return nil
}

// Delete removes a key and reports whether it existed.
func (m *Memory) Delete(_ context.Context, ns, collection, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := m.nss[ns][collection]
	if _, ok := keys[key]; !ok {
		return false, nil
	}
	delete(keys, key)
	if len(keys) == 0 {
		delete(m.nss[ns], collection)
	}
	return true, nil
}

// List returns copies of a collection's (or namespace's) entries.
func (m *Memory) List(_ context.Context, ns, collection string) ([]Entry, error) {
	m.mu.RLock()
	var out []Entry
	var errs []error
	for col, keys := range m.nss[ns] {
		if collection != "" && col != collection {
			continue
		}
		for k, raw := range keys {
			v, err := decode(raw)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out = append(out, Entry{Collection: col, Key: k, Value: v})
		}
	}
	m.mu.RUnlock()
	slices.SortFunc(out, func(a, b Entry) int {
		if a.Collection != b.Collection {
			return compareKeys(a.Collection, b.Collection)
		}
		return compareKeys(a.Key, b.Key)
	})
	return out, errors.Join(errs...)
}

// Update replaces an existing value with fn(current) under the store lock.
func (m *Memory) Update(_ context.Context, ns, collection, key string, fn func(current any) (any, error)) (any, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.nss[ns][collection][key]
	if !ok {
		return nil, false, nil
	}
	cur, err := decode(raw)
	if err != nil {
		return nil, true, err
	}
	next, err := fn(cur)
	if err != nil {
		return nil, true, err
	}
	if raw, err = encode(next); err != nil {
		return nil, true, err
	}
	m.nss[ns][collection][key] = raw
	return next, true, nil
}

// Seed stores initial entries; see Store.
func (m *Memory) Seed(_ context.Context, ns string, collections map[string]map[string]any) error {
	encoded := map[string]map[string][]byte{}
	for col, entries := range collections {
		encoded[col] = make(map[string][]byte, len(entries))
		for key, v := range entries {
			raw, err := encode(v)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", col, key, err)
			}
			encoded[col][key] = raw
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seeded := m.seeded[ns]
	add := 0
	for col, entries := range encoded {
		if seeded[col] || len(m.nss[ns][col]) > 0 {
			delete(encoded, col)
			continue
		}
		add += len(entries)
	}
	if count(m.nss[ns])+add > MaxKeysPerNamespace {
		return ErrFull
	}
	if seeded == nil {
		seeded = map[string]bool{}
		m.seeded[ns] = seeded
	}
	for col := range collections {
		seeded[col] = true
	}
	for col, entries := range encoded {
		for key, raw := range entries {
			if err := m.put(ns, col, key, raw); err != nil {
				return err // unreachable: capacity was checked above
			}
		}
	}
	return nil
}

// Clear removes a namespace.
func (m *Memory) Clear(_ context.Context, ns string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nss, ns)
	delete(m.seeded, ns)
	return nil
}

func count(cols map[string]map[string][]byte) int {
	n := 0
	for _, keys := range cols {
		n += len(keys)
	}
	return n
}

func encode(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("state value is not JSON: %w", err)
	}
	if len(raw) > MaxValueBytes {
		return nil, fmt.Errorf("state value is %d bytes; the limit is %d", len(raw), MaxValueBytes)
	}
	return raw, nil
}

func decode(raw []byte) (any, error) {
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}

// compareKeys orders numeric keys numerically ("2" before "10"), then
// everything else lexically, so REST-style ids list naturally.
func compareKeys(a, b string) int {
	na, aNum := numeric(a)
	nb, bNum := numeric(b)
	switch {
	case aNum && bNum:
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
		return 0
	case aNum:
		return -1
	case bNum:
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func numeric(s string) (uint64, bool) {
	if s == "" || len(s) > 19 {
		return 0, false
	}
	var n uint64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}
