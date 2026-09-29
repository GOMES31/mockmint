package template

import (
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"math/rand/v2"
	"time"
)

// Request is the request view exposed to templates as .Request.
type Request struct {
	Method  string
	Path    string
	Params  map[string]string   // path parameters
	Query   map[string]string   // first value per query parameter
	Queries map[string][]string // all values per query parameter
	Headers map[string]string   // first value per canonical header name
	Body    any                 // decoded JSON body, nil if absent or not JSON
	RawBody string
}

// Data is the template root. Its methods are the request-scoped helpers.
type Data struct {
	Request   Request
	Operation string
	Example   string
	Fake      Fake
	// Response is the decoded response body, set only while evaluating a
	// stateful create operation's key (e.g. "{{.Response.id}}").
	Response any
	State    State

	rng *rand.Rand
	now time.Time
}

// NewData returns template data drawing randomness from rng and time from now.
func NewData(req Request, operation, example string, rng *rand.Rand, now time.Time) *Data {
	return &Data{Request: req, Operation: operation, Example: example, Fake: Fake{rng: rng}, rng: rng, now: now}
}

// Now returns the render time (frozen when the package sets a clock).
func (d *Data) Now() time.Time { return d.now }

// Timestamp returns Now in RFC 3339 with nanoseconds trimmed to milliseconds.
func (d *Data) Timestamp() string { return d.now.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// Unix returns Now as Unix seconds.
func (d *Data) Unix() int64 { return d.now.Unix() }

// UnixMilli returns Now as Unix milliseconds.
func (d *Data) UnixMilli() int64 { return d.now.UnixMilli() }

// UUID returns a random (version 4) UUID drawn from the request RNG.
func (d *Data) UUID() string {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], d.rng.Uint64())
	binary.LittleEndian.PutUint64(b[8:], d.rng.Uint64())
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

// RandInt returns an int in [lo, hi]. Bounds are swapped if reversed.
func (d *Data) RandInt(lo, hi int) int {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + d.rng.IntN(hi-lo+1)
}

// RandFloat returns a float64 in [lo, hi).
func (d *Data) RandFloat(lo, hi float64) float64 {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + d.rng.Float64()*(hi-lo)
}

// RandBool returns a random boolean.
func (d *Data) RandBool() bool { return d.rng.IntN(2) == 1 }

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// maxRandString caps RandString to keep renders bounded.
const maxRandString = 4096

// RandString returns n random alphanumeric characters (n capped at 4096).
func (d *Data) RandString(n int) string {
	n = min(max(n, 0), maxRandString)
	b := make([]byte, n)
	for i := range b {
		b[i] = alnum[d.rng.IntN(len(alnum))]
	}
	return string(b)
}

// RandItem returns one of items, or nil when there are none.
func (d *Data) RandItem(items ...any) any {
	if len(items) == 0 {
		return nil
	}
	return items[d.rng.IntN(len(items))]
}

// NewRand returns the RNG for one request. With seed == 0 it is seeded from
// crypto/rand. Otherwise it is derived from the seed and parts (operation id,
// canonical request), so identical requests get identical sequences
// regardless of arrival order or concurrency.
func NewRand(seed int64, parts ...[]byte) *rand.Rand {
	if seed == 0 {
		var b [16]byte
		_, _ = crand.Read(b[:]) // never returns an error (Go 1.24+)
		return rand.New(rand.NewPCG(binary.LittleEndian.Uint64(b[:8]), binary.LittleEndian.Uint64(b[8:])))
	}
	h := fnv.New64a()
	var sb [8]byte
	binary.LittleEndian.PutUint64(sb[:], uint64(seed)) //nolint:gosec // G115: bit-for-bit reinterpretation for hashing
	_, _ = h.Write(sb[:])
	for _, p := range parts {
		binary.LittleEndian.PutUint64(sb[:], uint64(len(p)))
		_, _ = h.Write(sb[:]) // length prefix keeps ("ab","c") distinct from ("a","bc")
		_, _ = h.Write(p)
	}
	s := h.Sum64()
	return rand.New(rand.NewPCG(s, s^0x9e3779b97f4a7c15))
}
