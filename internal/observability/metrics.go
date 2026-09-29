package observability

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is a minimal Prometheus registry writing the text exposition
// format (version 0.0.4). It supports counters, gauges and histograms with a
// fixed set of label names per family.
type Registry struct {
	mu       sync.Mutex
	families []family
}

type family interface {
	name() string
	write(w *bufio.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) register(f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.families {
		if existing.name() == f.name() {
			panic("observability: duplicate metric " + f.name())
		}
	}
	r.families = append(r.families, f)
}

// WriteText writes all metrics in the Prometheus text format, families in
// registration order and series sorted by labels, plus Go runtime gauges.
func (r *Registry) WriteText(w io.Writer) error {
	bw := bufio.NewWriter(w)
	r.mu.Lock()
	fams := slices.Clone(r.families)
	r.mu.Unlock()
	for _, f := range fams {
		f.write(bw)
	}
	writeRuntime(bw)
	return bw.Flush()
}

// ContentType is the exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

type vec[T any] struct {
	fname, help, kind string
	labels            []string
	mu                sync.RWMutex
	series            map[string]*T
	values            map[string][]string
	newT              func() *T
}

func (v *vec[T]) with(values []string) *T {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("observability: %s wants %d label values, got %d", v.fname, len(v.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	v.mu.RLock()
	s, ok := v.series[key]
	v.mu.RUnlock()
	if ok {
		return s
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok = v.series[key]; !ok {
		s = v.newT()
		v.series[key] = s
		v.values[key] = slices.Clone(values)
	}
	return s
}

// sorted returns series keys in order, for stable output.
func (v *vec[T]) sorted() ([]string, map[string]*T, map[string][]string) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	keys := make([]string, 0, len(v.series))
	series := make(map[string]*T, len(v.series))
	values := make(map[string][]string, len(v.values))
	for k, s := range v.series {
		keys = append(keys, k)
		series[k], values[k] = s, v.values[k]
	}
	slices.Sort(keys)
	return keys, series, values
}

func (v *vec[T]) header(w *bufio.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.fname, escapeHelp(v.help), v.fname, v.kind)
}

func labelString(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n + `="` + escapeLabel(values[i]) + `"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(extra[i] + `="` + escapeLabel(extra[i+1]) + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// --- counter -----------------------------------------------------------------

// CounterVec is a family of monotonically increasing counters.
type CounterVec struct{ v *vec[atomic.Uint64] }

// NewCounterVec registers a counter family.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{v: &vec[atomic.Uint64]{fname: name, help: help, kind: "counter", labels: labels,
		series: map[string]*atomic.Uint64{}, values: map[string][]string{}, newT: func() *atomic.Uint64 { return new(atomic.Uint64) }}}
	r.register(c)
	return c
}

// Inc adds one to the series with the given label values.
func (c *CounterVec) Inc(labelValues ...string) { c.v.with(labelValues).Add(1) }

// Value returns a series' current value (for tests and the admin API).
func (c *CounterVec) Value(labelValues ...string) uint64 { return c.v.with(labelValues).Load() }

func (c *CounterVec) name() string { return c.v.fname }

func (c *CounterVec) write(w *bufio.Writer) {
	c.v.header(w)
	keys, series, values := c.v.sorted()
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %d\n", c.v.fname, labelString(c.v.labels, values[k]), series[k].Load())
	}
}

// --- gauge -------------------------------------------------------------------

// GaugeVec is a family of values that go up and down.
type GaugeVec struct{ v *vec[atomic.Uint64] } // float64 bits

// NewGaugeVec registers a gauge family.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	g := &GaugeVec{v: &vec[atomic.Uint64]{fname: name, help: help, kind: "gauge", labels: labels,
		series: map[string]*atomic.Uint64{}, values: map[string][]string{}, newT: func() *atomic.Uint64 { return new(atomic.Uint64) }}}
	r.register(g)
	return g
}

// Set sets the series with the given label values.
func (g *GaugeVec) Set(v float64, labelValues ...string) {
	g.v.with(labelValues).Store(math.Float64bits(v))
}

func (g *GaugeVec) name() string { return g.v.fname }

func (g *GaugeVec) write(w *bufio.Writer) {
	g.v.header(w)
	keys, series, values := g.v.sorted()
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", g.v.fname, labelString(g.v.labels, values[k]), formatFloat(math.Float64frombits(series[k].Load())))
	}
}

// --- histogram ---------------------------------------------------------------

type histogram struct {
	mu     sync.Mutex
	counts []uint64 // per bucket, non-cumulative; last is +Inf
	sum    float64
	count  uint64
}

// HistogramVec is a family of histograms sharing bucket bounds.
type HistogramVec struct {
	v       *vec[histogram]
	buckets []float64
}

// DefBuckets suit request latencies in seconds, from 1 ms to 10 s.
var DefBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// NewHistogramVec registers a histogram family with ascending buckets.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	if !slices.IsSorted(buckets) {
		panic("observability: buckets must be ascending: " + name)
	}
	b := slices.Clone(buckets)
	h := &HistogramVec{buckets: b, v: &vec[histogram]{fname: name, help: help, kind: "histogram", labels: labels,
		series: map[string]*histogram{}, values: map[string][]string{},
		newT: func() *histogram { return &histogram{counts: make([]uint64, len(b)+1)} }}}
	r.register(h)
	return h
}

// Observe records v in the series with the given label values.
func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	s := h.v.with(labelValues)
	i, _ := slices.BinarySearch(h.buckets, v) // first bucket with bound >= v
	s.mu.Lock()
	s.counts[i]++
	s.sum += v
	s.count++
	s.mu.Unlock()
}

func (h *HistogramVec) name() string { return h.v.fname }

func (h *HistogramVec) write(w *bufio.Writer) {
	h.v.header(w)
	keys, series, values := h.v.sorted()
	for _, k := range keys {
		s := series[k]
		s.mu.Lock()
		counts, sum, count := slices.Clone(s.counts), s.sum, s.count
		s.mu.Unlock()
		var cum uint64
		for i, bound := range h.buckets {
			cum += counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.v.fname, labelString(h.v.labels, values[k], "le", formatFloat(bound)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.v.fname, labelString(h.v.labels, values[k], "le", "+Inf"), count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.v.fname, labelString(h.v.labels, values[k]), formatFloat(sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.v.fname, labelString(h.v.labels, values[k]), count)
	}
}

func writeRuntime(w *bufio.Writer) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	for _, g := range []struct {
		name, help string
		v          float64
	}{
		{"go_goroutines", "Number of goroutines.", float64(runtime.NumGoroutine())},
		{"go_memstats_heap_inuse_bytes", "Heap bytes in use.", float64(ms.HeapInuse)},
		{"go_memstats_sys_bytes", "Bytes obtained from the OS.", float64(ms.Sys)},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", g.name, g.help, g.name, g.name, formatFloat(g.v))
	}
}
