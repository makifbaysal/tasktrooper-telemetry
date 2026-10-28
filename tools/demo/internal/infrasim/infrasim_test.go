package infrasim

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// contract metric names (contracts §20, §21) + collector health.
var allowed = map[string]bool{"trooper.collector.up": true}

func init() {
	for _, n := range strings.Fields(`trooper.host.cpu trooper.host.memory trooper.host.memory_bytes trooper.host.disk
	trooper.host.disk_read trooper.host.disk_write trooper.host.net_in trooper.host.net_out trooper.host.load1 trooper.host.uptime_s
	trooper.host.cpu_cores trooper.container.cpu trooper.container.cpu_limit trooper.container.memory_bytes
	trooper.container.memory_limit_bytes trooper.container.restarts trooper.container.net_in trooper.container.net_out
	trooper.container.throttled trooper.process.cpu trooper.process.memory_bytes trooper.process.threads trooper.process.fds
	trooper.k8s.node.ready trooper.k8s.node.allocatable_cpu trooper.k8s.node.allocatable_memory trooper.k8s.node.pods
	trooper.k8s.pod.ready trooper.k8s.pod.restarts trooper.k8s.pod.cpu_request trooper.k8s.pod.cpu_limit
	trooper.k8s.pod.memory_request trooper.k8s.pod.memory_limit trooper.k8s.workload.desired trooper.k8s.workload.ready
	trooper.k8s.workload.available trooper.k8s.workload.updated
	trooper.faas.invocations trooper.faas.errors trooper.faas.throttles trooper.faas.duration.avg trooper.faas.duration.max
	trooper.faas.duration.p95 trooper.faas.duration.p99 trooper.faas.concurrency trooper.faas.iterator_age_ms
	trooper.faas.memory_bytes trooper.faas.execution_units`) {
		allowed[n] = true
	}
}

type receiver struct {
	mu      sync.Mutex
	metrics []*colmetricspb.ExportMetricsServiceRequest
	logs    []*collogspb.ExportLogsServiceRequest
	keys    []string
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, req.Header.Get("x-trooper-key"))
	switch req.URL.Path {
	case "/v1/metrics":
		m := &colmetricspb.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(b, m); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		r.metrics = append(r.metrics, m)
	case "/v1/logs":
		l := &collogspb.ExportLogsServiceRequest{}
		if err := proto.Unmarshal(b, l); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		r.logs = append(r.logs, l)
	default:
		http.NotFound(w, req)
	}
}

func TestOOMCycle(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	w := NewWorld(now)
	var oom *pod
	for _, p := range w.pods {
		if p.oom {
			oom = p
		}
	}
	if oom == nil || oom.workload != "recommendation-api" || oom.service != "recommendation-api" || oom.kind != "Deployment" {
		t.Fatalf("oom pod = %+v", oom)
	}
	tests := []struct {
		name         string
		oomOn        bool
		minutes      int
		wantRestarts bool
	}{
		{"healthy before the incident", false, 20, false},
		{"reco-db-slow on", true, 20, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := oom.restarts()
			var evs []event
			for i := 0; i < tt.minutes*4; i++ {
				now = now.Add(15 * time.Second)
				evs = append(evs, w.step(now, 15*time.Second, tt.oomOn)...)
			}
			got := oom.restarts() - before
			if tt.wantRestarts != (got > 0) {
				t.Fatalf("restarts += %d", got)
			}
			if !tt.wantRestarts {
				return
			}
			if got < 3 || got > 8 {
				t.Errorf("restarts in 20 min = %d, want a few (one every ~3-4 min)", got)
			}
			if oom.reason != "OOMKilled" {
				t.Errorf("reason = %q", oom.reason)
			}
			reasons := map[string]int{}
			for _, e := range evs {
				reasons[e.reason]++
			}
			if reasons["BackOff"] != got || reasons["OOMKilling"] != got {
				t.Errorf("events = %v, want %d each", reasons, got)
			}
		})
	}
}

