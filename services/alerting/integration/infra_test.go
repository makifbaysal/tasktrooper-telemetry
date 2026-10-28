//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/pkg/config"

	"github.com/tasktrooper/telemetry/services/alerting/internal/conn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/engine"
	"github.com/tasktrooper/telemetry/services/alerting/internal/leader"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

var infraTables = []string{"hosts_1m", "containers_1m", "k8s_nodes_1m", "k8s_pods_1m", "k8s_workloads_1m", "faas_1m", "spans", "red_1m",
	"red_1h", "callers_1m", "edges_1m", "errors_1h", "trace_index"}

func cleanupInfra(t *testing.T, ctx context.Context, pg *pgxpool.Pool, ch driver.Conn) {
	t.Helper()
	for _, q := range []string{`DELETE FROM projects WHERE id = 9601`, `DELETE FROM orgs WHERE id = 9601`} {
		if _, err := pg.Exec(ctx, q); err != nil {
			t.Fatalf("pg cleanup: %v", err)
		}
	}
	mctx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 2}))
	for _, tbl := range infraTables {
		if err := ch.Exec(mctx, "ALTER TABLE telemetry."+tbl+" DELETE WHERE project_id = 9601"); err != nil {
			t.Fatalf("ch cleanup %s: %v", tbl, err)
		}
	}
}

