package rate

import (
	"math"
	"testing"
	"time"
)

func TestCountersDelta(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	type step struct {
		v     float64
		at    time.Duration
		delta float64
		rate  float64
		ok    bool
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"first reading has no delta", []step{{v: 100, ok: false}}},
		{"steady increase", []step{{v: 100}, {v: 250, at: 15 * time.Second, delta: 150, rate: 10, ok: true}, {v: 400, at: 30 * time.Second, delta: 150, rate: 10, ok: true}}},
		{"no change", []step{{v: 5}, {v: 5, at: 10 * time.Second, delta: 0, rate: 0, ok: true}}},
		{"counter reset uses new value", []step{{v: 1000}, {v: 30, at: 10 * time.Second, delta: 30, rate: 3, ok: true}}},
		{"clock did not advance", []step{{v: 1}, {v: 2, at: 0, ok: false}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCounters()
			for i, s := range tt.steps {
				d, dt, ok := c.Delta("k", s.v, t0.Add(s.at))
				if ok != s.ok {
					t.Fatalf("step %d: ok=%v want %v", i, ok, s.ok)
				}
				if !ok {
					continue
				}
				if d != s.delta {
					t.Errorf("step %d: delta=%v want %v", i, d, s.delta)
				}
				if r := PerSecond(d, dt); math.Abs(r-s.rate) > 1e-9 {
					t.Errorf("step %d: rate=%v want %v", i, r, s.rate)
				}
			}
		})
	}
}

func TestCountersPrune(t *testing.T) {
	c := NewCounters()
	t0 := time.Unix(0, 0)
	c.Delta("old", 1, t0)
	c.Delta("new", 1, t0.Add(time.Minute))
	c.Prune(t0.Add(30 * time.Second))
	if c.Len() != 1 {
		t.Fatalf("len=%d want 1", c.Len())
	}
}

func TestPerMinute(t *testing.T) {
	tests := []struct {
		delta float64
		dt    time.Duration
		want  float64
	}{
		{10, 15 * time.Second, 40},
		{0, 15 * time.Second, 0},
		{5, 0, 0},
		{3, time.Minute, 3},
	}
	for _, tt := range tests {
		if got := PerMinute(tt.delta, tt.dt); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("PerMinute(%v,%v)=%v want %v", tt.delta, tt.dt, got, tt.want)
		}
	}
}

