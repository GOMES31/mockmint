package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/state"
	"github.com/mockmint/mockmint/internal/template"
)

// errStateMissing means a read/update/delete key is not in the store.
var errStateMissing = errors.New("not found in state")

// stateCtx carries a stateful operation through one request.
type stateCtx struct {
	op    *pkg.StateOp
	ns    string
	store state.Store
	key   string
	value any // resolved entry for read/update
	found bool
}

// stateReader adapts a Store to template.StateReader for one package.
type stateReader struct {
	ctx   context.Context
	store state.Store
	ns    string
}

func (s stateReader) Get(collection, key string) (any, bool) {
	v, ok, err := s.store.Get(s.ctx, s.ns, collection, key)
	return v, ok && err == nil
}

func (s stateReader) List(collection string) []any {
	entries, err := s.store.List(s.ctx, s.ns, collection)
	if err != nil {
		return nil
	}
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = e.Value
	}
	return out
}

// resolve evaluates the key from the request and, for read/update/delete,
// looks it up. It returns errStateMissing when the entry does not exist.
func (sc *stateCtx) resolve(ctx context.Context, data *template.Data) error {
	if sc.op.Action == pkg.StateList || sc.op.Action == pkg.StateCreate {
		return nil
	}
	key, err := renderKey(sc.op, data)
	if err != nil {
		return err
	}
	sc.key = key
	sc.value, sc.found, err = sc.store.Get(ctx, sc.ns, sc.op.Collection, key)
	if err != nil {
		return err
	}
	if !sc.found {
		return errStateMissing
	}
	return nil
}

// apply performs the state change for a 2xx response and returns the body
// to send instead of the rendered one (nil keeps the rendered body).
func (sc *stateCtx) apply(ctx context.Context, data *template.Data, rendered []byte) ([]byte, error) {
	switch sc.op.Action {
	case pkg.StateCreate:
		var resp any
		if err := json.Unmarshal(rendered, &resp); err != nil {
			return nil, fmt.Errorf("state create: the example body must be JSON: %w", err)
		}
		data.Response = resp
		key, err := renderKey(sc.op, data)
		if err != nil {
			return nil, err
		}
		return nil, sc.store.Put(ctx, sc.ns, sc.op.Collection, key, resp)
	case pkg.StateRead:
		return json.Marshal(sc.value)
	case pkg.StateUpdate:
		// Read-modify-write in one step, so concurrent merges keep each
		// other's fields.
		next, found, err := sc.store.Update(ctx, sc.ns, sc.op.Collection, sc.key, func(current any) (any, error) {
			sc.value = current
			return sc.updated(data, rendered)
		})
		if err != nil {
			return nil, err
		}
		if !found { // deleted since resolve
			return nil, errStateMissing
		}
		return json.Marshal(next)
	case pkg.StateDelete:
		_, err := sc.store.Delete(ctx, sc.ns, sc.op.Collection, sc.key)
		return nil, err
	case pkg.StateList:
		entries, err := sc.store.List(ctx, sc.ns, sc.op.Collection)
		if err != nil {
			return nil, err
		}
		want := make([]string, len(sc.op.Where))
		for i, w := range sc.op.Where {
			b, err := w.Value.Execute(data)
			if err != nil {
				return nil, fmt.Errorf("state where %s: %w", w.Field, err)
			}
			want[i] = strings.TrimSpace(string(b))
		}
		values := make([]any, 0, len(entries))
		for _, e := range entries {
			if matchesWhere(e.Value, sc.op.Where, want) {
				values = append(values, e.Value)
			}
		}
		return json.Marshal(values)
	}
	return nil, nil
}

func (sc *stateCtx) updated(data *template.Data, rendered []byte) (any, error) {
	switch sc.op.Value {
	case pkg.UpdateRequest:
		if data.Request.Body == nil {
			return nil, errors.New("state update: the request body must be JSON")
		}
		return data.Request.Body, nil
	case pkg.UpdateResponse:
		var v any
		if err := json.Unmarshal(rendered, &v); err != nil {
			return nil, fmt.Errorf("state update: the example body must be JSON: %w", err)
		}
		return v, nil
	default: // merge
		patch, ok := data.Request.Body.(map[string]any)
		if !ok {
			return nil, errors.New("state update: merge needs a JSON object request body")
		}
		base, ok := sc.value.(map[string]any)
		if !ok {
			return patch, nil
		}
		merged := make(map[string]any, len(base)+len(patch))
		for k, v := range base {
			merged[k] = v
		}
		for k, v := range patch {
			merged[k] = v
		}
		return merged, nil
	}
}

// matchesWhere keeps entries whose fields equal every non-empty filter.
func matchesWhere(v any, where []pkg.WhereClause, want []string) bool {
	obj, _ := v.(map[string]any)
	for i, w := range where {
		if want[i] == "" || want[i] == "<no value>" {
			continue
		}
		field, ok := obj[w.Field]
		if !ok || fmt.Sprint(field) != want[i] {
			return false
		}
	}
	return true
}

func renderKey(op *pkg.StateOp, data *template.Data) (string, error) {
	b, err := op.Key.Execute(data)
	if err != nil {
		return "", fmt.Errorf("state key: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" || key == "<no value>" {
		return "", errors.New("state key rendered empty")
	}
	return key, nil
}
