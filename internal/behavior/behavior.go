// Package behavior implements latency injection, fault injection and rate
// limiting. Random decisions draw from the caller's per-request RNG, so they
// are deterministic under a package seed.
package behavior

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

// Fault actions.
const (
	ActionStatus = "status" // respond with Status (default)
	ActionDrop   = "drop"   // close the connection without a response
)

// Config is a behavior definition in mockmint.yaml. At package level it is
// the default for every operation; an operation's own settings replace the
// package's field by field (latency, faults, rateLimit).
type Config struct {
	Latency   *Latency   `yaml:"latency"`
	Faults    []Fault    `yaml:"faults"`
	RateLimit *RateLimit `yaml:"rateLimit"`
}

// Latency delays responses by Min, or uniformly within [Min, Max]. In YAML
// it is a duration ("150ms") or {min: 50ms, max: 200ms}.
type Latency struct {
	Min time.Duration
	Max time.Duration
}

// UnmarshalYAML accepts a duration scalar or a {min, max} mapping.
func (l *Latency) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		d, err := time.ParseDuration(n.Value)
		if err != nil {
			return fmt.Errorf("line %d: latency: %w", n.Line, err)
		}
		*l = Latency{Min: d, Max: d}
		return nil
	}
	var raw struct {
		Min string `yaml:"min"`
		Max string `yaml:"max"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	var err error
	if l.Min, err = time.ParseDuration(raw.Min); err != nil {
		return fmt.Errorf("line %d: latency.min: %w", n.Line, err)
	}
	if l.Max, err = time.ParseDuration(raw.Max); err != nil {
		return fmt.Errorf("line %d: latency.max: %w", n.Line, err)
	}
	return nil
}

// Fault injects a failure with the given probability (0..1].
type Fault struct {
	Probability float64 `yaml:"probability"`
	Action      string  `yaml:"action"`
	Status      int     `yaml:"status"`
}

// RateLimit is a token bucket: RPS tokens per second, up to Burst.
type RateLimit struct {
	RPS   float64 `yaml:"rps"`
	Burst int     `yaml:"burst"`
}

// Merge returns base overridden by override, field by field.
func Merge(base, override Config) Config {
	out := base
	if override.Latency != nil {
		out.Latency = override.Latency
	}
	if override.Faults != nil {
		out.Faults = override.Faults
	}
	if override.RateLimit != nil {
		out.RateLimit = override.RateLimit
	}
	return out
}

// Behavior is a compiled Config for one operation.
type Behavior struct {
	latency Latency
	faults  []Fault
	limiter *Limiter
}

// Compile validates cfg. now is the clock the rate limiter uses (nil = time.Now).
func Compile(cfg Config, now func() time.Time) (*Behavior, error) {
	b := &Behavior{}
	var errs []error
	if l := cfg.Latency; l != nil {
		if l.Min < 0 || l.Max < l.Min {
			errs = append(errs, fmt.Errorf("latency: need 0 <= min <= max, got %v..%v", l.Min, l.Max))
		}
		b.latency = *l
	}
	for i, f := range cfg.Faults {
		if f.Action == "" {
			f.Action = ActionStatus
		}
		if !(f.Probability > 0 && f.Probability <= 1) {
			errs = append(errs, fmt.Errorf("faults[%d]: probability must be in (0, 1]", i))
		}
		switch f.Action {
		case ActionStatus:
			if f.Status < 400 || f.Status > 599 {
				errs = append(errs, fmt.Errorf("faults[%d]: status must be 4xx or 5xx", i))
			}
		case ActionDrop:
		default:
			errs = append(errs, fmt.Errorf("faults[%d]: action %q: want status or drop", i, f.Action))
		}
		b.faults = append(b.faults, f)
	}
	if rl := cfg.RateLimit; rl != nil {
		if rl.RPS <= 0 || rl.Burst < 1 {
			errs = append(errs, errors.New("rateLimit: need rps > 0 and burst >= 1"))
		} else {
			b.limiter = NewLimiter(rl.RPS, rl.Burst, now)
		}
	}
	return b, errors.Join(errs...)
}

// Delay returns the latency to inject for one request.
func (b *Behavior) Delay(rng *rand.Rand) time.Duration {
	if b.latency.Max <= b.latency.Min {
		return b.latency.Min
	}
	return b.latency.Min + time.Duration(rng.Int64N(int64(b.latency.Max-b.latency.Min)+1))
}

// Fault rolls each fault in declaration order and returns the first hit.
func (b *Behavior) Fault(rng *rand.Rand) (Fault, bool) {
	for _, f := range b.faults {
		if rng.Float64() < f.Probability {
			return f, true
		}
	}
	return Fault{}, false
}

// Allow takes a rate-limit token. When denied it returns how long until the
// next token.
func (b *Behavior) Allow() (bool, time.Duration) {
	if b.limiter == nil {
		return true, 0
	}
	return b.limiter.Allow()
}

// Sleep waits for d or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Limiter is a mutex-guarded token bucket.
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewLimiter returns a full bucket.
func NewLimiter(rps float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rps, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

// Allow takes one token if available; otherwise it reports the wait until one is.
func (l *Limiter) Allow() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens = math.Min(l.burst, l.tokens+elapsed*l.rate)
		l.last = now
	}
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
	return false, wait
}
