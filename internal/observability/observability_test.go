package observability

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestRegistryText(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("req_total", "Requests.\nTwo lines.", "pkg", "status")
	g := r.NewGaugeVec("up", "Up.")
	h := r.NewHistogramVec("dur_seconds", "Duration.", []float64{0.1, 1}, "pkg")
	c.Inc("b", "200")
	c.Inc("a", "404")
	c.Inc("a", "404")
	c.Inc(`q"x\y`, "200")
	g.Set(1)
	h.Observe(0.05, "a")
	h.Observe(0.1, "a") // equal to a bound → that bucket (le is inclusive)
	h.Observe(3, "a")

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"# HELP req_total Requests.\\nTwo lines.\n# TYPE req_total counter\n",
		`req_total{pkg="a",status="404"} 2` + "\n" + `req_total{pkg="b",status="200"} 1` + "\n" + `req_total{pkg="q\"x\\y",status="200"} 1`,
		"# TYPE up gauge\nup 1\n",
		`dur_seconds_bucket{pkg="a",le="0.1"} 2`,
		`dur_seconds_bucket{pkg="a",le="1"} 2`,
		`dur_seconds_bucket{pkg="a",le="+Inf"} 3`,
		`dur_seconds_sum{pkg="a"} 3.15`,
		`dur_seconds_count{pkg="a"} 3`,
		"# TYPE go_goroutines gauge\ngo_goroutines ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if c.Value("a", "404") != 2 {
		t.Fatal("Value wrong")
	}
}

func TestRegistryPanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounterVec("x", "x", "a")
	for name, fn := range map[string]func(){
		"duplicate":    func() { r.NewCounterVec("x", "x") },
		"label count":  func() { r.NewCounterVec("y", "y", "a").Inc() },
		"bad buckets":  func() { r.NewHistogramVec("z", "z", []float64{2, 1}) },
		"gauge labels": func() { r.NewGaugeVec("w", "w").Set(1, "extra") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestMetricsConcurrent(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				m.HTTPRequests.Inc("p", "op", "200")
				m.HTTPDuration.Observe(0.002, "p", "op")
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			_ = m.Registry.WriteText(&bytes.Buffer{})
		}
	})
	wg.Wait()
	if got := m.HTTPRequests.Value("p", "op", "200"); got != 8000 {
		t.Fatalf("count = %d", got)
	}
}

func TestTrafficRing(t *testing.T) {
	tr := NewTraffic(3, "x-secret")
	for i := range 5 {
		tr.Record(Exchange{Protocol: "http", Package: []string{"a", "b"}[i%2], Path: string(rune('0' + i)),
			Headers: map[string]string{"authorization": "Bearer t", "X-Secret": "s", "Accept": "json"}})
	}
	all := tr.List(Filter{})
	if len(all) != 3 || all[0].Path != "4" || all[2].Path != "2" || all[0].ID != 5 {
		t.Fatalf("ring = %+v", all)
	}
	h := all[0].Headers
	if h["authorization"] != Redacted || h["X-Secret"] != Redacted || h["Accept"] != "json" {
		t.Fatalf("redaction = %v", h)
	}
	if got := tr.List(Filter{Package: "a", Limit: 1}); len(got) != 1 || got[0].Path != "4" {
		t.Fatalf("filter = %+v", got)
	}
	tr.Clear()
	if len(tr.List(Filter{})) != 0 {
		t.Fatal("not cleared")
	}
	var nilTraffic *Traffic
	nilTraffic.Record(Exchange{}) // must not panic
}

func TestTrafficTruncatesOnRuneBoundary(t *testing.T) {
	tr := NewTraffic(1)
	body := strings.Repeat("a", MaxBodyPreview-1) + "é" + "tail"
	tr.Record(Exchange{Body: body})
	e := tr.List(Filter{})[0]
	if !e.Truncated || len(e.Body) != MaxBodyPreview-1 || !strings.HasSuffix(e.Body, "a") {
		t.Fatalf("len %d truncated %v", len(e.Body), e.Truncated)
	}
	if got := Headers(http.Header{"A": {"1", "2"}, "B": {}}); len(got) != 1 || got["A"] != "1" {
		t.Fatalf("Headers = %v", got)
	}
}
