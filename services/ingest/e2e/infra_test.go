//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/ingest/internal/app"
)

// TestInfraFamilies: trooper-agent host/cluster metrics, trooper-cloud faas metrics, a Kubernetes Warning event and an
// OBI (eBPF) span go through ingest-gw (internal credential, project 9601) → Redpanda → ingest-writer → the typed
// tables of deploy/clickhouse/003_infra_k8s_faas.sql, logs and spans. Rows are tagged with a random suffix and deleted.
func TestInfraFamilies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pg, err := pgxpool.New(ctx, app.GatewayFromEnv().Common.PostgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var created bool
	if err := pg.QueryRow(ctx, `WITH ins AS (INSERT INTO projects (id, org_id, name, slug) VALUES (9601, 1, 'infra-e2e', 'infra-e2e-9601')
		ON CONFLICT (id) DO NOTHING RETURNING id) SELECT count(*) > 0 FROM ins`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created {
		defer func() { _, _ = pg.Exec(context.Background(), `DELETE FROM projects WHERE id = 9601`) }()
	}
	var org uint32
	if err := pg.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = 9601`).Scan(&org); err != nil {
		t.Fatal(err)
	}

	wcfg := app.WriterFromEnv()
	wcfg.HealthAddr, wcfg.MetricsAddr = "", ""
	wcfg.Writer.FlushInterval = 500 * time.Millisecond
	wctx, wstop := context.WithCancel(ctx)
	wdone := make(chan error, 1)
	go func() { wdone <- app.RunWriter(wctx, wcfg, log) }()
	defer func() { wstop(); <-wdone }()

	gcfg := app.GatewayFromEnv()
	gcfg.GW.HTTPAddr, gcfg.GW.GRPCAddr, gcfg.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0", ""
	gcfg.InternalToken = "e2e-internal-token"
	addrs := make(chan string, 1)
	gctx, gstop := context.WithCancel(ctx)
	gdone := make(chan error, 1)
	go func() { gdone <- app.RunGateway(gctx, gcfg, log, func(h, _ string) { addrs <- h }) }()
	defer func() { gstop(); <-gdone }()
	var base string
	select {
	case a := <-addrs:
		base = "http://" + a
	case err := <-gdone:
		t.Fatalf("gateway failed: %v", err)
	}

	sfx := fmt.Sprintf("e2e%d", rand.Uint32())
	host, cluster, fn, svc := "host-"+sfx, "k8s-"+sfx, "aws-eu-west-1-"+sfx, "ebpf-"+sfx
	ts := time.Now().Add(-time.Minute).Truncate(time.Minute).Add(10 * time.Second).UnixNano()
	kvs := func(pairs ...string) string {
		var b strings.Builder
		for i := 0; i+1 < len(pairs); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"key":%q,"value":{"stringValue":%q}}`, pairs[i], pairs[i+1])
		}
		return "[" + b.String() + "]"
	}
	gauge := func(name string, v float64, attrs string) string {
		return fmt.Sprintf(`{"name":%q,"gauge":{"dataPoints":[{"timeUnixNano":"%d","asDouble":%g,"attributes":%s}]}}`, name, ts, v, attrs)
	}
	resMetrics := func(res string, ms ...string) string {
		return fmt.Sprintf(`{"resource":{"attributes":%s},"scopeMetrics":[{"metrics":[%s]}]}`, res, strings.Join(ms, ","))
	}
	agentRes := kvs("service.name", "trooper-agent", "host.name", host, "host.id", "mid-"+sfx, "os.type", "linux", "host.arch", "arm64",
		"k8s.cluster.name", cluster, "k8s.node.name", host)
	ctr := kvs("container.id", "cid-"+sfx, "container.name", "api", "container.image.name", "api:1", "k8s.namespace.name", "shop",
		"k8s.pod.name", "api-"+sfx, "k8s.pod.uid", "uid-"+sfx, "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", "api",
		"trooper.service", "orders-"+sfx)
	pod := kvs("k8s.namespace.name", "shop", "k8s.pod.name", "api-"+sfx, "k8s.pod.uid", "uid-"+sfx, "k8s.pod.phase", "Running",
		"trooper.k8s.pod.reason", "OOMKilled", "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", "api", "trooper.service", "orders-"+sfx)
	wl := kvs("k8s.namespace.name", "shop", "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", "api")
	metrics := `{"resourceMetrics":[` + resMetrics(agentRes,
		gauge("trooper.host.cpu", 0.93, "[]"), gauge("trooper.host.disk", 0.5, "[]"),
		gauge("trooper.container.cpu", 0.4, ctr), gauge("trooper.container.cpu_limit", 0.5, ctr),
		gauge("trooper.process.cpu", 1.25, `[{"key":"process.pid","value":{"intValue":"4242"}},{"key":"process.executable.name","value":{"stringValue":"java"}}]`),
		gauge("trooper.k8s.node.ready", 0, "[]"),
		gauge("trooper.k8s.pod.ready", 0, pod), gauge("trooper.k8s.pod.restarts", 7, pod),
		gauge("trooper.k8s.workload.desired", 3, wl), gauge("trooper.k8s.workload.available", 2, wl),
	) + "," + resMetrics(kvs("service.name", "trooper-cloud", "cloud.provider", "aws", "cloud.region", "eu-west-1"),
		gauge("trooper.faas.invocations", 120, kvs("trooper.faas.id", fn, "faas.name", sfx)),
		gauge("trooper.faas.errors", 6, kvs("trooper.faas.id", fn, "faas.name", sfx)),
	) + `]}`
	event := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":%s},"scopeLogs":[{"logRecords":[{"timeUnixNano":"%d","severityNumber":13,
		"body":{"stringValue":"Back-off restarting failed container %s"},"attributes":%s}]}]}]}`,
		kvs("service.name", "k8s", "k8s.cluster.name", cluster), ts, sfx,
		kvs("k8s.event.reason", "BackOff", "k8s.event.type", "Warning", "k8s.object.kind", "Pod", "k8s.object.name", "api-"+sfx, "k8s.namespace.name", "shop", "k8s.event.count", "3"))
	span := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":%s},"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c",
		"spanId":"eee19b7ec3c1b174","name":"GET /pay","kind":2,"startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":%s}]}]}]}`,
		kvs("service.name", svc, "telemetry.sdk.name", "opentelemetry-ebpf-instrumentation"), ts, ts+int64(5*time.Millisecond),
		kvs("http.route", "/pay", "http.request.method", "GET"))

	for path, body := range map[string]string{"/v1/metrics": metrics, "/v1/logs": event, "/v1/traces": span} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-trooper-internal", gcfg.InternalToken)
		req.Header.Set("x-trooper-project", "9601")
		if code := do(t, req); code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
	}

	opts, err := clickhouse.ParseDSN(wcfg.Common.ClickHouseURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		for _, s := range []string{
			"ALTER TABLE hosts_1m DELETE WHERE project_id = 9601 AND host = '" + host + "'",
			"ALTER TABLE containers_1m DELETE WHERE project_id = 9601 AND host = '" + host + "'",
			"ALTER TABLE processes_1m DELETE WHERE project_id = 9601 AND host = '" + host + "'",
			"ALTER TABLE k8s_nodes_1m DELETE WHERE project_id = 9601 AND cluster = '" + cluster + "'",
			"ALTER TABLE k8s_pods_1m DELETE WHERE project_id = 9601 AND cluster = '" + cluster + "'",
			"ALTER TABLE k8s_workloads_1m DELETE WHERE project_id = 9601 AND cluster = '" + cluster + "'",
			"ALTER TABLE faas_1m DELETE WHERE project_id = 9601 AND function_id = '" + fn + "'",
			"ALTER TABLE logs DELETE WHERE project_id = 9601 AND service = 'k8s' AND attrs['k8s.cluster.name'] = '" + cluster + "'",
			"ALTER TABLE log_patterns_1m DELETE WHERE project_id = 9601 AND service = 'k8s'",
			"ALTER TABLE spans DELETE WHERE project_id = 9601 AND service = '" + svc + "'",
			"ALTER TABLE red_1m DELETE WHERE project_id = 9601 AND service = '" + svc + "'",
			"ALTER TABLE red_1h DELETE WHERE project_id = 9601 AND service = '" + svc + "'",
			"ALTER TABLE callers_1m DELETE WHERE project_id = 9601 AND service = '" + svc + "'",
			"ALTER TABLE trace_index DELETE WHERE project_id = 9601 AND has(services, '" + svc + "')",
		} {
			if err := conn.Exec(cctx, s); err != nil {
				t.Logf("cleanup (ignored): %v: %s", err, s)
			}
		}
	}()

	tenant := fmt.Sprintf("org_id = %d AND project_id = 9601", org)
	checks := []struct{ name, q string }{
		{"hosts_1m", "SELECT count() FROM hosts_1m WHERE " + tenant + " AND host = '" + host + "' AND cluster = '" + cluster + "' AND os = 'linux' AND cpu = 0.93 AND isInfinite(load1)"},
		{"containers_1m", "SELECT count() FROM containers_1m WHERE " + tenant + " AND host = '" + host + "' AND pod = 'api-" + sfx + "' AND service = 'orders-" + sfx + "' AND cpu = 0.4 AND cpu_limit = 0.5"},
		{"processes_1m", "SELECT count() FROM processes_1m WHERE " + tenant + " AND host = '" + host + "' AND pid = 4242 AND exe = 'java' AND cpu = 1.25"},
		{"k8s_nodes_1m", "SELECT count() FROM k8s_nodes_1m WHERE " + tenant + " AND cluster = '" + cluster + "' AND node = '" + host + "' AND ready = 0"},
		{"k8s_pods_1m", "SELECT count() FROM k8s_pods_1m WHERE " + tenant + " AND cluster = '" + cluster + "' AND pod_uid = 'uid-" + sfx + "' AND reason = 'OOMKilled' AND phase = 'Running' AND restarts = 7 AND ready = 0 AND node = '" + host + "'"},
		{"k8s_workloads_1m", "SELECT count() FROM k8s_workloads_1m WHERE " + tenant + " AND cluster = '" + cluster + "' AND kind = 'Deployment' AND name = 'api' AND desired - available = 1"},
		{"faas_1m", "SELECT count() FROM faas_1m WHERE " + tenant + " AND function_id = '" + fn + "' AND provider = 'aws' AND region = 'eu-west-1' AND invocations = 120 AND errors = 6"},
		{"k8s event log", "SELECT count() FROM logs WHERE " + tenant + " AND service = 'k8s' AND severity = 'warn' AND attrs['k8s.cluster.name'] = '" + cluster + "' AND attrs['k8s.event.reason'] = 'BackOff' AND attrs['k8s.object.name'] = 'api-" + sfx + "'"},
		{"ebpf span", "SELECT count() FROM spans WHERE " + tenant + " AND service = '" + svc + "' AND is_entry = 1 AND resource_attrs['telemetry.sdk.name'] = 'opentelemetry-ebpf-instrumentation'"},
	}
	deadline := time.Now().Add(60 * time.Second)
	for _, c := range checks {
		for {
			var n uint64
			err := conn.QueryRow(ctx, c.q).Scan(&n)
			if err == nil && n > 0 {
				t.Logf("✓ %-18s %d", c.name, n)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no rows (err=%v): %s", c.name, err, c.q)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}
