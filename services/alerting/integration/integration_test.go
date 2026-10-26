//go:build integration

// Package integration runs the alert engine and notifier against live Postgres + ClickHouse
// (deploy/docker-compose.yml). Synthetic data lives in org/project 9201 and is removed afterwards
// (set TT_ITEST_KEEP=1 to keep it).
//
//	GOWORK=off go test -tags integration -count=1 -v ./integration
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/pkg/config"

	"github.com/tasktrooper/telemetry/services/alerting/internal/conn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/engine"
	"github.com/tasktrooper/telemetry/services/alerting/internal/leader"
	"github.com/tasktrooper/telemetry/services/alerting/internal/notify"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

const pid = 9201

var chTables = []string{
	"spans", "trace_index", "red_1m", "red_1h", "consumer_1m", "edges_1m", "callers_1m", "errors_1h", "logs", "log_patterns_1m",
	"broker_lag_1m", "broker_groups_1m", "rum_events", "rum_views_1m", "rum_vitals_1m", "rum_resources_1m", "sli_1m", "deploys",
}

func cleanup(t *testing.T, ctx context.Context, pg *pgxpool.Pool, ch driver.Conn) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM projects WHERE id = 9201`, // cascades monitors, incidents (+events, notifications), kpis, channels
		`DELETE FROM orgs WHERE id = 9201`,
	} {
		if _, err := pg.Exec(ctx, q); err != nil {
			t.Fatalf("pg cleanup: %v", err)
		}
	}
	mctx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 2}))
	for _, tbl := range chTables {
		if err := ch.Exec(mctx, "ALTER TABLE telemetry."+tbl+" DELETE WHERE project_id = 9201"); err != nil {
			t.Fatalf("ch cleanup %s: %v", tbl, err)
		}
	}
}

// span describes a batch of synthetic spans: perMin spans per minute over [from, from+minutes).
type span struct {
	service, kind, route, method, entryRoute, caller string
	dep, depKind, depService, topic, group, version  string
	entry                                            bool
	durMs                                            float64
	errEvery, dlqEvery                               int
}

func insertSpans(t *testing.T, ctx context.Context, ch driver.Conn, s span, from time.Time, minutes, perMin int) {
	t.Helper()
	q := `INSERT INTO telemetry.spans (org_id, project_id, ts, duration_ns, trace_id, span_id, parent_span_id, service, env, version, kind, name,
		is_entry, sample_weight, route, http_method, status_code, is_error, entry_route, entry_method, caller, caller_kind, dep, dep_kind, dep_service,
		topic, consumer_group, dlq)
	SELECT 9201, 9201, toDateTime64(?, 6) + toIntervalMillisecond(number * ?), toUInt64(? * 1000000),
		toFixedString(unhex(leftPad(hex(number), 32, '0')), 16), toFixedString(unhex(leftPad(hex(number), 16, '0')), 8), toFixedString(unhex('0000000000000000'), 8),
		?, 'production', ?, ?, ?, ?, 1, ?, ?,
		if(? > 0 AND number % ? = 0, 500, 200), if(? > 0 AND number % ? = 0, 1, 0),
		?, ?, ?, if(? = '', '', 'service'), ?, ?, ?, ?, ?, if(? > 0 AND number % ? = 0, 1, 0)
	FROM numbers(?)`
	entry := uint8(0)
	if s.entry {
		entry = 1
	}
	err := ch.Exec(ctx, q,
		from.Unix(), 60000/perMin, s.durMs,
		s.service, s.version, s.kind, s.method+" "+s.route, entry, s.route, s.method,
		s.errEvery, s.errEvery, s.errEvery, s.errEvery,
		s.entryRoute, s.method, s.caller, s.caller, s.dep, s.depKind, s.depService, s.topic, s.group, s.dlqEvery, s.dlqEvery,
		minutes*perMin)
	if err != nil {
		t.Fatalf("insert spans %s/%s: %v", s.service, s.kind, err)
	}
}

func mustExec(t *testing.T, ctx context.Context, ch driver.Conn, q string, args ...any) {
	t.Helper()
	if err := ch.Exec(ctx, q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

type monitorDef struct {
	name, kind               string
	target, query, condition string
	channels                 []string
	wantState                string
}

func TestEngineEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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

	cleanup(t, ctx, pg, ch)
	if os.Getenv("TT_ITEST_KEEP") == "" {
		t.Cleanup(func() { cleanup(t, context.Background(), pg, ch) })
	}

	// webhook receiver for the notifier
	var mu sync.Mutex
	var hooks []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		hooks = append(hooks, m)
		mu.Unlock()
	}))
	defer srv.Close()

	// ── Postgres fixtures ──
	for _, q := range []string{
		`INSERT INTO orgs (id, name) VALUES (9201, 'itest')`,
		`INSERT INTO projects (id, org_id, name, slug) VALUES (9201, 9201, 'itest', 'itest')`,
	} {
		if _, err := pg.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Exec(ctx, `INSERT INTO notification_channels (project_id, name, type, config) VALUES (9201, '#itest-oncall', 'webhook', $1)`,
		map[string]any{"url": srv.URL + "/hook"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	cur := now.Truncate(time.Minute).Add(-15 * time.Minute) // "current" window start (RCA)
	base := cur.Add(-60 * time.Minute)

	// ── ClickHouse fixtures ──
	// checkout-api POST /checkout → HTTP · itest-pay (POST /charge) → postgres · pay-db
	// baseline: fast, no errors · current: slow, 50 % errors (pay-db got 50× slower; itest-pay itself fails)
	for _, w := range []struct {
		from       time.Time
		mins       int
		root, http float64
		charge, db float64
		errEvery   int
		version    string
	}{
		{base, 60, 100, 50, 40, 10, 0, "v1"},
		{cur, 15, 600, 540, 530, 500, 2, "v2"},
	} {
		insertSpans(t, ctx, ch, span{service: "checkout-api", kind: "server", entry: true, route: "/checkout", method: "POST", entryRoute: "/checkout", caller: "web", durMs: w.root, errEvery: w.errEvery, version: "v1"}, w.from, w.mins, 20)
		insertSpans(t, ctx, ch, span{service: "checkout-api", kind: "client", route: "", method: "POST", entryRoute: "/checkout", dep: "HTTP · itest-pay", depKind: "http", depService: "itest-pay", durMs: w.http, errEvery: w.errEvery, version: "v1"}, w.from, w.mins, 20)
		insertSpans(t, ctx, ch, span{service: "itest-pay", kind: "server", entry: true, route: "/charge", method: "POST", entryRoute: "/charge", caller: "checkout-api", durMs: w.charge, errEvery: w.errEvery, version: w.version}, w.from, w.mins, 20)
		insertSpans(t, ctx, ch, span{service: "itest-pay", kind: "client", method: "POST", entryRoute: "/charge", dep: "postgres · pay-db", depKind: "db", durMs: w.db, version: w.version}, w.from, w.mins, 20)
	}
	// consumer with DLQ'd messages
	insertSpans(t, ctx, ch, span{service: "itest-worker-svc", kind: "consumer", entry: true, route: "itest-worker", topic: "orders", group: "itest-group", durMs: 20, errEvery: 3, dlqEvery: 4}, cur, 15, 10)
	// 40 error logs of one new pattern in the last 5 minutes
	mustExec(t, ctx, ch, `INSERT INTO telemetry.logs (org_id, project_id, ts, service, env, severity, body, pattern_id)
		SELECT 9201, 9201, toDateTime64(?, 6) + toIntervalSecond(number * 7), 'checkout-api', 'production', 'error', 'payment declined by provider', 777 FROM numbers(40)`,
		now.Truncate(time.Minute).Add(-5*time.Minute).Unix())
	// broker lag: two partitions, lag growing 2k/min, produce rate 10 msg/s each, for the last 20 minutes incl. the current one
	mustExec(t, ctx, ch, `INSERT INTO telemetry.broker_lag_1m (org_id, project_id, cluster_id, consumer_group, topic, partition, minute, lag, end_offset, committed_offset, consume_rate, produce_rate, unacked)
		SELECT 9201, 9201, 'itest-kafka', 'itest-group', 'orders', p, toDateTime(?) + toIntervalMinute(m), 1000 * (m + 1) * (p + 1), 0, 0, 5, 10, 0
		FROM (SELECT number % 2 AS p, intDiv(number, 2) AS m FROM numbers(40))`, now.Truncate(time.Minute).Add(-19*time.Minute).Unix())
	// RUM: slow LCP on /, 3 crashes over 10 sessions
	mustExec(t, ctx, ch, `INSERT INTO telemetry.rum_events (org_id, project_id, ts, received_at, app, env, platform, type, session_id, view_name, vital_name, vital_value, duration_ms, is_crash)
		SELECT 9201, 9201, toDateTime64(?, 3) + toIntervalSecond(number * 10), now64(3), 'itest-web', 'production', 'web',
			multiIf(number < 10, 'view_start', number < 13, 'error', 'vital'), concat('s', toString(number % 10)), '/',
			if(number >= 13, 'LCP', ''), if(number >= 13, 4000, 0), if(number < 10, 900, 0), if(number >= 10 AND number < 13, 1, 0)
		FROM numbers(25)`, now.Truncate(time.Minute).Add(-4*time.Minute).Unix())

	// ── monitors ──
	ep := `{"service":"checkout-api","endpoint":"POST /checkout"}`
	defs := []monitorDef{
		{"Checkout error rate", "error_rate", ep, `{}`, `{"op":">","threshold":0.1,"forMin":0,"window":"5m"}`, []string{"#itest-oncall", "#nope"}, "firing"},
		{"Checkout p95", "latency", ep, `{"percentile":"p95"}`, `{"op":">","threshold":300,"forMin":0}`, nil, "firing"},
		{"Checkout apdex", "apdex", ep, `{"tMs":500}`, `{"threshold":0.7}`, nil, "firing"},
		{"Checkout traffic drop", "throughput", ep, `{"direction":"drop","baselineOffset":"30m"}`, `{"threshold":0.5}`, nil, "ok"},
		{"Checkout errors in logs", "log_count", `{"service":"checkout-api"}`, `{"minSeverity":"error"}`, `{"threshold":20}`, nil, "firing"},
		{"Payment declined text", "log_match", `{"service":"checkout-api"}`, `{"contains":"Payment Declined"}`, `{"threshold":20}`, nil, "firing"},
		{"New error pattern", "log_pattern", `{"service":"checkout-api"}`, `{}`, `{"threshold":0}`, nil, "firing"},
		{"Orders lag time behind", "consumer_lag", `{}`, `{"clusterId":"itest-kafka","group":"itest-group","topic":"orders","metric":"time_behind","threshold":60,"forMin":5}`, `{}`, nil, "firing"},
		{"Orders lag growing", "consumer_lag", `{}`, `{"clusterId":"itest-kafka","group":"itest-group","metric":"growing","threshold":0,"forMin":3}`, `{}`, nil, "firing"},
		{"Orders partition lag", "consumer_lag", `{}`, `{"clusterId":"itest-kafka","group":"itest-group","metric":"partition_lag","threshold":1000000,"forMin":1}`, `{}`, nil, "ok"},
		{"Worker DLQ", "dlq", `{"service":"itest-worker-svc","process":"itest-worker"}`, `{}`, `{"threshold":0}`, nil, "firing"},
		{"Homepage LCP", "rum_vital", `{"service":"itest-web","view":"/"}`, `{"vital":"LCP"}`, `{"threshold":2500}`, nil, "firing"},
		{"Web crash rate", "crash_rate", `{"service":"itest-web"}`, `{}`, `{"threshold":0.1}`, nil, "firing"},
		{"Anomaly (no baseline)", "anomaly", ep, `{}`, `{"threshold":1}`, nil, "no_data"},
	}
	ids := map[string]int64{}
	for _, d := range defs {
		var id int64
		err := pg.QueryRow(ctx, `INSERT INTO monitors (project_id, name, kind, target, query, condition, severity, channels, source)
			VALUES (9201, $1, $2, $3, $4, $5, 'critical', $6, 'manual') RETURNING id`,
			d.name, d.kind, d.target, d.query, d.condition, append([]string{}, d.channels...)).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids[d.name] = id
	}
	var kpiID int64
	if err := pg.QueryRow(ctx, `INSERT INTO kpis (project_id, name, target, sli, good_when, objective, "window", source)
		VALUES (9201, 'Checkout succeeds', '{"kind":"endpoint","service":"checkout-api","ref":"POST /checkout"}', 'availability', '{"statusBelow":500}', 0.99, '28d', 'manual')
		RETURNING id`).Scan(&kpiID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(ctx, `INSERT INTO kpi_alert_rules (kpi_id, kind, long_window, short_window, threshold, severity, channels)
		VALUES ($1, 'burn_rate', '1h', '5m', 6, 'critical', '{#itest-oncall}')`, kpiID); err != nil {
		t.Fatal(err)
	}

	// ── one evaluation cycle ──
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := obs.NewRegistry()
	el := &leader.Elector{Pool: pg, Base: leader.DefaultBase + 9201_000, Shards: 1}
	eng := engine.New(engine.Config{ProjectIDs: []int64{pid}, PublicURL: "http://dash.local"}, log, &store.Store{DB: pg}, ch, el, reg)
	eng.Now = func() time.Time { return now }
	defer el.Release()
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if reg.Get("tt_alert_leader") != 1 {
		t.Fatal("engine did not become leader")
	}

	for _, d := range defs {
		var state string
		var last *float64
		if err := pg.QueryRow(ctx, `SELECT state, last_value FROM monitors WHERE id = $1`, ids[d.name]).Scan(&state, &last); err != nil {
			t.Fatal(err)
		}
		lv := "nil"
		if last != nil {
			lv = fmt.Sprintf("%.4g", *last)
		}
		t.Logf("%-26s %-13s state=%-8s value=%s", d.name, d.kind, state, lv)
		if state != d.wantState {
			t.Errorf("%s: state %s want %s", d.name, state, d.wantState)
		}
	}

	// incident + RCA + outbox for the error-rate monitor
	errID := ids["Checkout error rate"]
	var incID int64
	var rc []byte
	var peak float64
	if err := pg.QueryRow(ctx, `SELECT id, root_cause, peak_value FROM incidents WHERE monitor_id = $1 AND state = 'firing'`, errID).Scan(&incID, &rc, &peak); err != nil {
		t.Fatalf("incident: %v", err)
	}
	var rcErr struct {
		Summary string `json:"summary"`
		Root    struct{ Kind, Service, Label string }
		Path    []json.RawMessage
	}
	_ = json.Unmarshal(rc, &rcErr)
	t.Logf("error RCA: %s (peak %.3f)", rcErr.Summary, peak)
	if rcErr.Root.Service != "itest-pay" || len(rcErr.Path) < 2 {
		t.Errorf("error RCA should point into itest-pay: %s", rc)
	}
	var latRC []byte
	if err := pg.QueryRow(ctx, `SELECT root_cause FROM incidents WHERE monitor_id = $1 AND state = 'firing'`, ids["Checkout p95"]).Scan(&latRC); err != nil {
		t.Fatal(err)
	}
	var rcLat struct {
		Summary string `json:"summary"`
		Root    struct{ Label string }
	}
	_ = json.Unmarshal(latRC, &rcLat)
	t.Logf("latency RCA: %s", rcLat.Summary)
	if rcLat.Root.Label != "postgres · pay-db" || !strings.HasPrefix(rcLat.Summary, "Caused by itest-pay's database pay-db") {
		t.Errorf("latency RCA should blame pay-db: %s", latRC)
	}
	var events int
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM incident_events WHERE incident_id = $1 AND kind = 'fired'`, incID).Scan(&events)
	if events != 1 {
		t.Errorf("fired events = %d", events)
	}
	var n int
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE incident_id = $1 AND kind = 'firing' AND dedupe_key LIKE $2`, incID, "incident:%:firing:%").Scan(&n)
	if n != 2 {
		t.Errorf("firing notifications = %d want 2", n)
	}

	// KPI burn-rate incident + sli_1m
	var kpiInc int64
	if err := pg.QueryRow(ctx, `SELECT id FROM incidents WHERE kpi_id = $1 AND state = 'firing'`, kpiID).Scan(&kpiInc); err != nil {
		t.Errorf("kpi incident: %v", err)
	}
	var sliRows uint64
	_ = ch.QueryRow(ctx, `SELECT count() FROM telemetry.sli_1m WHERE project_id = 9201 AND kpi_id = ?`, uint64(kpiID)).Scan(&sliRows)
	if sliRows < 60 {
		t.Errorf("sli_1m rows = %d", sliRows)
	}

	// second tick must not duplicate incidents or notifications
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE project_id = 9201`).Scan(&n)
	firing := 0
	for _, d := range defs {
		if d.wantState == "firing" {
			firing++
		}
	}
	if n != firing+1 {
		t.Errorf("incidents = %d want %d", n, firing+1)
	}

	// ── notifier ──
	w := &notify.Worker{DB: pg, D: notify.NewDeliverer(notify.SMTPConfig{}), Log: log, Reg: reg}
	if _, err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	var sent, failed int
	_ = pg.QueryRow(ctx, `SELECT count(*) FILTER (WHERE sent_at IS NOT NULL), count(*) FILTER (WHERE error LIKE 'unknown notification channel%')
		FROM notifications n JOIN incidents i ON i.id = n.incident_id WHERE i.project_id = 9201`).Scan(&sent, &failed)
	mu.Lock()
	got := len(hooks)
	mu.Unlock()
	if sent != 2 || failed != 1 || got != 2 {
		t.Errorf("notifier: sent=%d unknown-channel=%d webhook calls=%d", sent, failed, got)
	}

	// ── recovery: healthy traffic later on, then two ticks (clear → hysteresis → resolve) ──
	later := now.Add(20 * time.Minute)
	insertSpans(t, ctx, ch, span{service: "checkout-api", kind: "server", entry: true, route: "/checkout", method: "POST", entryRoute: "/checkout", caller: "web", durMs: 90, version: "v1"},
		now.Truncate(time.Minute).Add(5*time.Minute), 25, 20)
	eng.Now = func() time.Time { return later }
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	eng.Now = func() time.Time { return later.Add(90 * time.Second) }
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var incState string
	_ = pg.QueryRow(ctx, `SELECT state FROM incidents WHERE id = $1`, incID).Scan(&incState)
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE incident_id = $1 AND kind = 'resolved'`, incID).Scan(&n)
	if incState != "resolved" || n != 2 {
		t.Errorf("after recovery: incident %s, resolved notifications %d", incState, n)
	}
	if _, err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var resolvedHook bool
	for _, h := range hooks {
		if h["event"] == "resolved" {
			resolvedHook = true
		}
	}
	if !resolvedHook {
		t.Error("no resolved webhook delivered")
	}
}
