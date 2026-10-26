package kpi

import (
	"math"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

func uniform(maxMs float64) []float64 {
	v := make([]float64, len(eval.GridLevels))
	for i, l := range eval.GridLevels {
		v[i] = l * maxMs
	}
	return v
}

func TestGood(t *testing.T) {
	tests := []struct {
		name string
		sli  string
		gw   model.GoodWhen
		a    Agg
		want float64
	}{
		{"availability", "availability", model.GoodWhen{}, Agg{Total: 100, Bad: 3}, 97},
		{"success never negative", "success_rate", model.GoodWhen{}, Agg{Total: 5, Bad: 9}, 0},
		{"latency 300ms of uniform 0..1000", "latency", model.GoodWhen{LatencyMs: 300}, Agg{Total: 100, Quantiles: uniform(1000)}, 30},
		{"latency excludes failures", "latency", model.GoodWhen{LatencyMs: 500}, Agg{Total: 100, Bad: 20, Quantiles: uniform(1000)}, 40},
		{"lcp threshold wins", "latency", model.GoodWhen{LatencyMs: 100, LcpMs: 2500}, Agg{Total: 10, Quantiles: uniform(5000)}, 5},
		{"default threshold", "latency", model.GoodWhen{}, Agg{Total: 10, Quantiles: uniform(600)}, 5},
		{"no quantiles", "latency", model.GoodWhen{LatencyMs: 100}, Agg{Total: 10, Quantiles: []float64{math.NaN()}}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Good(tc.sli, tc.gw, tc.a); math.Abs(got-tc.want) > 0.01 {
				t.Fatalf("good %v want %v", got, tc.want)
			}
		})
	}
}

func TestToMinutesAndSupported(t *testing.T) {
	k := &model.Kpi{SLI: "availability", Target: model.KpiTarget{Kind: "endpoint", Service: "orders-api", Ref: "POST /v1/orders"}}
	m0 := time.Unix(0, 0)
	ms := ToMinutes(k, []Agg{{Minute: m0, Total: 0}, {Minute: m0.Add(time.Minute), Total: 10, Bad: 1}})
	if len(ms) != 1 || ms[0].Good != 9 || ms[0].Total != 10 {
		t.Fatalf("minutes %+v", ms)
	}
	if !Supported(k) {
		t.Fatal("endpoint should be supported")
	}
	for _, kind := range []string{"journey", "app_start"} {
		if Supported(&model.Kpi{Target: model.KpiTarget{Kind: kind, Service: "x"}}) {
			t.Fatalf("%s should be unsupported", kind)
		}
	}
	if !Supported(&model.Kpi{Target: model.KpiTarget{Kind: "page", ServiceID: "shop-web"}}) {
		t.Fatal("serviceId fallback")
	}
}
