package eval

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/lag"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

func TestCDF(t *testing.T) {
	levels := []float64{0.5, 0.9, 0.99}
	values := []float64{100, 200, 1000}
	tests := []struct {
		x, want float64
	}{
		{0, 0}, {50, 0.25}, {100, 0.5}, {150, 0.7}, {200, 0.9}, {600, 0.945}, {1000, 1}, {5000, 1},
	}
	for _, tc := range tests {
		if got := CDF(levels, values, tc.x); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("CDF(%v) = %v want %v", tc.x, got, tc.want)
		}
	}
	if !math.IsNaN(CDF(levels, []float64{math.NaN()}, 1)) {
		t.Error("NaN quantiles should give NaN")
	}
	// flat region (many identical quantiles)
	if got := CDF([]float64{0.1, 0.5, 0.9}, []float64{10, 10, 20}, 10); got != 0.5 {
		t.Errorf("flat CDF = %v", got)
	}
}

func TestApdex(t *testing.T) {
	// uniform 0..1000 ms on the grid
	vals := make([]float64, len(GridLevels))
	for i, l := range GridLevels {
		vals[i] = l * 1000
	}
	tests := []struct {
		name      string
		t, errs   float64
		want, tol float64
	}{
		{"T=100: 10% sat, 30% tol", 100, 0, 0.1 + 0.3/2, 0.01},
		{"T=250: all within 4T", 250, 0, 0.25 + 0.75/2, 0.01},
		{"errors frustrate", 1000, 0.2, 0.8, 0.01},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Apdex(GridLevels, vals, tc.t, tc.errs); math.Abs(got-tc.want) > tc.tol {
				t.Fatalf("apdex %v want %v", got, tc.want)
			}
		})
	}
}

func TestParsePercentile(t *testing.T) {
	for in, want := range map[string]float64{"p95": 0.95, "99": 0.99, "0.75": 0.75, "p99.9": 0.999, "": 0.95, "bogus": 0.95, "p100": 0.95} {
		if got := ParsePercentile(in, 0.95); math.Abs(got-want) > 1e-12 {
			t.Errorf("%q → %v want %v", in, got, want)
		}
	}
}

func TestConditionAndScope(t *testing.T) {
	var c model.Condition
	if err := json.Unmarshal([]byte(`{"op":">=","threshold":0.02,"forMin":5,"window":"10m","min_requests":50}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Op != ">=" || c.Threshold != 0.02 || c.For != 5*time.Minute || c.Window != 10*time.Minute || c.MinRequests != 50 {
		t.Fatalf("condition %+v", c)
	}
	var d model.Condition
	_ = json.Unmarshal([]byte(`{"threshold":800,"for":"2m"}`), &d)
	if d.Op != ">" || d.OpSet || d.For != 2*time.Minute || d.Window != 5*time.Minute {
		t.Fatalf("defaults %+v", d)
	}

	m := &model.Monitor{Kind: "latency", Target: model.Target{Service: "orders-api", Endpoint: "POST /checkout"}}
	if s, r, me, _ := m.Scope(); s != "orders-api" || r != "/checkout" || me != "POST" {
		t.Fatalf("scope %s %s %s", s, r, me)
	}
	m.Query.Route = "GET /v1/orders/:id"
	if _, r, me, _ := m.Scope(); r != "/v1/orders/:id" || me != "GET" {
		t.Fatalf("query route %s %s", r, me)
	}

	th, fm := 600.0, 10.0
	lm := &model.Monitor{Kind: "consumer_lag", Condition: model.Condition{Op: "<", Threshold: 1}, Query: model.Query{Metric: "time_behind", Threshold: &th, ForMin: &fm}}
	if c := Condition(lm); c.Threshold != 600 || c.For != 10*time.Minute || c.Op != ">" {
		t.Fatalf("lag condition %+v", c)
	}
}

func TestLagLookback(t *testing.T) {
	tests := []struct {
		m      lag.Metric
		th, fm float64
		want   time.Duration
	}{
		{lag.Lag, 1000, 5, 15 * time.Minute},
		{lag.Growing, 0, 15, 30 * time.Minute},
		{lag.NotDrained, 2, 0, 6 * time.Hour},
		{lag.NotDrained, 10, 0, 20 * time.Hour},
		{lag.NotDrained, 1000, 0, 7 * 24 * time.Hour},
	}
	for _, tc := range tests {
		if got := lagLookback(tc.m, tc.th, tc.fm); got != tc.want {
			t.Errorf("%s: %v want %v", tc.m, got, tc.want)
		}
	}
}

func TestUnknownKindIsNoData(t *testing.T) {
	e := &Evaluator{}
	for _, k := range []string{"composite", "custom", "whatever"} {
		r, err := e.Eval(context.Background(), &model.Monitor{Kind: k}, time.Now())
		if !errors.Is(err, ErrUnsupported) || !r.NoData {
			t.Fatalf("%s: %+v %v", k, r, err)
		}
	}
	// bad lag metric must not panic or query
	r, err := e.Eval(context.Background(), &model.Monitor{Kind: "consumer_lag", Query: model.Query{Metric: "nope"}}, time.Now())
	if err == nil || !r.NoData {
		t.Fatalf("bad lag metric: %+v %v", r, err)
	}
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 7, 31, 0, time.UTC)
	from, to := window(now, 5*time.Minute)
	if !to.Equal(time.Date(2026, 10, 25, 12, 7, 0, 0, time.UTC)) || to.Sub(from) != 5*time.Minute {
		t.Fatalf("window %v %v", from, to)
	}
	if severities(model.Query{MinSeverity: "warn"})[0] != "warn" || len(severities(model.Query{MinSeverity: "error"})) != 2 {
		t.Fatal("severities")
	}
}
