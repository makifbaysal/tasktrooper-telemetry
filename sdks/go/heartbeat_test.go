package trooper_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	trooper "github.com/tasktrooper/telemetry-go"
)

// heartbeats returns the heartbeat values and the resource service.instance.id of the captured metric exports.
func heartbeats(t *testing.T, reqs []captured) (vals []int64, instance string) {
	t.Helper()
	for _, r := range reqs {
		var req colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(r.body, &req); err != nil {
			t.Fatal(err)
		}
		for _, rm := range req.ResourceMetrics {
			for _, kv := range rm.Resource.Attributes {
				if kv.Key == "service.instance.id" {
					instance = kv.Value.GetStringValue()
				}
			}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != trooper.HeartbeatMetric {
						continue
					}
					if sm.Scope.GetName() != trooper.MeterName {
						t.Errorf("heartbeat scope = %q", sm.Scope.GetName())
					}
					for _, p := range m.GetGauge().GetDataPoints() {
						if len(p.Attributes) != 0 {
							t.Errorf("heartbeat attributes = %v", p.Attributes)
						}
						vals = append(vals, p.GetAsInt())
					}
				}
			}
		}
	}
	return vals, instance
}

func TestHeartbeat(t *testing.T) {
	defer trooper.SetFirstExportDelay(50 * time.Millisecond)()
	tests := []struct {
		name     string
		cfg      trooper.Config
		wait     time.Duration // before checking (no shutdown flush)
		wantBeat bool
	}{
		{name: "early first export without traffic", cfg: trooper.Config{Service: "idle", MetricInterval: time.Hour}, wait: 2 * time.Second, wantBeat: true},
		{name: "metrics disabled", cfg: trooper.Config{Service: "idle", DisableMetrics: true}, wait: 300 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ing := &fakeIngest{}
			srv := httptest.NewServer(ing)
			defer srv.Close()
			tt.cfg.DisableRuntimeMetrics, tt.cfg.DisableLogs, tt.cfg.DisableTraces = true, true, true
			shutdown := startAgainst(t, srv, tt.cfg)
			defer func() { _ = shutdown(context.Background()) }()
			deadline := time.Now().Add(tt.wait)
			for time.Now().Before(deadline) && len(ing.byPath("/v1/metrics")) == 0 {
				time.Sleep(20 * time.Millisecond)
			}
			vals, instance := heartbeats(t, ing.byPath("/v1/metrics"))
			if got := len(vals) > 0; got != tt.wantBeat {
				t.Fatalf("heartbeat exported = %v, want %v", got, tt.wantBeat)
			}
			for _, v := range vals {
				if v != 1 {
					t.Errorf("heartbeat = %d", v)
				}
			}
			if tt.wantBeat && len(instance) != 36 {
				t.Errorf("service.instance.id = %q", instance)
			}
		})
	}
}
