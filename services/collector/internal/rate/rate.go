// Package rate holds the counter → rate math every scraper shares: deltas between scrapes,
// per-second / per-minute rates, hit ratios, quantiles and the rebalance counter.
package rate

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Counters remembers the previous value of monotonically increasing counters (keyed by name)
// and turns new readings into deltas. A reading lower than the previous one is treated as a
// counter reset (server restart, pg_stat_statements_reset(), ...): the delta is the new value.
type Counters struct {
	mu   sync.Mutex
	prev map[string]reading
}

type reading struct {
	v  float64
	at time.Time
}

// NewCounters returns an empty tracker.
func NewCounters() *Counters { return &Counters{prev: map[string]reading{}} }

// Delta records v for key at now and returns the increase since the previous reading and the
// elapsed time. ok is false on the first reading of a key (there is nothing to compare with)
// or when the clock did not advance.
func (c *Counters) Delta(key string, v float64, now time.Time) (delta float64, dt time.Duration, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, seen := c.prev[key]
	c.prev[key] = reading{v: v, at: now}
	if !seen {
		return 0, 0, false
	}
	dt = now.Sub(p.at)
	if dt <= 0 {
		return 0, 0, false
	}
	if v < p.v { // reset
		return v, dt, true
	}
	return v - p.v, dt, true
}

// Rate returns the per-second rate of key (see Delta).
func (c *Counters) Rate(key string, v float64, now time.Time) (float64, bool) {
	d, dt, ok := c.Delta(key, v, now)
	if !ok {
		return 0, false
	}
	return PerSecond(d, dt), true
}

// Prune forgets keys not updated since before cutoff (dropped queries, deleted topics).
func (c *Counters) Prune(cutoff time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, r := range c.prev {
		if r.at.Before(cutoff) {
			delete(c.prev, k)
		}
	}
}

// Len is the number of tracked keys.
func (c *Counters) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prev)
}

// PerSecond converts a delta over dt into a per-second rate (0 when dt <= 0).
func PerSecond(delta float64, dt time.Duration) float64 {
	if dt <= 0 {
		return 0
	}
	return delta / dt.Seconds()
}

// PerMinute converts a delta over dt into a per-minute rate (0 when dt <= 0).
func PerMinute(delta float64, dt time.Duration) float64 { return PerSecond(delta, dt) * 60 }

// HitRatio returns hit / (hit + miss), clamped to 0..1. ok is false when there was no traffic.
func HitRatio(hit, miss float64) (float64, bool) {
	if hit < 0 || miss < 0 {
		return 0, false
	}
	total := hit + miss
	if total <= 0 {
		return 0, false
	}
	return Clamp01(hit / total), true
}

// Fraction returns used / limit clamped to 0..1; ok is false when limit <= 0.
func Fraction(used, limit float64) (float64, bool) {
	if limit <= 0 || used < 0 {
		return 0, false
	}
	return Clamp01(used / limit), true
}

// Clamp01 clamps v into [0, 1]; NaN becomes 0.
func Clamp01(v float64) float64 {
	switch {
	case math.IsNaN(v) || v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// Quantile returns the q-quantile (0..1) of samples using the nearest-rank method.
// ok is false for an empty slice. samples is not modified.
func Quantile(samples []float64, q float64) (float64, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	return s[rank(len(s), q)], true
}

func rank(n int, q float64) int {
	if q <= 0 {
		return 0
	}
	if q >= 1 {
		return n - 1
	}
	i := int(math.Ceil(q*float64(n))) - 1
	if i < 0 {
		i = 0
	}
	return i
}

// Weighted is one value with a weight (e.g. a statement's mean latency weighted by its calls).
type Weighted struct {
	Value, Weight float64
}

// WeightedQuantile returns the q-quantile of a weighted distribution: the smallest value whose
// cumulative weight reaches q of the total. Used for "p95 across statements" where each
// statement contributes its interval mean latency once per call. ok is false when the total
// weight is 0.
func WeightedQuantile(items []Weighted, q float64) (float64, bool) {
	var total float64
	s := make([]Weighted, 0, len(items))
	for _, it := range items {
		if it.Weight > 0 && !math.IsNaN(it.Value) {
			s = append(s, it)
			total += it.Weight
		}
	}
	if total <= 0 {
		return 0, false
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Value < s[j].Value })
	target := q * total
	var cum float64
	for _, it := range s {
		cum += it.Weight
		if cum >= target {
			return it.Value, true
		}
	}
	return s[len(s)-1].Value, true
}

// ApproxP95 approximates a p95 from mean and standard deviation (mean + 2σ; for a normal
// distribution that is ≈ p97.7, a deliberately conservative upper estimate). Used where the
// database only keeps mean/stddev (pg_stat_statements).
func ApproxP95(mean, stddev float64) float64 {
	if stddev < 0 || math.IsNaN(stddev) {
		stddev = 0
	}
	return mean + 2*stddev
}

// NewKeyDelta is like Delta, but a key never seen before counts entirely as new when the
// tracker already has a baseline (baselineAt non-zero): a statement / command that first appears
// between two scrapes ran only in that interval. On the very first scrape (baselineAt zero)
// unseen keys only establish the baseline.
func (c *Counters) NewKeyDelta(key string, v float64, now, baselineAt time.Time) (float64, bool) {
	c.mu.Lock()
	_, seen := c.prev[key]
	c.mu.Unlock()
	d, _, ok := c.Delta(key, v, now)
	if !seen && !baselineAt.IsZero() && now.After(baselineAt) {
		return max(v, 0), true
	}
	return d, ok
}

// IntervalHitRatio prefers the ratio over the last interval (deltas) and falls back to the
// cumulative ratio since server start on the first scrape or when the interval had no traffic.
func IntervalHitRatio(dHit, dMiss float64, haveDelta bool, hit, miss float64) (float64, bool) {
	if haveDelta {
		if v, ok := HitRatio(dHit, dMiss); ok {
			return v, true
		}
	}
	return HitRatio(hit, miss)
}
