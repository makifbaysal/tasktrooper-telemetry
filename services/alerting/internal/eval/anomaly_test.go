package eval

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/lag"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

func TestRobustZ(t *testing.T) {
	tests := []struct {
		name        string
		samples     []float64
		x           float64
		median, mad float64
		z           float64
		n           int
	}{
		{"symmetric", []float64{1, 2, 3, 4, 5}, 8, 3, 1, 5 / 1.4826, 5},
		{"outlier in baseline ignored", []float64{10, 10, 11, 9, 10, 500}, 10, 10, 0.5, 0, 6},
		{"down", []float64{100, 102, 98, 101, 99}, 80, 100, 1, -20 / 1.4826, 5},
		{"flat baseline uses mean deviation", []float64{5, 5, 5, 5, 6}, 7, 5, 0, 2 / (1.2533 * 0.2), 5},
		{"constant baseline, deviation", []float64{0, 0, 0, 0}, 0.1, 0, 0, anomalyMaxZ, 4},
		{"constant baseline, equal", []float64{0, 0, 0}, 0, 0, 0, 0, 3},
		{"NaN samples dropped", []float64{1, math.NaN(), 3, math.Inf(1)}, 2, 2, 1, 0, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := RobustZ(tc.samples, tc.x)
			if r.N != tc.n || math.Abs(r.Median-tc.median) > 1e-9 || math.Abs(r.MAD-tc.mad) > 1e-9 || math.Abs(r.Z-tc.z) > 1e-6 {
				t.Fatalf("got %+v", r)
			}
		})
	}
	if r := RobustZ(nil, 1); r.N != 0 || !math.IsNaN(r.Z) {
		t.Fatalf("empty: %+v", r)
	}
}

func TestAnomalyBreached(t *testing.T) {
	tests := []struct {
		dir             string
		z, th, dev, min float64
		want            bool
	}{
		{"up", 4, 3, 0.05, 0, true},
		{"up", -4, 3, -0.05, 0, false},
		{"down", -4, 3, -0.05, 0, true},
		{"both", -4, 3, -0.05, 0, true},
		{"", 2, 3, 0.05, 0, false},
		{"up", 10, 3, 0.004, 0.01, false}, // below minDeviation
		{"up", math.NaN(), 3, 1, 0, false},
	}
	for _, tc := range tests {
		if got := AnomalyBreached(tc.dir, tc.z, tc.th, tc.dev, tc.min); got != tc.want {
			t.Errorf("%+v = %v", tc, got)
		}
	}
}

func TestAnomalyRanges(t *testing.T) {
	to := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	rs := AnomalyRanges(to, 5*time.Minute, 4)
	if len(rs) != 4 {
		t.Fatal(len(rs))
	}
	if want := to.AddDate(0, 0, -7).Add(-65 * time.Minute); !rs[0][0].Equal(want) || !rs[3][1].Equal(to.AddDate(0, 0, -28).Add(time.Hour)) {
		t.Fatalf("%v", rs)
	}
}

func TestJobWindowAndConditions(t *testing.T) {
	mk := func(kind, q, c string) *model.Monitor {
		m := &model.Monitor{Kind: kind}
		_ = json.Unmarshal([]byte(q), &m.Query)
		_ = json.Unmarshal([]byte(c), &m.Condition)
		return m
	}
	tests := []struct {
		name   string
		m      *model.Monitor
		window time.Duration
		th     float64
	}{
		{"interval + grace", mk("job_missed", `{"intervalMin":60,"graceMin":5}`, `{}`), 65 * time.Minute, 65},
		{"default grace 10%", mk("job_missed", `{"intervalMin":30}`, `{}`), 33 * time.Minute, 33},
		{"grace at least 1m", mk("job_missed", `{"intervalMin":5}`, `{}`), 6 * time.Minute, 6},
		{"window only", mk("job_missed", `{}`, `{"window":"2h"}`), 2 * time.Hour, 120},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w := JobWindow(tc.m); w != tc.window {
				t.Fatalf("window %v", w)
			}
			if c := Condition(tc.m); c.Threshold != tc.th || c.Op != ">" {
				t.Fatalf("cond %+v", c)
			}
		})
	}
	if c := Condition(mk("anomaly", `{}`, `{"forMin":5}`)); c.Threshold != 3 {
		t.Fatalf("anomaly default z %v", c.Threshold)
	}
	c := mk("composite", `{"op":"or","monitorIds":["12", 13]}`, `{"renotifyMin":30,"stream":true}`)
	if len(c.Query.MonitorIDs) != 2 || c.Query.MonitorIDs[0] != 12 || c.Query.MonitorIDs[1] != 13 || c.Condition.Renotify != 30*time.Minute || !c.Condition.Stream {
		t.Fatalf("decode %+v %+v", c.Query, c.Condition)
	}
}

func TestOldestAgeSeries(t *testing.T) {
	s, err := lag.Series(lag.OldestAge, lag.Inputs{Lag: []float64{1, 2, 3}, OldestAge: []float64{10, -1, 900}})
	if err != nil || s[0] != 10 || s[1] != 0 || s[2] != 900 {
		t.Fatalf("%v %v", s, err)
	}
	ev := lag.Evaluate(s, 600, 1, 1)
	if !ev.FiringNow || ev.Run != 1 {
		t.Fatalf("%+v", ev)
	}
	for sys, want := range map[string]bool{"aws_sqs": true, "gcp_pubsub": true, "rabbitmq": true, "kafka": false} {
		if HasOldestAge(sys) != want {
			t.Errorf("%s", sys)
		}
	}
}
