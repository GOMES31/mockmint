package openapi

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// maxDepth stops recursion through self-referencing schemas.
const maxDepth = 6

// Caps on schema-declared sizes, so a spec cannot make synthesis allocate
// without bound (minItems: 1e12). maxGenNodes bounds the whole value, since
// per-array caps multiply through nested arrays (100^6).
const (
	maxGenItems  = 100
	maxGenLength = 10_000
	maxGenNodes  = 10_000
)

// ErrSchemaTooLarge is returned when a synthesized value would exceed
// maxGenNodes. Authors should provide an explicit example instead.
var ErrSchemaTooLarge = fmt.Errorf("schema too large to synthesize (over %d values); provide an example", maxGenNodes)

// capped converts a schema size to int, clamped to limit (limit >= 0).
func capped(v uint64, limit int) int {
	if v > uint64(limit) { //nolint:gosec // G115: limit is a small non-negative constant
		return limit
	}
	return int(v) //nolint:gosec // G115: v <= limit here
}

// genEpoch anchors generated dates so output does not depend on wall time.
var genEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// Generate synthesizes a value satisfying schema. It prefers declared
// const/example/examples/default/enum values; otherwise it builds a value
// from the type and constraints, drawing randomness only from rng. The same
// schema and rng state always yield the same value. A nil schema yields an
// empty object. It fails with ErrSchemaTooLarge instead of building huge
// values.
func Generate(schema *openapi3.Schema, rng *rand.Rand) (any, error) {
	g := &generator{rng: rng, seen: map[*openapi3.Schema]int{}, budget: maxGenNodes}
	if schema == nil {
		return map[string]any{}, nil
	}
	v := g.value(schema, 0)
	if g.budget < 0 {
		return nil, ErrSchemaTooLarge
	}
	return normalizeJSON(v), nil
}

type generator struct {
	rng    *rand.Rand
	seen   map[*openapi3.Schema]int
	budget int // remaining values; negative once exhausted
}

func (g *generator) value(s *openapi3.Schema, depth int) any {
	if s == nil {
		return nil
	}
	if g.budget--; g.budget < 0 {
		return nil // exhausted: unwind quickly; Generate reports the error
	}
	switch {
	case s.Const != nil:
		return s.Const
	case s.Example != nil:
		return s.Example
	case len(s.Examples) > 0:
		return s.Examples[0]
	case s.Default != nil:
		return s.Default
	case len(s.Enum) > 0:
		return s.Enum[g.rng.IntN(len(s.Enum))]
	}
	if g.seen[s] > 0 && depth > 0 {
		return nil // cycle: stop here; nullable-or-optional in practice
	}
	g.seen[s]++
	defer func() { g.seen[s]-- }()
	if depth > maxDepth {
		return nil
	}

	if len(s.AllOf) > 0 {
		return g.allOf(s, depth)
	}
	if len(s.OneOf) > 0 {
		return g.value(s.OneOf[0].Value, depth+1)
	}
	if len(s.AnyOf) > 0 {
		return g.value(s.AnyOf[0].Value, depth+1)
	}

	switch typeOf(s) {
	case openapi3.TypeObject:
		return g.object(s, depth)
	case openapi3.TypeArray:
		return g.array(s, depth)
	case openapi3.TypeString:
		return g.str(s)
	case openapi3.TypeInteger:
		return g.integer(s)
	case openapi3.TypeNumber:
		return g.number(s)
	case openapi3.TypeBoolean:
		return g.rng.IntN(2) == 1
	case openapi3.TypeNull:
		return nil
	}
	return nil
}

// typeOf returns the schema's primary type, inferring object/array from
// properties/items when the type is omitted, and skipping "null" in 3.1
// type arrays.
func typeOf(s *openapi3.Schema) string {
	if s.Type != nil {
		for _, t := range s.Type.Slice() {
			if t != openapi3.TypeNull {
				return t
			}
		}
		if s.Type.IncludesNull() {
			return openapi3.TypeNull
		}
	}
	switch {
	case len(s.Properties) > 0 || s.AdditionalProperties.Schema != nil:
		return openapi3.TypeObject
	case s.Items != nil:
		return openapi3.TypeArray
	}
	return openapi3.TypeObject
}

func (g *generator) allOf(s *openapi3.Schema, depth int) any {
	merged := map[string]any{}
	var last any
	for _, ref := range s.AllOf {
		v := g.value(ref.Value, depth+1)
		if m, ok := v.(map[string]any); ok {
			for k, val := range m {
				merged[k] = val
			}
		} else if v != nil {
			last = v
		}
	}
	if len(s.Properties) > 0 {
		for k, v := range g.object(s, depth).(map[string]any) {
			merged[k] = v
		}
	}
	if len(merged) == 0 && last != nil {
		return last
	}
	return merged
}

func (g *generator) object(s *openapi3.Schema, depth int) any {
	out := map[string]any{}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if g.budget < 0 {
			break
		}
		ref := s.Properties[n]
		if ref == nil || ref.Value == nil || ref.Value.WriteOnly {
			continue
		}
		v := g.value(ref.Value, depth+1)
		if v == nil && !slices.Contains(s.Required, n) {
			continue
		}
		out[n] = v
	}
	return out
}

