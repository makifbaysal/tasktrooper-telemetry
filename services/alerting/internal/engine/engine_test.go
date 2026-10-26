package engine

import (
	"math"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/burn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
)

func TestRuleCheck(t *testing.T) {
	fast := model.KpiRule{Kind: "burn_rate", LongWindow: time.Hour, ShortWindow: 5 * time.Minute, Threshold: 14.4}
	below := model.KpiRule{Kind: "below_target", LongWindow: 15 * time.Minute, Threshold: 97}
	tests := []struct {
		name        string
		r           model.KpiRule
		obj         float64
		long, short burn.Counts
		value       float64
		breached    bool
		noData      bool
	}{
		{"fast burn fires", fast, 0.99, burn.Counts{Good: 800, Total: 1000}, burn.Counts{Good: 70, Total: 100}, 20, true, false},
		{"burn stopped (short window clean)", fast, 0.99, burn.Counts{Good: 800, Total: 1000}, burn.Counts{Good: 100, Total: 100}, 20, false, false},
		{"no traffic", fast, 0.99, burn.Counts{}, burn.Counts{}, 0, false, true},
		{"below target 97% given as percent", below, 0.95, burn.Counts{Good: 96, Total: 100}, burn.Counts{}, 0.96, true, false},
		{"at target", below, 0.95, burn.Counts{Good: 97, Total: 100}, burn.Counts{}, 0.97, false, false},
		{"below without traffic", below, 0.95, burn.Counts{}, burn.Counts{}, 0, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, b, nd := RuleCheck(tc.r, tc.obj, tc.long, tc.short)
			if math.Abs(v-tc.value) > 1e-9 || b != tc.breached || nd != tc.noData {
				t.Fatalf("got %v %v %v", v, b, nd)
			}
		})
	}
}

func TestRuleTitle(t *testing.T) {
	k := &model.Kpi{Name: "Checkout succeeds"}
	tests := []struct {
		r    model.KpiRule
		want string
	}{
		{model.KpiRule{Kind: "burn_rate", LongWindow: time.Hour, ShortWindow: 5 * time.Minute, Threshold: 14.4}, "Checkout succeeds: error budget burning > 14.4× (1h/5m)"},
		{model.KpiRule{Kind: "burn_rate", LongWindow: 6 * time.Hour, Threshold: 6}, "Checkout succeeds: error budget burning > 6× (6h/30m)"},
		{model.KpiRule{Kind: "burn_rate", Threshold: 6}, "Checkout succeeds: error budget burning > 6× (1h/5m)"},
		{model.KpiRule{Kind: "below_target", Threshold: 0.97}, "Checkout succeeds: below 97% over 15m"},
		{model.KpiRule{Kind: "below_target", LongWindow: 24 * time.Hour, Threshold: 99.5}, "Checkout succeeds: below 99.5% over 1d"},
	}
	for _, tc := range tests {
		if got := RuleTitle(k, tc.r); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

func TestRCAUnit(t *testing.T) {
	tests := []struct {
		m    model.Monitor
		want rca.Unit
	}{
		{model.Monitor{Kind: "latency"}, rca.UnitMs},
		{model.Monitor{Kind: "error_rate"}, rca.UnitRatio},
		{model.Monitor{Kind: "metric_threshold", Query: model.Query{Metric: "p95"}}, rca.UnitMs},
		{model.Monitor{Kind: "metric_threshold", Query: model.Query{Metric: "error_rate"}}, rca.UnitRatio},
		{model.Monitor{Kind: "metric_threshold", Query: model.Query{Metric: "rpm"}}, ""},
		{model.Monitor{Kind: "consumer_lag"}, ""},
	}
	for _, tc := range tests {
		if got := rcaUnit(&tc.m); got != tc.want {
			t.Errorf("%s/%s: %q want %q", tc.m.Kind, tc.m.Query.Metric, got, tc.want)
		}
	}
}
