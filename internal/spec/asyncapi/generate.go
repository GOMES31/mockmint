package asyncapi

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

// Synthesis limits, matching the OpenAPI generator: a cap per array and
// string, and a node budget for the whole value (nested arrays multiply).
const (
	maxDepth     = 6
	maxGenItems  = 100
	maxGenLength = 10_000
	maxGenNodes  = 10_000
)

// ErrSchemaTooLarge is returned when a synthesized value would exceed the
// node budget. Authors should provide an example instead.
var ErrSchemaTooLarge = fmt.Errorf("schema too large to synthesize (over %d values); provide an example", maxGenNodes)

var genEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// Generate synthesizes a JSON-compatible value satisfying the schema,
// drawing randomness only from rng.
func (s *Schema) Generate(rng *rand.Rand) (any, error) {
	g := &generator{rng: rng, docs: s.docs, budget: maxGenNodes, active: map[string]bool{}}
	v, err := g.value(s.node, s.at, 0)
	if err != nil {
		return nil, err
	}
	if g.budget < 0 {
		return nil, ErrSchemaTooLarge
	}
	return normalize(v), nil
}

type generator struct {
	rng    *rand.Rand
	docs   *docSet
	budget int
	active map[string]bool // schemas on the current path, to cut $ref cycles
}

func (g *generator) value(n *yaml.Node, at loc, depth int) (any, error) {
	if g.budget--; g.budget < 0 || depth > maxDepth {
		return nil, nil
	}
	n, at, err := g.docs.resolve(n, at)
	if err != nil {
		return nil, err
	}
	if n.Kind == yaml.ScalarNode { // boolean schema
		return nil, nil
	}
	key := at.String()
	if g.active[key] {
		return nil, nil // cycle
	}
	g.active[key] = true
	defer delete(g.active, key)

	for _, k := range []string{"const", "example", "default"} {
		if v := get(n, k); v != nil {
			return decodeAny(v)
		}
	}
	if ex := get(n, "examples"); ex != nil && ex.Kind == yaml.SequenceNode && len(ex.Content) > 0 {
		return decodeAny(ex.Content[0])
	}
	if en := get(n, "enum"); en != nil && en.Kind == yaml.SequenceNode && len(en.Content) > 0 {
		return decodeAny(en.Content[g.rng.IntN(len(en.Content))])
	}
	if all := get(n, "allOf"); all != nil && all.Kind == yaml.SequenceNode {
		return g.allOf(n, all, at, depth)
	}
	for _, k := range []string{"oneOf", "anyOf"} {
		if alt := get(n, k); alt != nil && alt.Kind == yaml.SequenceNode && len(alt.Content) > 0 {
			return g.value(alt.Content[0], at.child(k).index(0), depth+1)
		}
	}

	switch typeOf(n) {
	case "object":
		return g.object(n, at, depth)
	case "array":
		return g.array(n, at, depth)
	case "string":
		return g.str(n), nil
	case "integer":
		return g.integer(n), nil
	case "number":
		return g.number(n), nil
	case "boolean":
		return g.rng.IntN(2) == 1, nil
	}
	return nil, nil
}

func typeOf(n *yaml.Node) string {
	t := get(n, "type")
	switch {
	case t != nil && t.Kind == yaml.ScalarNode:
		return t.Value
	case t != nil && t.Kind == yaml.SequenceNode:
		for _, c := range t.Content {
			if c.Value != "null" {
				return c.Value
			}
		}
		return "null"
	case get(n, "properties") != nil:
		return "object"
	case get(n, "items") != nil:
		return "array"
	}
	return "object"
}

func (g *generator) allOf(n, all *yaml.Node, at loc, depth int) (any, error) {
	merged := map[string]any{}
	var last any
	for i, part := range all.Content {
		v, err := g.value(part, at.child("allOf").index(i), depth+1)
		if err != nil {
			return nil, err
		}
		if m, ok := v.(map[string]any); ok {
			for k, val := range m {
				merged[k] = val
			}
		} else if v != nil {
			last = v
		}
	}
	if get(n, "properties") != nil {
		v, err := g.object(n, at, depth)
		if err != nil {
			return nil, err
		}
		for k, val := range v.(map[string]any) {
			merged[k] = val
		}
	}
	if len(merged) == 0 && last != nil {
		return last, nil
	}
	return merged, nil
}