func (g *generator) array(s *openapi3.Schema, depth int) any {
	n := max(capped(s.MinItems, maxGenItems), 1)
	if s.MaxItems != nil {
		n = min(n, capped(*s.MaxItems, maxGenItems))
	}
	out := make([]any, 0, n)
	for i := range n {
		var item *openapi3.Schema
		switch {
		case i < len(s.PrefixItems):
			item = s.PrefixItems[i].Value
		case s.Items != nil:
			item = s.Items.Value
		}
		if item == nil {
			out = append(out, g.word())
			continue
		}
		v := g.value(item, depth+1)
		if v == nil || g.budget < 0 {
			break
		}
		out = append(out, v)
	}
	return out
}

var loremWords = []string{"lorem", "ipsum", "dolor", "sit", "amet", "consectetur", "adipiscing", "elit", "sed", "do", "eiusmod", "tempor"}

func (g *generator) word() string { return loremWords[g.rng.IntN(len(loremWords))] }

func (g *generator) str(s *openapi3.Schema) any {
	var v string
	switch strings.ToLower(s.Format) {
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
	case "time":
		return fmt.Sprintf("%02d:%02d:00Z", g.rng.IntN(24), g.rng.IntN(60))
	case "email":
		return g.word() + "@example.com"
	case "uri", "url":
		return "https://example.com/" + g.word()
	case "hostname":
		return g.word() + ".example.com"
	case "ipv4":
		return fmt.Sprintf("192.0.2.%d", 1+g.rng.IntN(254))
	case "ipv6":
		return fmt.Sprintf("2001:db8::%x", 1+g.rng.IntN(0xfffe))
	case "byte":
		return "bW9ja21pbnQ=" // "mockmint"
	default:
		v = g.word()
	}
	minLen := capped(s.MinLength, maxGenLength)
	var sb strings.Builder
	sb.WriteString(v)
	for sb.Len() < minLen {
		sb.WriteString(" " + g.word())
	}
	v = sb.String()
	if s.MaxLength != nil {
		if maxLen := capped(*s.MaxLength, maxGenLength); len(v) > maxLen {
			v = v[:maxLen]
		}
	}
	return v
}

// bounds returns the inclusive-or-exclusive numeric range for s and whether
// each end is exclusive. OpenAPI 3.0 marks minimum/maximum exclusive with a
// boolean; 3.1 gives the exclusive bound itself as a number.
func bounds(s *openapi3.Schema, defLo, defHi float64) (lo, hi float64, exLo, exHi bool) {
	lo, hi = defLo, defHi
	hasLo, hasHi := false, false
	if s.Min != nil {
		lo, hasLo, exLo = *s.Min, true, s.ExclusiveMin.IsTrue()
	}
	if v := s.ExclusiveMin.Value; v != nil && (!hasLo || *v >= lo) {
		lo, hasLo, exLo = *v, true, true
	}
	if s.Max != nil {
		hi, hasHi, exHi = *s.Max, true, s.ExclusiveMax.IsTrue()
	}
	if v := s.ExclusiveMax.Value; v != nil && (!hasHi || *v <= hi) {
		hi, hasHi, exHi = *v, true, true
	}
	switch {
	case hasLo && !hasHi && hi < lo:
		hi = lo + defHi
	case hasHi && !hasLo && lo > hi:
		lo = hi - defHi
	}
	return lo, hi, exLo, exHi
}

func (g *generator) integer(s *openapi3.Schema) any {
	lo, hi, exLo, exHi := bounds(s, 1, 1000)
	ilo, ihi := int64(math.Ceil(lo)), int64(math.Floor(hi))
	if exLo && float64(ilo) == lo {
		ilo++
	}
	if exHi && float64(ihi) == hi {
		ihi--
	}
	if ihi < ilo {
		return ilo
	}
	v := ilo + g.rng.Int64N(ihi-ilo+1)
	if s.MultipleOf != nil && *s.MultipleOf >= 1 && *s.MultipleOf == math.Trunc(*s.MultipleOf) {
		m := int64(*s.MultipleOf)
		// Smallest multiple >= ilo, then a random multiple within range.
		first := ilo + (m-((ilo%m)+m)%m)%m
		if first <= ihi {
			v = first + m*g.rng.Int64N((ihi-first)/m+1)
		}
	}
	return v
}

func (g *generator) number(s *openapi3.Schema) any {
	lo, hi, exLo, exHi := bounds(s, 0, 1000)
	v := math.Round((lo+g.rng.Float64()*(hi-lo))*100) / 100
	// Rounding can land on an exclusive bound or outside the range; pull
	// back to the midpoint, which is strictly inside any non-empty range.
	if v < lo || v > hi || (exLo && v == lo) || (exHi && v == hi) {
		v = lo + (hi-lo)/2
	}
	return v
}