// TestInfraFaaS (org/project 9601): synthetic hosts / containers / pods / nodes / workloads / functions in the
// 003_infra_k8s_faas.sql tables; infra and faas monitors fire with the worst entity and detail.entities, unreported
// gauges are no_data, and a service error-rate incident gets the OOMKilled pod as its root cause.
func TestInfraFaaS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	common := config.Load()
	pg, err := conn.Postgres(ctx, common.PostgresURL)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(pg.Close)
	ch, err := conn.ClickHouse(ctx, common.ClickHouseURL)
	if err != nil {
		t.Skipf("clickhouse unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pg.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	cleanupInfra(t, ctx, pg, ch)
	if os.Getenv("TT_ITEST_KEEP") == "" {
		t.Cleanup(func() { cleanupInfra(t, context.Background(), pg, ch) })
	}
	exec(`INSERT INTO orgs (id, name) VALUES (9601, 'itest infra')`)
	exec(`INSERT INTO projects (id, org_id, name, slug) VALUES (9601, 9601, 'itest infra', 'itest-infra')`)
	var hasFns bool
	_ = pg.QueryRow(ctx, `SELECT to_regclass('serverless_functions') IS NOT NULL`).Scan(&hasFns)
	if hasFns {
		exec(`INSERT INTO serverless_functions (project_id, provider, slug, name, region, timeout_s, service) VALUES
			(9601, 'aws', 'it-fn-a', 'a', 'eu-west-1', 1, 'it-fn-svc'), (9601, 'aws', 'it-fn-b', 'b', 'eu-west-1', 30, 'it-fn-b')`)
	}

	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	to := now.Truncate(time.Minute)
	from := to.Add(-10 * time.Minute).Unix()
	ins := func(q string, args ...any) { mustExec(t, ctx, ch, q, args...) }
	// 10 minutes of data; number = minute index (9 = newest complete minute)
	ins(`INSERT INTO telemetry.hosts_1m (org_id, project_id, cluster, host, node, os, minute, cpu, disk, load1)
		SELECT 9601, 9601, 'itc', h, h, 'linux', toDateTime(?) + number * 60, if(h = 'it-h1', 0.95, 0.3), 0.5, -inf
		FROM numbers(10) ARRAY JOIN ['it-h1', 'it-h2'] AS h`, from)
	ins(`INSERT INTO telemetry.containers_1m (org_id, project_id, cluster, host, namespace, pod, container, service, minute, cpu, cpu_limit, restarts, memory_bytes, memory_limit, throttled)
		SELECT 9601, 9601, 'itc', h, 'shop', p, 'api', 'it-svc', toDateTime(?) + number * 60, if(p = 'api-1', 0.45, 0.1), 0.5,
			if(p = 'api-1', intDiv(number, 2), 0), -inf, -inf, -inf
		FROM numbers(10) ARRAY JOIN ['it-h1', 'it-h2'] AS h, ['api-1', 'api-2'] AS p`, from)
	ins(`INSERT INTO telemetry.k8s_pods_1m (org_id, project_id, cluster, namespace, pod, pod_uid, node, workload_kind, workload, service, minute,
			phase, reason, ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit)
		SELECT 9601, 9601, 'itc', 'shop', p, p || '-uid', h, 'Deployment', 'api', 'it-svc', toDateTime(?) + number * 60, 'Running',
			if(p = 'api-1' AND number >= 7, 'OOMKilled', ''), if(p = 'api-1' AND number = 9, 0, 1), if(p = 'api-1', intDiv(number, 2), 0),
			-inf, 0.5, -inf, -inf
		FROM numbers(10) ARRAY JOIN ['it-h1', 'it-h2'] AS h, ['api-1', 'api-2'] AS p`, from)
	// a deleted pod that was not ready 9 minutes ago must not count
	ins(`INSERT INTO telemetry.k8s_pods_1m (org_id, project_id, cluster, namespace, pod, pod_uid, node, service, minute, phase, reason, ready, restarts,
			cpu_request, cpu_limit, memory_request, memory_limit)
		VALUES (9601, 9601, 'itc', 'shop', 'old-1', 'old-uid', 'it-h1', 'it-svc', toDateTime(?), 'Pending', '', 0, 0, -inf, -inf, -inf, -inf)`, from)
	ins(`INSERT INTO telemetry.k8s_nodes_1m (org_id, project_id, cluster, node, minute, ready, allocatable_cpu, allocatable_memory, pods)
		SELECT 9601, 9601, 'itc', h, toDateTime(?) + number * 60, if(h = 'it-h2' AND number >= 8, 0, 1), 4, 8e9, 10
		FROM numbers(10) ARRAY JOIN ['it-h1', 'it-h2'] AS h`, from)
	ins(`INSERT INTO telemetry.k8s_workloads_1m (org_id, project_id, cluster, namespace, kind, name, minute, desired, ready, available, updated)
		SELECT 9601, 9601, 'itc', 'shop', 'Deployment', 'api', toDateTime(?) + number * 60, 2, if(number = 9, 1, 2), if(number = 9, 1, 2), 2
		FROM numbers(10)`, from)
	ins(`INSERT INTO telemetry.faas_1m (org_id, project_id, function_id, provider, region, minute, invocations, errors, throttles, duration_avg,
			duration_max, duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units)
		SELECT 9601, 9601, f, 'aws', 'eu-west-1', toDateTime(?) + number * 60, 100, if(f = 'it-fn-a', 10, 0), -inf, 300, 950,
			if(f = 'it-fn-a', 900, 100), 990, 4, -inf, -inf, -inf
		FROM numbers(10) ARRAY JOIN ['it-fn-a', 'it-fn-b'] AS f`, from)
	ins(`INSERT INTO telemetry.spans (org_id, project_id, ts, duration_ns, trace_id, span_id, parent_span_id, service, env, kind, name, is_entry,
			sample_weight, route, http_method, status_code, attrs)
		SELECT 9601, 9601, toDateTime64(?, 6) + toIntervalSecond(number * 50), 1000000, toFixedString(unhex(leftPad(hex(number + 77), 32, '0')), 16),
			toFixedString(unhex(leftPad(hex(number + 77), 16, '0')), 8), toFixedString(unhex('0000000000000000'), 8), 'it-fn-svc', 'production',
			'server', 'handler', 1, 1, '', '', 200, map('faas.coldstart', 'true')
		FROM numbers(3)`, to.Add(-4*time.Minute).Unix())
	insertSpansIn(t, ctx, ch, 9601, span{service: "it-svc", kind: "server", entry: true, route: "/pay", method: "POST", entryRoute: "/pay",
		caller: "web", durMs: 40, errEvery: 2}, to.Add(-10*time.Minute), 10, 20)

	type mon struct {
		id                     int64
		kind, target, q, cond string
		want                   string
		value                  float64
		top                    string // detail.entities[0].id
	}
	mons := []mon{
		{9601101, "infra", `{}`, `{"entity":"pod","metric":"restarts"}`, `{"op":">","threshold":3,"window":"10m"}`, "firing", 4, "shop/api-1"},
		{9601102, "infra", `{}`, `{"entity":"pod","metric":"not_ready","filter":{"cluster":"itc"}}`, `{"op":">","threshold":0}`, "firing", 1, "shop/api-1"},
		{9601103, "infra", `{}`, `{"entity":"node","metric":"not_ready"}`, `{"op":">","threshold":0}`, "firing", 1, "it-h2"},
		{9601104, "infra", `{"service":"it-svc"}`, `{"entity":"host","metric":"cpu","agg":"avg"}`, `{"op":">","threshold":0.9}`, "firing", 0.95, "it-h1"},
		{9601105, "infra", `{}`, `{"entity":"container","metric":"cpu_ratio","agg":"max","filter":{"namespace":"shop"}}`, `{"op":">","threshold":0.8}`, "firing", 0.9, "shop/api-1/api"},
		{9601106, "infra", `{}`, `{"entity":"workload","metric":"unavailable","filter":{"service":"it-svc"}}`, `{"op":">","threshold":0}`, "firing", 1, "shop/Deployment/api"},
		{9601107, "infra", `{}`, `{"entity":"host","metric":"load1"}`, `{"op":">","threshold":4}`, "no_data", 0, ""},
		{9601108, "infra", `{}`, `{"entity":"pod","metric":"memory_ratio"}`, `{"op":">","threshold":0.9}`, "no_data", 0, ""},
		{9601109, "faas", `{}`, `{"metric":"error_rate","filter":{"provider":"aws"}}`, `{"op":">","threshold":0.05,"minRequests":20}`, "firing", 0.1, "it-fn-a"},
		{9601110, "faas", `{}`, `{"metric":"throttles"}`, `{"op":">","threshold":0}`, "no_data", 0, ""},
		{9601111, "infra", `{}`, `{"entity":"host","metric":"disk","agg":"max"}`, `{"op":">","threshold":0.9}`, "ok", 0.5, ""},
		{9601112, "error_rate", `{"service":"it-svc","endpoint":"POST /pay"}`, `{}`, `{"op":">","threshold":0.2}`, "firing", 0.5, ""},
	}
	if hasFns {
		mons = append(mons,
			mon{9601113, "faas", `{}`, `{"functionId":"it-fn-a","metric":"duration_p95","relativeToTimeout":true}`, `{"op":">","threshold":0.8}`, "firing", 0.9, "it-fn-a"},
			mon{9601114, "faas", `{}`, `{"functionId":"it-fn-a","metric":"cold_starts"}`, `{"op":">","threshold":2}`, "firing", 3, "it-fn-a"},
			mon{9601115, "faas", `{"service":"it-fn-b"}`, `{"metric":"duration_p95"}`, `{"op":">","threshold":500}`, "ok", 100, ""},
		)
	} else {
		t.Log("serverless_functions missing (011 not applied): faas lookups skipped")
	}
	for _, m := range mons {
		exec(`INSERT INTO monitors (id, project_id, name, kind, target, query, condition, severity, channels, source)
			VALUES ($1, 9601, $2, $3, $4, $5, $6, 'critical', '{webhook:itest}', 'template')`, m.id, "infra "+itoa(m.id), m.kind, m.target, m.q, m.cond)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	el := &leader.Elector{Pool: pg, Base: leader.DefaultBase + 9601_101, Shards: 1}
	defer el.Release()
	eng := engine.New(engine.Config{ProjectIDs: []int64{9601}}, log, &store.Store{DB: pg}, ch, el, obs.NewRegistry())
	eng.Now = func() time.Time { return now }
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	for _, m := range mons {
		var state string
		var last *float64
		if err := pg.QueryRow(ctx, `SELECT state, last_value FROM monitors WHERE id = $1`, m.id).Scan(&state, &last); err != nil {
			t.Fatal(err)
		}
		if state != m.want {
			t.Errorf("monitor %d (%s %s): state %s, want %s", m.id, m.kind, m.q, state, m.want)
			continue
		}
		if m.want != "no_data" && (last == nil || abs(*last-m.value) > 1e-6) {
			t.Errorf("monitor %d: value %v, want %v", m.id, last, m.value)
		}
		if m.top == "" {
			continue
		}
		var data []byte
		if err := pg.QueryRow(ctx, `SELECT e.data FROM incident_events e JOIN incidents i ON i.id = e.incident_id
			WHERE i.monitor_id = $1 AND e.kind = 'fired' ORDER BY e.id LIMIT 1`, m.id).Scan(&data); err != nil {
			t.Errorf("monitor %d: fired event: %v", m.id, err)
			continue
		}
		var ev struct {
			Detail struct {
				Entities []map[string]any `json:"entities"`
			} `json:"detail"`
		}
		_ = json.Unmarshal(data, &ev)
		if len(ev.Detail.Entities) == 0 || ev.Detail.Entities[0]["id"] != m.top {
			t.Errorf("monitor %d: entities %s, want top %s", m.id, data, m.top)
		}
	}

	// RCA: the service incident's likely cause is the OOMKilled pod (call tree: own code only)
	var summary string
	var conf float64
	var rc []byte
	if err := pg.QueryRow(ctx, `SELECT root_cause->>'summary', (root_cause->>'confidence')::float8, root_cause FROM incidents WHERE monitor_id = 9601112`).
		Scan(&summary, &conf, &rc); err != nil {
		t.Fatal(err)
	}
	if summary != "Pod api-1 restarted (OOMKilled)." || conf < 0.6 || conf > 0.85 || !strings.Contains(string(rc), `"kind": "host"`) {
		t.Errorf("root cause %q %.2f: %s", summary, conf, rc)
	}
	var payload string
	if err := pg.QueryRow(ctx, `SELECT coalesce(n.payload->'rootCause'->>'summary', '') FROM notifications n JOIN incidents i ON i.id = n.incident_id WHERE i.monitor_id = 9601112 AND n.kind = 'firing' LIMIT 1`).
		Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != summary {
		t.Errorf("firing payload lacks the infra cause: %s", payload)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