func TestHitRatio(t *testing.T) {
	tests := []struct {
		name      string
		hit, miss float64
		want      float64
		ok        bool
	}{
		{"all hits", 100, 0, 1, true},
		{"all misses", 0, 50, 0, true},
		{"mixed", 990, 10, 0.99, true},
		{"no traffic", 0, 0, 0, false},
		{"negative is invalid", -1, 5, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := HitRatio(tt.hit, tt.miss)
			if ok != tt.ok || math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("HitRatio=%v,%v want %v,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestFraction(t *testing.T) {
	tests := []struct {
		used, limit, want float64
		ok                bool
	}{
		{50, 100, 0.5, true},
		{150, 100, 1, true},
		{1, 0, 0, false},
	}
	for _, tt := range tests {
		got, ok := Fraction(tt.used, tt.limit)
		if ok != tt.ok || got != tt.want {
			t.Errorf("Fraction(%v,%v)=%v,%v want %v,%v", tt.used, tt.limit, got, ok, tt.want, tt.ok)
		}
	}
}

func TestQuantile(t *testing.T) {
	s := make([]float64, 100)
	for i := range s {
		s[i] = float64(100 - i) // 100..1, unsorted
	}
	tests := []struct {
		q    float64
		want float64
	}{{0.95, 95}, {0.5, 50}, {1, 100}, {0, 1}}
	for _, tt := range tests {
		got, ok := Quantile(s, tt.q)
		if !ok || got != tt.want {
			t.Errorf("Quantile(%v)=%v want %v", tt.q, got, tt.want)
		}
	}
	if _, ok := Quantile(nil, 0.95); ok {
		t.Error("empty should not be ok")
	}
	if s[0] != 100 {
		t.Error("input was modified")
	}
}

func TestWeightedQuantile(t *testing.T) {
	tests := []struct {
		name  string
		items []Weighted
		q     float64
		want  float64
		ok    bool
	}{
		{"single", []Weighted{{Value: 4, Weight: 10}}, 0.95, 4, true},
		{"heavy fast statement dominates", []Weighted{{Value: 1, Weight: 99}, {Value: 500, Weight: 1}}, 0.95, 1, true},
		{"slow tail above 5%", []Weighted{{Value: 1, Weight: 90}, {Value: 500, Weight: 10}}, 0.95, 500, true},
		{"zero weights ignored", []Weighted{{Value: 7, Weight: 0}}, 0.95, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := WeightedQuantile(tt.items, tt.q)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("got %v,%v want %v,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestApproxP95(t *testing.T) {
	if got := ApproxP95(10, 2); got != 14 {
		t.Fatalf("got %v", got)
	}
	if got := ApproxP95(10, math.NaN()); got != 10 {
		t.Fatalf("NaN stddev: got %v", got)
	}
}

func TestRebalanceCounter(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	type obs struct {
		at    time.Duration
		state string
		gen   string
		want  int
	}
	tests := []struct {
		name string
		obs  []obs
	}{
		{"stable group never rebalances", []obs{
			{0, "Stable", "a,b", 0}, {15 * time.Second, "Stable", "a,b", 0}, {30 * time.Second, "Stable", "a,b", 0},
		}},
		{"member joins: one rebalance", []obs{
			{0, "Stable", "a,b", 0}, {15 * time.Second, "Stable", "a,b,c", 1}, {30 * time.Second, "Stable", "a,b,c", 1},
		}},
		{"transient states count once", []obs{
			{0, "Stable", "a", 0},
			{15 * time.Second, "PreparingRebalance", "a", 0},
			{30 * time.Second, "CompletingRebalance", "", 0},
			{45 * time.Second, "Stable", "b", 1},
		}},
		{"static member rejoin with same ids still counts", []obs{
			{0, "Stable", "a", 0}, {15 * time.Second, "PreparingRebalance", "a", 0}, {30 * time.Second, "Stable", "a", 1},
		}},
		{"first observation mid-rebalance has no baseline", []obs{
			{0, "PreparingRebalance", "", 0}, {15 * time.Second, "Stable", "a", 0},
		}},
		{"group drains to Empty counts", []obs{
			{0, "Stable", "a", 0}, {15 * time.Second, "Empty", "", 1},
		}},
		{"events leave the 1h window", []obs{
			{0, "Stable", "a", 0},
			{time.Minute, "Stable", "b", 1},
			{2 * time.Minute, "Stable", "c", 2},
			{61 * time.Minute, "Stable", "c", 1},
			{62*time.Minute + time.Second, "Stable", "c", 0},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRebalanceCounter()
			for i, o := range tt.obs {
				if got := r.Observe("g", o.state, o.gen, t0.Add(o.at)); got != o.want {
					t.Fatalf("obs %d (%s %q): got %d want %d", i, o.state, o.gen, got, o.want)
				}
			}
		})
	}
}

func TestRebalanceCounterGroupsIndependent(t *testing.T) {
	r := NewRebalanceCounter()
	t0 := time.Unix(0, 0)
	r.Observe("g1", "Stable", "a", t0)
	r.Observe("g2", "Stable", "x", t0)
	if got := r.Observe("g1", "Stable", "b", t0.Add(time.Second)); got != 1 {
		t.Fatalf("g1=%d", got)
	}
	if got := r.Observe("g2", "Stable", "x", t0.Add(time.Second)); got != 0 {
		t.Fatalf("g2=%d", got)
	}
}

func TestNewKeyDelta(t *testing.T) {
	t0 := time.Unix(100, 0)
	tests := []struct {
		name     string
		baseline time.Time
		pre      []float64
		v        float64
		want     float64
		ok       bool
	}{
		{"first scrape: baseline only", time.Time{}, nil, 50, 0, false},
		{"new key after baseline counts fully", t0.Add(-15 * time.Second), nil, 50, 50, true},
		{"known key uses delta", t0.Add(-15 * time.Second), []float64{30}, 50, 20, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCounters()
			for _, p := range tt.pre {
				c.Delta("k", p, t0.Add(-15*time.Second))
			}
			got, ok := c.NewKeyDelta("k", tt.v, t0, tt.baseline)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("got %v,%v want %v,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestIntervalHitRatio(t *testing.T) {
	tests := []struct {
		name            string
		dh, dm          float64
		have            bool
		hit, miss, want float64
		ok              bool
	}{
		{"interval wins", 9, 1, true, 50, 50, 0.9, true},
		{"idle interval falls back to cumulative", 0, 0, true, 75, 25, 0.75, true},
		{"first scrape uses cumulative", 0, 0, false, 1, 1, 0.5, true},
		{"never any traffic", 0, 0, true, 0, 0, 0, false},
	}
	for _, tt := range tests {
		got, ok := IntervalHitRatio(tt.dh, tt.dm, tt.have, tt.hit, tt.miss)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: got %v,%v", tt.name, got, ok)
		}
	}
}
