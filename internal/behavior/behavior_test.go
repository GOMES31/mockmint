package behavior

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func parse(t *testing.T, yml string) Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func rng(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed)) }

func TestLatency(t *testing.T) {
	b, err := Compile(parse(t, "latency: 150ms"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := b.Delay(rng(1)); d != 150*time.Millisecond {
		t.Fatalf("fixed = %v", d)
	}
	b, err = Compile(parse(t, "latency: {min: 10ms, max: 20ms}"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		if d := b.Delay(rng(uint64(i))); d < 10*time.Millisecond || d > 20*time.Millisecond {
			t.Fatalf("ranged out of bounds: %v", d)
		}
	}
	if first, second := b.Delay(rng(9)), b.Delay(rng(9)); first != second {
		t.Fatal("delay must be deterministic for the same rng")
	}
	if d := (&Behavior{}).Delay(rng(1)); d != 0 {
		t.Fatalf("no latency = %v", d)
	}
}

func TestFaults(t *testing.T) {
	b, err := Compile(parse(t, `
faults:
  - {probability: 0.25, status: 503}
  - {probability: 1, action: drop}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	r := rng(42)
	for range 10000 {
		f, ok := b.Fault(r)
		if !ok {
			t.Fatal("second fault has probability 1")
		}
		counts[f.Action]++
	}
	if s := counts[ActionStatus]; s < 2300 || s > 2700 {
		t.Fatalf("status faults = %d of 10000, want ~2500", s)
	}
	if _, ok := (&Behavior{}).Fault(r); ok {
		t.Fatal("no faults configured")
	}
}

func TestCompileErrors(t *testing.T) {
	for _, tt := range []struct{ yml, want string }{
		{"latency: {min: 20ms, max: 10ms}", "min <= max"},
		{"latency: soon", "latency"},
		{"latency: {min: 1ms, max: x}", "latency.max"},
		{"faults: [{probability: 0, status: 500}]", "probability"},
		{"faults: [{probability: 1.5, status: 500}]", "probability"},
		{"faults: [{probability: 0.5, status: 200}]", "4xx or 5xx"},
		{"faults: [{probability: 0.5, action: explode}]", "action"},
		{"rateLimit: {rps: 0, burst: 1}", "rateLimit"},
		{"rateLimit: {rps: 1, burst: 0}", "rateLimit"},
	} {
		c, err := func() (Config, error) {
			var c Config
			return c, yaml.Unmarshal([]byte(tt.yml), &c)
		}()
		if err == nil {
			_, err = Compile(c, nil)
		}
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.yml, err, tt.want)
		}
	}
}

func TestMerge(t *testing.T) {
	base := parse(t, "latency: 1s\nfaults: [{probability: 1, status: 500}]")
	got := Merge(base, parse(t, "latency: 2s\nrateLimit: {rps: 1, burst: 1}"))
	if got.Latency.Min != 2*time.Second || len(got.Faults) != 1 || got.RateLimit == nil {
		t.Fatalf("merge = %+v", got)
	}
	// An explicit empty list clears inherited faults.
	if got = Merge(base, parse(t, "faults: []")); got.Faults == nil || len(got.Faults) != 0 {
		t.Fatalf("faults not cleared: %+v", got.Faults)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := NewLimiter(2, 3, clock) // 2/s, burst 3
	for i := range 3 {
		if ok, _ := l.Allow(); !ok {
			t.Fatalf("burst token %d denied", i)
		}
	}
	ok, wait := l.Allow()
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("empty bucket: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(250 * time.Millisecond)
	if ok, wait = l.Allow(); ok || wait != 250*time.Millisecond {
		t.Fatalf("half token: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(250 * time.Millisecond)
	if ok, _ = l.Allow(); !ok {
		t.Fatal("refilled token denied")
	}
	now = now.Add(time.Hour)
	for range 3 {
		if ok, _ = l.Allow(); !ok {
			t.Fatal("bucket must refill to burst")
		}
	}
	if ok, _ = l.Allow(); ok {
		t.Fatal("bucket must cap at burst")
	}
}

func TestSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("sleep ignored cancellation")
	}
	if err := Sleep(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}
