package rules

import (
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

func TestEvaluate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		data      []float64
		th        float64
		forMin    float64
		stepMin   float64
		episodes  int
		firingNow bool
	}{
		{"never", []float64{1, 2, 3}, 5, 1, 1, 0, false},
		{"one short breach below for", []float64{9, 1, 1}, 5, 2, 1, 0, false},
		{"one episode", []float64{9, 9, 1, 1}, 5, 2, 1, 1, false},
		{"two episodes firing now", []float64{9, 9, 1, 9, 9, 9}, 5, 2, 1, 2, true},
		{"forMin 0 means one point", []float64{6, 1, 6}, 5, 0, 1, 2, true},
		{"coarse step rounds up", []float64{6, 6, 6}, 5, 10, 5, 1, true},
		{"equal is not above", []float64{5, 5}, 5, 1, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Evaluate(tc.data, tc.th, tc.forMin, tc.stepMin)
			if e.Episodes != tc.episodes || e.FiringNow != tc.firingNow || e.Now != tc.data[len(tc.data)-1] {
				t.Errorf("got %+v", e)
			}
		})
	}
}

func TestEvaluateOp(t *testing.T) {
	if e := EvaluateOp([]float64{0.9, 0.8}, "<", 0.95, 1, 1); e.Episodes != 1 || !e.FiringNow {
		t.Errorf("%+v", e)
	}
}

func TestRuleSeries(t *testing.T) {
	lag := []float64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000, 1100, 1200}
	in := LagInputs{Lag: lag, ProduceRate: make([]float64, len(lag)), MaxPartition: make([]float64, len(lag)), Rebalances: make([]float64, len(lag)), OldestAge: make([]float64, len(lag))}
	for i := range lag {
		in.OldestAge[i] = float64(i * 30)
		in.ProduceRate[i] = 10
		in.MaxPartition[i] = lag[i] / 2
		in.Rebalances[i] = 3
	}
	for _, tc := range []struct {
		metric string
		idx    int
		want   float64
	}{
		{"lag", 3, 400},
		{"time_behind", 0, 10},
		{"partition_lag", 1, 100},
		{"rebalances", 5, 3},
		{"oldest_age", 4, 120},
		{"growing", 0, 0},
		{"growing", 11, 100}, // +100 per 1-minute step
		{"not_drained", 11, 12.0 / 60},
	} {
		got := RuleSeries(tc.metric, in, 1)
		if len(got) != len(lag) {
			t.Fatalf("%s: len %d", tc.metric, len(got))
		}
		if d := got[tc.idx] - tc.want; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s[%d] = %v, want %v", tc.metric, tc.idx, got[tc.idx], tc.want)
		}
	}
	// not_drained resets when lag returns near zero
	nd := RuleSeries("not_drained", LagInputs{Lag: []float64{1000, 1000, 0, 1000}}, 60)
	if nd[1] != 2 || nd[2] != 0 || nd[3] != 1 {
		t.Errorf("not_drained %v", nd)
	}
}

func TestDescribeQueueRule(t *testing.T) {
	for _, tc := range []struct {
		r    model.QueueRule
		want string
	}{
		{model.QueueRule{Metric: "lag", Threshold: 10000, ForMin: 5, Topic: "orders.created"}, "lag > 10K for 5m on orders.created"},
		{model.QueueRule{Metric: "not_drained", Threshold: 2}, "lag not drained in 2h"},
		{model.QueueRule{Metric: "growing", ForMin: 15}, "lag growing for 15m"},
		{model.QueueRule{Metric: "time_behind", Threshold: 120, ForMin: 10}, "time behind > 120s for 10m"},
		{model.QueueRule{Metric: "rebalances", Threshold: 5, ForMin: 15}, "rebalances > 5/h for 15m"},
		{model.QueueRule{Metric: "partition_lag", Threshold: 1500}, "max partition lag > 1.5K"},
		{model.QueueRule{Metric: "oldest_age", Threshold: 300, ForMin: 5}, "oldest message age > 300s for 5m"},
	} {
		if got := DescribeQueueRule(tc.r); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

func TestDescribeCondition(t *testing.T) {
	if got := DescribeCondition("metric_threshold", "error_rate", model.MonitorCondition{Op: ">", Threshold: 0.02, ForMin: 5}); got != "error rate > 2% for 5m" {
		t.Error(got)
	}
	if got := DescribeCondition("latency", "p95", model.MonitorCondition{Threshold: 600, ForMin: 10}); got != "p95 > 600 ms for 10m" {
		t.Error(got)
	}
}