func (g *generator) object(n *yaml.Node, at loc, depth int) (any, error) {
	out := map[string]any{}
	props := get(n, "properties")
	var required []string
	if r := get(n, "required"); r != nil {
		for _, c := range r.Content {
			required = append(required, c.Value)
		}
	}
	names := keys(props)
	slices.Sort(names)
	for _, name := range names {
		if g.budget < 0 {
			break
		}
		v, err := g.value(get(props, name), at.child("properties").child(name), depth+1)
		if err != nil {
			return nil, err
		}
		if v == nil && !slices.Contains(required, name) {
			continue
		}
		out[name] = v
	}
	return out, nil
}

func (g *generator) array(n *yaml.Node, at loc, depth int) (any, error) {
	count := max(min(uintOf(n, "minItems", 0), maxGenItems), 1)
	if get(n, "maxItems") != nil {
		count = min(count, min(uintOf(n, "maxItems", 0), maxGenItems))
	}
	items := get(n, "items")
	out := make([]any, 0, count)
	for i := range count {
		if items == nil {
			out = append(out, g.word())
			continue
		}
		item, iat := items, at.child("items")
		if items.Kind == yaml.SequenceNode { // draft-07 tuple form
			if i >= len(items.Content) {
				break
			}
			item, iat = items.Content[i], iat.index(i)
		}
		v, err := g.value(item, iat, depth+1)
		if err != nil {
			return nil, err
		}
		if v == nil || g.budget < 0 {
			break
		}
		out = append(out, v)
	}
	return out, nil
}

var loremWords = []string{"lorem", "ipsum", "dolor", "sit", "amet", "consectetur", "adipiscing", "elit", "sed", "do", "eiusmod", "tempor"}

func (g *generator) word() string { return loremWords[g.rng.IntN(len(loremWords))] }

func (g *generator) str(n *yaml.Node) string {
	format := ""
	if f := get(n, "format"); f != nil {
		format = f.Value
	}
	switch format {
	case "uuid":
		var b [16]byte
		binary.LittleEndian.PutUint64(b[:8], g.rng.Uint64())
		binary.LittleEndian.PutUint64(b[8:], g.rng.Uint64())
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		h := hex.EncodeToString(b[:])
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	case "date-time":
		return genEpoch.Add(time.Duration(g.rng.IntN(365*24)) * time.Hour).Format(time.RFC3339)
	case "date":
		return genEpoch.AddDate(0, 0, g.rng.IntN(365)).Format(time.DateOnly)
	case "email":
		return g.word() + "@example.com"
	case "uri", "url":
		return "https://example.com/" + g.word()
	}
	v := g.word()
	minLen := min(uintOf(n, "minLength", 0), maxGenLength)
	for len(v) < minLen {
		v += " " + g.word()
	}
	if get(n, "maxLength") != nil {
		if maxLen := min(uintOf(n, "maxLength", 0), maxGenLength); len(v) > maxLen {
			v = v[:maxLen]
		}
	}
	return v
}

func (g *generator) integer(n *yaml.Node) int64 {
	lo, hi := int64(1), int64(1000)
	if v, ok := floatOf(n, "minimum"); ok {
		lo = int64(math.Ceil(v))
		hi = max(hi, lo+1000)
	}
	if v, ok := floatOf(n, "exclusiveMinimum"); ok {
		lo = int64(math.Floor(v)) + 1
		hi = max(hi, lo+1000)
	}
	if v, ok := floatOf(n, "maximum"); ok {
		hi = int64(math.Floor(v))
	}
	if v, ok := floatOf(n, "exclusiveMaximum"); ok {
		hi = int64(math.Ceil(v)) - 1
	}
	if hi < lo {
		return lo
	}
	return lo + g.rng.Int64N(hi-lo+1)
}

func (g *generator) number(n *yaml.Node) float64 {
	lo, hi := 0.0, 1000.0
	if v, ok := floatOf(n, "minimum"); ok {
		lo, hi = v, math.Max(hi, v+1000)
	}
	if v, ok := floatOf(n, "maximum"); ok {
		hi = v
	}
	v := math.Round((lo+g.rng.Float64()*(hi-lo))*100) / 100
	if v < lo || v > hi {
		v = lo + (hi-lo)/2
	}
	return v
}

func uintOf(n *yaml.Node, key string, def int) int {
	v := get(n, key)
	if v == nil {
		return def
	}
	i, err := strconv.ParseUint(v.Value, 10, 31)
	if err != nil {
		return maxGenNodes // absurd or malformed sizes hit the caps
	}
	return int(i)
}

func floatOf(n *yaml.Node, key string) (float64, bool) {
	v := get(n, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return 0, false
	}
	f, err := strconv.ParseFloat(v.Value, 64)
	return f, err == nil
}

func decodeAny(n *yaml.Node) (any, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, errors.New("decode schema value: " + err.Error())
	}
	return normalize(v), nil
}