func TestSendsContractTelemetry(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	rt := &world.Runtime{Scenarios: map[string]int64{"reco-db-slow": 0, "payments-provider-errors": 0}}
	s := New(srv.URL, "tt_dev_ingest_key", rt, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	now := time.Now()
	if err := s.S.Metrics(ctx, s.hostBatches(now)...); err != nil {
		t.Fatal(err)
	}
	if err := s.S.Metrics(ctx, s.clusterBatch(now), s.faas.batch(now.Truncate(time.Minute), rt)); err != nil {
		t.Fatal(err)
	}
	evs := s.scenarioEvents(now, 3)
	if err := s.S.Logs(ctx, []KV{{"service.name", "k8s"}, {"k8s.cluster.name", ClusterName}}, eventLogs(evs)); err != nil {
		t.Fatal(err)
	}

	resources := map[string]map[string]string{}
	seen := map[string]bool{}
	slugs := map[string]bool{}
	for _, req := range rcv.metrics {
		for _, rm := range req.ResourceMetrics {
			res := map[string]string{}
			for _, kv := range rm.Resource.Attributes {
				res[kv.Key] = kv.Value.GetStringValue()
			}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if !allowed[m.Name] {
						t.Errorf("metric %s is not in contracts §20/§21", m.Name)
					}
					if m.GetGauge() == nil {
						t.Errorf("%s must be a gauge", m.Name)
					}
					seen[m.Name] = true
					if strings.HasPrefix(m.Name, "trooper.faas.") {
						if res["service.name"] != "trooper-cloud" || res["cloud.provider"] != "aws" {
							t.Errorf("faas resource = %v", res)
						}
						for _, dp := range m.GetGauge().DataPoints {
							for _, kv := range dp.Attributes {
								if kv.Key == "trooper.faas.id" {
									slugs[kv.Value.GetStringValue()] = true
								}
							}
						}
					}
				}
			}
			resources[res["host.name"]+"|"+res["service.name"]] = res
		}
	}
	for n := range allowed {
		if n == "trooper.faas.memory_bytes" || n == "trooper.faas.execution_units" { // Azure-only
			continue
		}
		if !seen[n] {
			t.Errorf("metric %s never sent", n)
		}
	}
	if len(slugs) != 4 {
		t.Errorf("functions = %v", slugs)
	}
	for s := range slugs {
		if !strings.HasPrefix(s, "aws-eu-central-1-") {
			t.Errorf("slug %s", s)
		}
	}
	node := resources["ip-10-0-1-23.eu-central-1.compute.internal|trooper-agent"]
	if node["k8s.cluster.name"] != ClusterName || node["k8s.node.name"] == "" || node["host.id"] == "" || node["os.type"] != "linux" {
		t.Errorf("node resource = %v", node)
	}
	if ci := resources["ci-runner-1|trooper-agent"]; ci == nil || ci["k8s.cluster.name"] != "" {
		t.Errorf("docker host resource = %v", ci)
	}
	if len(rcv.logs) != 1 || rcv.logs[0].ResourceLogs[0].Resource.Attributes[0].Value.GetStringValue() != "k8s" {
		t.Fatalf("logs = %v", rcv.logs)
	}
	lr := rcv.logs[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if lr.SeverityText != "warn" || !strings.Contains(lr.Body.GetStringValue(), "Readiness probe failed") {
		t.Errorf("log = %v", lr)
	}
	for _, k := range rcv.keys {
		if k != "tt_dev_ingest_key" {
			t.Errorf("key = %q", k)
		}
	}
}

func TestPodNamesAndWorkloads(t *testing.T) {
	w := NewWorld(time.Now())
	names := map[string]bool{}
	for _, p := range w.pods {
		if names[p.name] {
			t.Errorf("duplicate pod %s", p.name)
		}
		names[p.name] = true
		if p.kind == "Deployment" && !strings.HasPrefix(p.name, p.workload+"-") {
			t.Errorf("pod %s does not belong to %s", p.name, p.workload)
		}
	}
	perNode := map[string]int{}
	for _, p := range w.pods {
		perNode[p.node]++
	}
	if len(perNode) != 3 {
		t.Errorf("pods spread over %d nodes", len(perNode))
	}
	for _, s := range world.Services {
		found := false
		for _, wl := range w.workloads {
			if wl.name == s.Name && wl.kind == "Deployment" && wl.ns == Namespace {
				found = true
			}
		}
		if !found {
			t.Errorf("no Deployment for %s", s.Name)
		}
	}
}

// TestUpsertFunctions writes the 4 functions into the platform Postgres (opt-in: TT_DEMO_PG_TEST=1,
// URL from TT_POSTGRES_URL or the demo default). Idempotent, like the demo itself.
func TestUpsertFunctions(t *testing.T) {
	if os.Getenv("TT_DEMO_PG_TEST") == "" {
		t.Skip("set TT_DEMO_PG_TEST=1 to run against the platform Postgres")
	}
	pg := os.Getenv("TT_POSTGRES_URL")
	if pg == "" {
		pg = world.DefaultPostgres
	}
	fns := newFaasWorld().fns
	for range 2 { // twice: the second run updates
		n, err := UpsertFunctions(context.Background(), pg, fns)
		if err != nil {
			t.Fatal(err)
		}
		if n != -1 && n != len(fns) {
			t.Fatalf("upserted %d", n)
		}
	}
}
