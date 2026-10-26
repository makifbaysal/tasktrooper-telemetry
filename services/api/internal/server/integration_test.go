//go:build integration

// Integration test against live infra (deploy/docker-compose.yml). It owns org/project 9101: it seeds
// Postgres + ClickHouse for that tenant, exercises the API through the real store and PG auth, and deletes
// everything it wrote. Run: GOWORK=off go test -tags integration ./internal/server/ -run Integration -v
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/conntest"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/symbolicate"
	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
)

const (
	itTenant = 9101
	itKey    = "tt_it_9101_api_key"
)

// seedCH generates ~50 minutes of telemetry for tenant 9101 (all rows relative to now()).
var seedCH = []string{
	// HTTP entry spans: it-api GET /orders/:id called by it-web, 5% 500s.
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, version, kind, name, is_entry,
	   route, http_method, status_code, is_error, error_type, error_fingerprint, entry_route, entry_method, caller, caller_kind, attrs)
	 SELECT 9101, 9101, now64(6) - toIntervalSecond(number * 5), (20 + number % 80) * 1000000,
	   MD5(toString(number)), substring(MD5(toString(number)), 1, 8), 'it-api', 'prod', if(number < 300, 'v2', 'v1'), 'server', 'GET /orders/:id', 1,
	   '/orders/:id', 'GET', if(number % 20 = 0, 500, 200), number % 20 = 0, if(number % 20 = 0, 'OrderError', ''), if(number % 20 = 0, 777, 0),
	   '/orders/:id', 'GET', 'it-web', 'web', map('exception.message', if(number % 20 = 0, 'order lookup failed', ''),
	   'exception.stacktrace', if(number % 20 = 0, 'OrderError: order lookup failed\n    at lookup (dist/orders.js:1:4242)', ''))
	 FROM numbers(600)`,
	// DB client spans under the same traces.
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, parent_span_id, service, env, kind, name, is_entry,
	   entry_route, entry_method, dep, dep_kind, db_query_hash, attrs)
	 SELECT 9101, 9101, now64(6) - toIntervalSecond(number * 5) + toIntervalMillisecond(2), (5 + number % 10) * 1000000,
	   MD5(toString(number)), substring(MD5(concat('db', toString(number))), 1, 8), substring(MD5(toString(number)), 1, 8),
	   'it-api', 'prod', 'client', 'SELECT orders', 0, '/orders/:id', 'GET', 'postgres · it-orders', 'db', 4242,
	   map('db.query.text', 'select *  from orders where id = 42')
	 FROM numbers(600)`,
	// Producer spans.
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, parent_span_id, service, env, kind, name, is_entry,
	   entry_route, entry_method, dep, dep_kind, topic)
	 SELECT 9101, 9101, now64(6) - toIntervalSecond(number * 5) + toIntervalMillisecond(3), 2000000,
	   MD5(toString(number)), substring(MD5(concat('p', toString(number))), 1, 8), substring(MD5(toString(number)), 1, 8),
	   'it-api', 'prod', 'producer', 'orders.created publish', 0, '/orders/:id', 'GET', 'kafka · orders.created', 'queue', 'orders.created'
	 FROM numbers(600)`,
	// Consumer process spans in it-worker.
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry,
	   route, is_error, topic, consumer_group, attempt, dlq, messaging_system)
	 SELECT 9101, 9101, now64(6) - toIntervalSecond(number * 5) + toIntervalMillisecond(50), (30 + number % 50) * 1000000,
	   MD5(concat('c', toString(number))), substring(MD5(concat('c', toString(number))), 1, 8),
	   'it-worker', 'prod', 'consumer', 'process-order', 1, 'process-order', number % 50 = 0, 'orders.created', 'it-fulfil',
	   if(number % 10 = 0, 2, 1), number % 100 = 0, 'kafka'
	 FROM numbers(600)`,
	// SQS consumer in it-mailer (messaging.system aws_sqs).
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry,
	   route, topic, consumer_group, messaging_system)
	 SELECT 9101, 9101, now64(6) - toIntervalSecond(number * 20), 15000000, MD5(concat('q', toString(number))),
	   substring(MD5(concat('q', toString(number))), 1, 8), 'it-mailer', 'prod', 'consumer', 'send-mail', 1, 'send-mail', 'mail-queue', '', 'aws_sqs'
	 FROM numbers(100)`,
	`INSERT INTO broker_lag_1m (org_id, project_id, cluster_id, consumer_group, topic, partition, minute, lag, consume_rate, produce_rate, oldest_age_s)
	 SELECT 9101, 9101, 'it-sqs', 'mail-queue', 'mail-queue', 0, toStartOfMinute(now()) + toIntervalMinute(1) - toIntervalMinute(number), 40, 2, 2, 610 - number * 10
	 FROM numbers(51)`,
	`INSERT INTO broker_topics_1m (org_id, project_id, cluster_id, topic, minute, partitions, replication, retention_ms, avg_msg_bytes, in_rate, bytes_in_rate, size_bytes)
	 SELECT 9101, 9101, 'it-kafka', 'orders.created', toStartOfMinute(now()) - toIntervalMinute(number), 3, 3, 604800000, 1800, 30, 54000, -inf
	 FROM numbers(10)`,
	// A topic nobody consumes (only broker_topics_1m knows it).
	`INSERT INTO broker_topics_1m (org_id, project_id, cluster_id, topic, minute, partitions, replication, retention_ms, avg_msg_bytes, in_rate, bytes_in_rate, size_bytes)
	 VALUES (9101, 9101, 'it-kafka', 'orders.audit', toStartOfMinute(now()), 1, 3, -1, 200, 0.5, 100, 1e6)`,
	`INSERT INTO logs (org_id, project_id, ts, service, env, severity, body, trace_id, route, pattern_id)
	 SELECT 9101, 9101, now64(6) - toIntervalSecond(number * 10), 'it-api', 'prod',
	   if(number % 10 = 0, 'error', 'info'), if(number % 10 = 0, concat('order ', toString(number), ' failed'), 'served order'),
	   MD5(toString(number)), '/orders/:id', if(number % 10 = 0, 11, 12)
	 FROM numbers(300)`,
	`INSERT INTO rum_events (org_id, project_id, ts, received_at, app, env, platform, type, session_id, view_id, view_name,
	   anonymous_id, country, device_type, browser, duration_ms, vital_name, vital_value, error_type, error_message, error_fingerprint,
	   resource_url, resource_method, status_code, error_stack, app_version)
	 SELECT 9101, 9101, now64(3) - toIntervalSecond(number * 10), now64(3), 'it-web', 'prod', 'web',
	   ['view_start', 'view_end', 'vital', 'error', 'resource'][number % 5 + 1],
	   concat('s', toString(intDiv(number, 20))), concat('v', toString(intDiv(number, 5))), '/orders/:id',
	   concat('a', toString(intDiv(number, 20))), 'TR', 'desktop', 'Chrome', 300 + number % 400,
	   if(number % 5 = 2, 'LCP', ''), if(number % 5 = 2, 1500 + number % 2000, 0),
	   if(number % 5 = 3, 'TypeError', ''), if(number % 5 = 3, 'x is undefined', ''), if(number % 5 = 3, 99, 0),
	   if(number % 5 = 4, '/api/orders/:id', ''), if(number % 5 = 4, 'GET', ''), if(number % 5 = 4, 200, 0),
	   if(number % 5 = 3, 'TypeError: x is undefined\n    at a (assets/app.min.js:1:200)', ''), 'web-1.4.0'
	 FROM numbers(300)`,
	`INSERT INTO rum_events (org_id, project_id, ts, received_at, app, env, platform, type, session_id, view_name, anonymous_id, duration_ms, name)
	 SELECT 9101, 9101, now64(3) - toIntervalSecond(number * 30), now64(3), 'it-mobile', 'prod', 'android', 'app_start',
	   concat('m', toString(number)), 'Home', concat('m', toString(number)), 800 + number * 10, if(number % 3 = 0, 'warm', 'cold')
	 FROM numbers(60)`,
	`INSERT INTO db_1m (org_id, project_id, database_id, engine, node, role, minute, cpu, memory, disk, connections, max_connections,
	   ops, p95_ms, slow, hit_ratio, evictions, repl_lag_ms)
	 SELECT 9101, 9101, 'it-pg', 'postgres', n, if(n = 'it-pg-1', 'primary', 'replica'), toStartOfMinute(now()) - toIntervalMinute(m),
	   0.4 + (m % 20) / 100, 0.6, 0.55, 30, 200, 1200, 4.5, 1, 0.99, 0, if(n = 'it-pg-1', 0, 12)
	 FROM (SELECT number AS m FROM numbers(50)) ARRAY JOIN ['it-pg-1', 'it-pg-2'] AS n`,
	// pg_stat_statements style text; spans carry a literal, mapping is by normalized text.
	`INSERT INTO db_queries_1m (org_id, project_id, database_id, query_hash, minute, query, calls, total_ms, p95_ms, rows)
	 SELECT 9101, 9101, 'it-pg', 987654321, toStartOfMinute(now()) - toIntervalMinute(number), 'SELECT * FROM orders WHERE id = $1', 12, 60, 9, 12
	 FROM numbers(50)`,
	`INSERT INTO broker_lag_1m (org_id, project_id, cluster_id, consumer_group, topic, partition, minute, lag, end_offset,
	   committed_offset, consume_rate, produce_rate)
	 SELECT 9101, 9101, 'it-kafka', 'it-fulfil', 'orders.created', p, toStartOfMinute(now()) + toIntervalMinute(1) - toIntervalMinute(m),
	   100 + (50 - m) * 20, 100000 - m * 600, 100000 - m * 600 - (100 + (50 - m) * 20), 9, 10
	 FROM (SELECT number AS m FROM numbers(51)) ARRAY JOIN [0, 1, 2] AS p`,
	`INSERT INTO broker_groups_1m (org_id, project_id, cluster_id, consumer_group, minute, state, members, rebalances)
	 SELECT 9101, 9101, 'it-kafka', 'it-fulfil', toStartOfMinute(now()) - toIntervalMinute(number), 'Stable', 3, 0 FROM numbers(50)`,
	`INSERT INTO broker_nodes_1m (org_id, project_id, cluster_id, node, minute, cpu, memory, disk, net_in)
	 SELECT 9101, 9101, 'it-kafka', 'broker-1', toStartOfMinute(now()) - toIntervalMinute(number), 0.35, 0.5, 0.4, 12 FROM numbers(50)`,
}

func itEnv(t *testing.T) (*pgxpool.Pool, driver.Conn) {
	t.Helper()
	cfg := config.Load()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	opts, err := clickhouse.ParseDSN(cfg.ClickHouseURL)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Ping(ctx); err != nil {
		t.Skipf("clickhouse unavailable: %v", err)
	}
	return pool, ch
}

func itCleanup(t *testing.T, pool *pgxpool.Pool, ch driver.Conn) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, itTenant); err != nil { // cascades to project, keys, monitors, ...
		t.Errorf("pg cleanup: %v", err)
	}
	rows, err := ch.Query(ctx, `SELECT name FROM system.tables WHERE database = 'telemetry' AND engine LIKE '%MergeTree'
	  AND name IN (SELECT table FROM system.columns WHERE database = 'telemetry' AND name = 'project_id')`)
	if err != nil {
		t.Errorf("ch tables: %v", err)
		return
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tb := range tables {
		if err := ch.Exec(ctx, fmt.Sprintf("ALTER TABLE telemetry.`%s` DELETE WHERE project_id = %d SETTINGS mutations_sync = 1", tb, itTenant)); err != nil {
			t.Errorf("ch cleanup %s: %v", tb, err)
		}
	}
}

type itClient struct {
	t    *testing.T
	base string
	key  string
}

func (c itClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// json calls and decodes, failing on an unexpected status.
func (c itClient) json(method, path string, body any, want int, out any) {
	c.t.Helper()
	code, b := c.do(method, path, body)
	if code != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: decode: %v: %s", method, path, err, b)
		}
	}
}

type series struct {
	Timestamps []int64 `json:"timestamps"`
	Series     []struct {
		ID   string    `json:"id"`
		Data []float64 `json:"data"`
	} `json:"series"`
}

func (s series) sum(id string) float64 {
	var v float64
	for _, x := range s.Series {
		if x.ID == id {
			for _, d := range x.Data {
				v += d
			}
		}
	}
	return v
}

func TestIntegration(t *testing.T) {
	pool, ch := itEnv(t)
	defer ch.Close()
	defer pool.Close()
	ctx := context.Background()
	itCleanup(t, pool, ch)
	defer itCleanup(t, pool, ch)

	for _, q := range []string{
		`INSERT INTO orgs (id, name) VALUES (9101, 'it-9101')`,
		`INSERT INTO projects (id, org_id, name, slug) VALUES (9101, 9101, 'it', 'it-9101')`,
		`INSERT INTO api_keys (project_id, name, kind, hash, prefix) VALUES (9101, 'it', 'api', digest('` + itKey + `', 'sha256'), 'tt_it_91')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("pg seed: %v", err)
		}
	}
	for _, q := range seedCH {
		if err := ch.Exec(ctx, q); err != nil {
			t.Fatalf("ch seed: %v\n%s", err, q)
		}
	}

	// symbolicator fake (services/symbolicator is built in parallel; contract §12)
	var symCalls []symbolicate.Request
	sym := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in symbolicate.Request
		_ = json.NewDecoder(r.Body).Decode(&in)
		symCalls = append(symCalls, in)
		if r.Header.Get("Authorization") != "Bearer "+itKey {
			w.WriteHeader(401)
			return
		}
		if in.Release == "web-1.4.0" {
			_ = json.NewEncoder(w).Encode(map[string]any{"stack": "TypeError: x is undefined\n    at render (src/Orders.tsx:42:7)", "symbolicated": true,
				"frames": []map[string]any{{"fn": "render", "file": "src/Orders.tsx", "line": 42, "col": 7, "inApp": true}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"stack": in.Stack, "symbolicated": false, "frames": []any{}})
	}))
	defer sym.Close()
	var cloudSecret string
	st := store.New(ch, pool)
	srv := &Server{Store: st, Auth: auth.NewPG(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Origins: []string{"http://localhost:5173"}, Test: conntest.Run, Symbolicator: symbolicate.New(sym.URL),
		CloudTest: func(_ context.Context, provider string, _ json.RawMessage, secret string) model.IntegrationTestResult {
			cloudSecret = secret // the real checks (cloudcheck) need cloud credentials; unit-tested with httptest there
			return model.IntegrationTestResult{OK: true, Message: "fake " + provider}
		}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := itClient{t: t, base: ts.URL, key: itKey}

	if code, _ := (itClient{t: t, base: ts.URL, key: "nope"}).do("GET", "/services", nil); code != 401 {
		t.Fatalf("bad key: %d", code)
	}

	t.Run("apm", func(t *testing.T) {
		c.t = t
		var svcs []struct {
			ID, Type, Version string
			Rpm, ErrorRate    float64
		}
		c.json("GET", "/services?range=1h", nil, 200, &svcs)
		found := map[string]bool{}
		for _, s := range svcs {
			found[s.ID] = true
			if s.ID == "it-api" && (s.Rpm <= 0 || s.ErrorRate <= 0 || s.Version != "v2") {
				t.Errorf("it-api: %+v", s)
			}
		}
		for _, id := range []string{"it-api", "it-worker", "it-web"} {
			if !found[id] {
				t.Errorf("service %s missing from %+v", id, svcs)
			}
		}
		var red series
		c.json("GET", "/services/it-api/series?range=1h", nil, 200, &red)
		if len(red.Timestamps) == 0 || red.sum("rpm") <= 0 {
			t.Errorf("red series empty: %+v", red)
		}
		for _, x := range red.Series {
			if len(x.Data) != len(red.Timestamps) {
				t.Errorf("series %s not zero-filled", x.ID)
			}
		}
		var eps []struct{ ID string }
		c.json("GET", "/services/it-api/endpoints?range=1h", nil, 200, &eps)
		if len(eps) != 1 || eps[0].ID != "get-orders-id" {
			t.Errorf("endpoints: %+v", eps)
		}
		var procs []struct{ ID string }
		c.json("GET", "/services/it-worker/endpoints?range=1h", nil, 200, &procs)
		if len(procs) != 1 || procs[0].ID != "process-order" {
			t.Errorf("processes: %+v", procs)
		}
		var callers []struct{ Rpm float64 }
		c.json("GET", "/services/it-api/endpoints/get-orders-id/callers?range=1h", nil, 200, &callers)
		if len(callers) == 0 {
			t.Errorf("no callers")
		}
		var deps []struct{ Name string }
		c.json("GET", "/services/it-api/dependencies?range=1h", nil, 200, &deps)
		if len(deps) < 2 {
			t.Errorf("deps: %+v", deps)
		}
		c.json("GET", "/services/it-api/dependencies/"+strings.ReplaceAll("postgres · it-orders", " ", "%20")+"/series?range=1h", nil, 200, nil)
		for _, p := range []string{"/callers/series", "/breakdown", "/breakdown/series", "/calls", "/series"} {
			c.json("GET", "/services/it-api/endpoints/get-orders-id"+p+"?range=6h", nil, 200, nil)
		}
		c.json("GET", "/services/it-worker/endpoints/process-order/callers?range=1h", nil, 200, nil)
		// client apps get RUM-based series; consumers list their topic sources as callers
		var webRed series
		c.json("GET", "/services/it-web/series?range=1h", nil, 200, &webRed)
		if webRed.sum("rpm") <= 0 {
			t.Errorf("client app series all zero: %+v", webRed)
		}
		var mailerCallers []struct {
			Caller      struct{ Name string }
			Rpm         float64
			TopEndpoint *struct{ Rpm, P95 float64 }
		}
		c.json("GET", "/services/it-mailer/callers?range=1h", nil, 200, &mailerCallers)
		if len(mailerCallers) == 0 || mailerCallers[0].Rpm <= 0 {
			t.Errorf("consumer callers: %+v", mailerCallers)
		}
		var mailerCS series
		c.json("GET", "/services/it-mailer/callers/series?range=1h", nil, 200, &mailerCS)
		if len(mailerCS.Series) == 0 {
			t.Errorf("consumer caller series empty")
		}
		var apiCallers []struct {
			TopEndpoint *struct{ Rpm, P95 float64 }
		}
		c.json("GET", "/services/it-api/callers?range=1h", nil, 200, &apiCallers)
		if len(apiCallers) == 0 || apiCallers[0].TopEndpoint == nil || apiCallers[0].TopEndpoint.Rpm <= 0 || apiCallers[0].TopEndpoint.P95 <= 0 {
			t.Errorf("topEndpoint stats: %+v", apiCallers)
		}
		var rca model.EndpointRca
		c.json("GET", "/services/it-api/endpoints/get-orders-id/rca?range=1h&baseline=1d", nil, 200, &rca)
		if rca.Current.Requests <= 0 || rca.Current.P95 <= 0 || len(rca.Children) < 2 || rca.Children[0].Current.CallsPerReq <= 0 || rca.BaselineTo != rca.To-86400000 {
			t.Errorf("rca: %+v", rca)
		}
		c.json("GET", "/services/it-worker/endpoints/process-order/rca?range=1h", nil, 200, &rca)
		if rca.Current.Requests <= 0 {
			t.Errorf("process rca: %+v", rca)
		}
		c.json("GET", "/services/it-api/endpoints/get-orders-id/rca?range=1h&baseline=2w", nil, 400, nil)
		c.json("GET", "/services/it-api?range=7d", nil, 200, nil)
		c.json("GET", "/overview/series?range=24h", nil, 200, nil)
		var deploys []struct{ Version string }
		c.json("GET", "/services/it-api/deploys?range=24h", nil, 200, &deploys)
		if len(deploys) != 2 {
			t.Errorf("deploys: %+v", deploys)
		}

		var traces []struct {
			ID      string
			IsError bool
		}
		c.json("GET", "/traces?range=1h&service=it-api&status=error", nil, 200, &traces)
		if len(traces) == 0 || !traces[0].IsError {
			t.Fatalf("traces: %+v", traces)
		}
		var tr struct{ Spans []struct{ Service string } }
		c.json("GET", "/traces/"+traces[0].ID, nil, 200, &tr)
		if len(tr.Spans) != 3 {
			t.Errorf("trace spans: %d", len(tr.Spans))
		}
		var errs []struct{ Fingerprint, Status string }
		c.json("GET", "/errors?range=24h", nil, 200, &errs)
		if len(errs) != 1 || errs[0].Fingerprint != "777" {
			t.Fatalf("errors: %+v", errs)
		}
		var ed model.ErrorDetail
		c.json("GET", "/errors/777?range=24h", nil, 200, &ed)
		if ed.Release != "v2" && ed.Release != "v1" || !strings.Contains(ed.Stack, "dist/orders.js:1:4242") || ed.Symbolicated || ed.Frames == nil {
			t.Errorf("error detail: %+v", ed)
		}
		if len(symCalls) == 0 || symCalls[len(symCalls)-1].App != "it-api" {
			t.Errorf("symbolicator not called: %+v", symCalls)
		}
		c.json("GET", "/errors/12345?range=24h", nil, 404, nil)
		c.json("PATCH", "/errors/777", map[string]string{"status": "resolved"}, 200, nil)
		c.json("GET", "/errors?range=24h", nil, 200, &errs)
		if errs[0].Status != "resolved" {
			t.Errorf("error status: %+v", errs)
		}
	})

	t.Run("tenant isolation", func(t *testing.T) {
		other := itClient{t: t, base: ts.URL, key: "tt_dev_api_key"}
		code, b := other.do("GET", "/services?range=1h", nil)
		if code == 200 && strings.Contains(string(b), "it-api") {
			t.Errorf("project 1 sees project 9101 data")
		}
	})

	t.Run("logs", func(t *testing.T) {
		c.t = t
		var page struct {
			Lines  []struct{ Level, Message string }
			Levels map[string]int
		}
		c.json("GET", "/logs?range=1h&level=error&q=failed", nil, 200, &page)
		if len(page.Lines) == 0 || page.Lines[0].Level != "error" {
			t.Errorf("logs: %+v", page)
		}
		c.json("GET", "/logs/series?range=1h", nil, 200, nil)
		c.json("GET", "/logs/patterns?range=1h", nil, 200, nil)
	})

	t.Run("rum", func(t *testing.T) {
		c.t = t
		var sum struct {
			ActiveUsers float64
			Vitals      []struct {
				ID    string
				Value float64
			}
		}
		c.json("GET", "/apps/it-web/summary?range=1h", nil, 200, &sum)
		if sum.ActiveUsers == 0 || len(sum.Vitals) == 0 || sum.Vitals[0].Value == 0 {
			t.Errorf("rum summary: %+v", sum)
		}
		for _, p := range []string{"pages", "sessions", "errors", "resources", "series", "vitals/lcp/series", "api-calls"} {
			c.json("GET", "/apps/it-web/"+p+"?range=1h", nil, 200, nil)
		}
		var starts []struct{ Kind string }
		c.json("GET", "/apps/it-mobile/app-starts?range=1h", nil, 200, &starts)
		if len(starts) == 0 {
			t.Errorf("no app starts")
		}
		c.json("GET", "/apps/it-mobile/summary?range=1h", nil, 200, nil)
		var rd model.RumErrorDetail
		c.json("GET", "/apps/it-web/errors/99?range=1h", nil, 200, &rd)
		if !rd.Symbolicated || len(rd.Frames) != 1 || rd.Frames[0].File != "src/Orders.tsx" || rd.Platform != "web" || rd.AppVersion != "web-1.4.0" {
			t.Errorf("rum error detail: %+v", rd)
		}
	})

	t.Run("infra", func(t *testing.T) {
		c.t = t
		c.json("POST", "/infra/databases", map[string]any{"id": "it-pg", "name": "it-pg", "engine": "postgres", "host": "localhost", "port": 5432, "collector": "agent"}, 201, nil)
		c.json("POST", "/infra/databases", map[string]any{"id": "it-pg", "name": "it-pg", "engine": "postgres", "host": "localhost", "port": 5432, "collector": "agent"}, 409, nil)
		var db struct {
			Nodes []struct{ ID string }
		}
		c.json("GET", "/infra/databases/it-pg?range=1h", nil, 200, &db)
		if len(db.Nodes) != 2 {
			t.Errorf("db nodes: %+v", db)
		}
		var dbs []any
		c.json("GET", "/infra/databases?range=1h", nil, 200, &dbs)
		var cs series
		c.json("GET", "/infra/databases/it-pg/series?range=1h&metric=cpu", nil, 200, &cs)
		if cs.sum(cs.Series[0].ID) == 0 {
			t.Errorf("db series empty")
		}
		var qs []struct {
			ID, ServiceID, Endpoint string
		}
		c.json("GET", "/infra/databases/it-pg/queries?range=1h", nil, 200, &qs)
		if len(qs) != 1 || qs[0].ServiceID != "it-api" || qs[0].Endpoint != "GET /orders/:id" {
			t.Errorf("db queries: %+v", qs)
		}
		c.json("GET", "/infra/databases/it-pg/users?range=1h", nil, 200, nil)
		var res struct {
			OK      bool
			Message string
		}
		c.json("POST", "/infra/databases/it-pg/test-connection", map[string]any{"user": "trooper", "password": "trooper", "database": "trooper"}, 200, &res)
		if !res.OK {
			t.Errorf("pg test-connection: %+v", res)
		}

		c.json("POST", "/infra/clusters", map[string]any{"id": "it-kafka", "name": "it-kafka", "kind": "kafka", "endpoint": "localhost:19092", "collector": "agent"}, 201, nil)
		var cl struct {
			TotalLag float64
			Groups   int
		}
		c.json("GET", "/infra/clusters/it-kafka", nil, 200, &cl)
		if cl.TotalLag == 0 {
			t.Errorf("cluster lag: %+v", cl)
		}
		c.json("GET", "/infra/clusters", nil, 200, nil)
		c.json("POST", "/infra/clusters/it-kafka/test-connection", nil, 200, &res)
		if !res.OK {
			t.Errorf("kafka test-connection: %+v", res)
		}
		var groups []struct {
			Group     string `json:"group"`
			ServiceID string `json:"serviceId"`
		}
		c.json("GET", "/infra/clusters/it-kafka/groups", nil, 200, &groups)
		if len(groups) != 1 {
			t.Fatalf("groups: %+v", groups)
		}
		c.json("PUT", "/infra/clusters/it-kafka/groups/it-fulfil/owner", map[string]string{"serviceId": "it-worker", "process": "process-order"}, 204, nil)
		c.json("GET", "/infra/clusters/it-kafka/groups/it-fulfil", nil, 200, nil)
		var lag series
		c.json("GET", "/infra/clusters/it-kafka/groups/it-fulfil/lag?range=1h&by=partition", nil, 200, &lag)
		if len(lag.Series) != 3 {
			t.Errorf("partition lag series: %d", len(lag.Series))
		}
		c.json("GET", "/infra/clusters/it-kafka/topics", nil, 200, nil)
		c.json("GET", "/infra/clusters/it-kafka/topics/orders.created/series?range=1h", nil, 200, nil)
		c.json("GET", "/infra/clusters/it-kafka/nodes/series?range=1h&metric=cpu", nil, 200, nil)
		c.json("GET", "/infra/groups?serviceId=it-worker", nil, 200, &groups)
		if len(groups) != 1 {
			t.Errorf("groups by service: %+v", groups)
		}
		// SQS queue: oldest message age, messaging_system from spans, topics from broker_topics_1m
		c.json("POST", "/infra/clusters", map[string]any{"id": "it-sqs", "name": "it-sqs", "kind": "sqs", "endpoint": "https://sqs.eu-central-1.amazonaws.com", "collector": "cloud"}, 201, nil)
		var sqsGroups []model.ConsumerGroup
		c.json("GET", "/infra/clusters/it-sqs/groups", nil, 200, &sqsGroups)
		if len(sqsGroups) != 1 || sqsGroups[0].Reads[0].OldestAgeS == nil || *sqsGroups[0].Reads[0].OldestAgeS < 600 {
			t.Errorf("sqs oldest age: %+v", sqsGroups)
		}
		var mailer []model.Endpoint
		c.json("GET", "/services/it-mailer/endpoints?range=1h", nil, 200, &mailer)
		if len(mailer) != 1 || mailer[0].Sub == nil || mailer[0].Sub.Source != "sqs" {
			t.Errorf("sqs subscription: %+v", mailer)
		}
		var topics []model.Topic
		c.json("GET", "/infra/clusters/it-kafka/topics", nil, 200, &topics)
		tm := map[string]model.Topic{}
		for _, x := range topics {
			tm[x.Name] = x
		}
		if oc := tm["orders.created"]; oc.Retention != "7d" || oc.AvgBytes != 1800 || oc.InPerSec != 30 || oc.Partitions != 3 || oc.Replication != 3 {
			t.Errorf("topic meta: %+v", oc)
		}
		if au := tm["orders.audit"]; au.Retention != "∞" || au.SizeBytes != 1e6 {
			t.Errorf("unconsumed topic: %+v", au)
		}
		var cc struct {
			ProjectID      int `json:"projectId"`
			Databases      []any
			BrokerClusters []any
		}
		c.json("GET", "/internal/collector-config", nil, 200, &cc)
		if cc.ProjectID != itTenant || len(cc.Databases) != 1 || len(cc.BrokerClusters) != 2 {
			t.Errorf("collector config: %+v", cc)
		}
	})

	t.Run("monitors and incidents", func(t *testing.T) {
		c.t = t
		queue := map[string]any{"clusterId": "it-kafka", "group": "it-fulfil", "topic": "orders.created", "metric": "lag", "threshold": 1000, "forMin": 5}
		var pv struct {
			Data      []float64
			FiringNow bool
		}
		c.json("POST", "/monitors/preview?range=1h", map[string]any{"name": "x", "kind": "consumer_lag", "queue": queue, "severity": "warning"}, 200, &pv)
		if len(pv.Data) == 0 || !pv.FiringNow {
			t.Errorf("lag preview: %+v", pv)
		}
		var m struct{ ID string }
		c.json("POST", "/monitors", map[string]any{"name": "fulfil lag", "kind": "consumer_lag", "queue": queue, "severity": "warning",
			"channels": []string{"#ops"}, "condition": map[string]any{"op": ">", "threshold": 1000, "forMin": 5}}, 201, &m)
		c.json("GET", "/monitors/"+m.ID+"/preview?range=6h", nil, 200, nil)
		var lat struct{ ID string }
		c.json("POST", "/monitors", map[string]any{"name": "orders p95", "kind": "latency", "severity": "critical",
			"target": map[string]any{"service": "it-api", "endpoint": "GET /orders/:id"}, "condition": map[string]any{"op": ">", "threshold": 50, "forMin": 5}}, 201, &lat)
		c.json("GET", "/monitors/"+lat.ID+"/preview?range=1h", nil, 200, &pv)
		if len(pv.Data) == 0 {
			t.Errorf("latency preview empty")
		}
		c.json("PUT", "/monitors/"+lat.ID, map[string]any{"name": "orders p95", "kind": "latency", "severity": "warning", "status": "muted",
			"target": map[string]any{"service": "it-api", "endpoint": "GET /orders/:id"}, "condition": map[string]any{"op": ">", "threshold": 80, "forMin": 5}}, 200, nil)
		var ms []any
		c.json("GET", "/monitors?service=it-api", nil, 200, &ms)
		c.json("DELETE", "/monitors/"+lat.ID, nil, 204, nil)
		c.json("GET", "/monitors/"+lat.ID, nil, 404, nil)

		var incID int64
		if err := pool.QueryRow(ctx, `INSERT INTO incidents (project_id, monitor_id, title, severity, service, state) VALUES (9101, $1, 'lag high', 'warning', 'it-worker', 'firing') RETURNING id`, m.ID).Scan(&incID); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprint(incID)
		var inc struct{ State string }
		c.json("POST", "/incidents/"+id+"/ack", nil, 200, &inc)
		c.json("POST", "/incidents/"+id+"/ack", nil, 409, nil)
		c.json("POST", "/incidents/"+id+"/resolve", nil, 200, &inc)
		if inc.State != "resolved" {
			t.Errorf("incident state: %s", inc.State)
		}
		var evs []any
		c.json("GET", "/incidents/"+id+"/events", nil, 200, &evs)
		if len(evs) != 2 {
			t.Errorf("incident events: %d", len(evs))
		}
		c.json("GET", "/incidents?state=resolved", nil, 200, nil)

		// pending incidents are listed; quiet ones only with includeQuiet
		if _, err := pool.Exec(ctx, `INSERT INTO incidents (project_id, monitor_id, title, severity, service, state) VALUES (9101, $1, 'lag rising', 'warning', 'it-worker', 'pending')`, m.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO incidents (project_id, title, severity, service, state, quiet, resolved_at) VALUES (9101, 'blip', 'warning', 'it-worker', 'resolved', true, now())`); err != nil {
			t.Fatal(err)
		}
		var incs []model.Incident
		c.json("GET", "/incidents?state=pending", nil, 200, &incs)
		if len(incs) != 1 || incs[0].State != "pending" {
			t.Errorf("pending: %+v", incs)
		}
		c.json("GET", "/incidents?state=resolved", nil, 200, &incs)
		n := len(incs)
		c.json("GET", "/incidents?state=resolved&includeQuiet=true", nil, 200, &incs)
		if len(incs) != n+1 || !incs[0].Quiet {
			t.Errorf("quiet: %d vs %d %+v", n, len(incs), incs)
		}

		// mute until / unmute
		until := time.Now().Add(time.Hour).UnixMilli()
		var mon model.Monitor
		c.json("PATCH", "/monitors/"+m.ID, map[string]any{"mutedUntil": until}, 200, &mon)
		if mon.State != "muted" || mon.MutedUntil != until {
			t.Errorf("muted: %+v", mon)
		}
		mon = model.Monitor{}
		c.json("PATCH", "/monitors/"+m.ID, map[string]any{"mutedUntil": nil, "severity": "critical"}, 200, &mon)
		if mon.State == "muted" || mon.MutedUntil != 0 || mon.Severity != "critical" {
			t.Errorf("unmuted: %+v", mon)
		}
		c.json("PATCH", "/monitors/"+m.ID, map[string]any{"severity": "loud"}, 400, nil)

		// previews of the engine kinds
		var p model.Preview
		for _, tc := range []struct {
			body  map[string]any
			check func(model.Preview) bool
		}{
			{map[string]any{"kind": "log_count", "target": map[string]any{"service": "it-api"}, "query": map[string]any{"severity": "error"},
				"condition": map[string]any{"op": ">", "threshold": 1, "forMin": 0}}, func(p model.Preview) bool { return p.Episodes > 0 }},
			{map[string]any{"kind": "log_match", "target": map[string]any{"service": "it-api"}, "query": map[string]any{"contains": "FAILED"},
				"condition": map[string]any{"op": ">", "threshold": 0}}, func(p model.Preview) bool { return p.Episodes > 0 }},
			{map[string]any{"kind": "log_pattern_new", "target": map[string]any{"service": "it-api"},
				"condition": map[string]any{"op": ">", "threshold": 0}}, func(p model.Preview) bool { return p.Episodes == 1 }},
			{map[string]any{"kind": "dlq", "target": map[string]any{"service": "it-worker", "process": "process-order"},
				"condition": map[string]any{"op": ">", "threshold": 0}}, func(p model.Preview) bool { return p.Episodes > 0 }},
			{map[string]any{"kind": "consumer_failures", "target": map[string]any{"service": "it-worker"},
				"condition": map[string]any{"op": ">", "threshold": 0.5, "minRequests": 10}}, func(p model.Preview) bool { return p.Episodes == 0 && len(p.Data) == 60 }},
			{map[string]any{"kind": "rum_vital", "target": map[string]any{"service": "it-web"}, "query": map[string]any{"vital": "LCP"},
				"condition": map[string]any{"op": ">", "threshold": 1000}}, func(p model.Preview) bool { return p.Episodes > 0 }},
			{map[string]any{"kind": "crash_rate", "target": map[string]any{"service": "it-web"},
				"condition": map[string]any{"op": ">", "threshold": 0.5}}, func(p model.Preview) bool { return len(p.Data) == 60 }},
			{map[string]any{"kind": "consumer_lag", "queue": map[string]any{"clusterId": "it-sqs", "group": "mail-queue", "metric": "oldest_age", "threshold": 300, "forMin": 5}},
				func(p model.Preview) bool { return p.Episodes == 1 && p.FiringNow && p.Now >= 300 }},
		} {
			tc.body["name"], tc.body["severity"] = "p", "warning"
			c.json("POST", "/monitors/preview?range=1h", tc.body, 200, &p)
			if !tc.check(p) {
				t.Errorf("preview %v: episodes=%d firing=%v now=%v", tc.body["kind"], p.Episodes, p.FiringNow, p.Now)
			}
		}

		// maintenance windows
		var mw model.MaintenanceWindow
		now := time.Now()
		c.json("POST", "/maintenance-windows", map[string]any{"name": "db upgrade", "startsAt": now.Add(-time.Minute).UnixMilli(),
			"endsAt": now.Add(time.Hour).UnixMilli(), "scope": map[string]any{"services": []string{"it-api"}}}, 201, &mw)
		if !mw.Active || len(mw.Scope.Services) != 1 {
			t.Errorf("window: %+v", mw)
		}
		c.json("POST", "/maintenance-windows", map[string]any{"name": "bad", "startsAt": 2, "endsAt": 1}, 400, nil)
		var mws []model.MaintenanceWindow
		c.json("GET", "/maintenance-windows?only=active", nil, 200, &mws)
		if len(mws) != 1 {
			t.Errorf("active windows: %+v", mws)
		}
		c.json("PUT", "/maintenance-windows/"+mw.ID, map[string]any{"name": "db upgrade", "startsAt": now.Add(time.Hour).UnixMilli(), "endsAt": now.Add(2 * time.Hour).UnixMilli()}, 200, &mw)
		if mw.Active {
			t.Errorf("moved window still active")
		}
		c.json("DELETE", "/maintenance-windows/"+mw.ID, nil, 204, nil)
		c.json("GET", "/maintenance-windows/"+mw.ID, nil, 404, nil)

		// notification channels: secrets sealed, never echoed
		const hook = "https://hooks.slack.com/services/T000/B000/itsecret"
		code, raw := c.do("POST", "/notification-channels", map[string]any{"name": "#it-ops", "type": "slack", "config": map[string]any{"webhookUrl": hook}})
		if code != 201 || strings.Contains(string(raw), "itsecret") {
			t.Fatalf("channel create: %d %s", code, raw)
		}
		var nc model.NotificationChannel
		_ = json.Unmarshal(raw, &nc)
		ref, _ := nc.Config["webhookUrl"].(string)
		if !strings.HasPrefix(ref, "secret:") || len(nc.Secrets) != 1 {
			t.Errorf("channel: %+v", nc)
		}
		plain, err := secrets.Resolve(ctx, ref, itTenant, st.SecretsKeyOrDefault(), pgSecrets{pool})
		if err != nil || string(plain) != hook {
			t.Errorf("sealed webhook: %q %v", plain, err)
		}
		c.json("POST", "/notification-channels", map[string]any{"name": "#it-ops", "type": "slack", "config": map[string]any{"webhookUrl": hook}}, 409, nil)
		c.json("POST", "/notification-channels", map[string]any{"name": "teams", "type": "teams", "config": map[string]any{}}, 400, nil)
		c.json("PUT", "/notification-channels/"+nc.ID, map[string]any{"name": "#it-ops", "type": "slack", "config": map[string]any{"channel": "#ops"}}, 200, &nc)
		if nc.Config["webhookUrl"] != ref || nc.Config["channel"] != "#ops" {
			t.Errorf("channel update kept secret: %+v", nc)
		}
		var teams model.NotificationChannel
		c.json("POST", "/notification-channels", map[string]any{"name": "it-teams", "type": "teams", "config": map[string]any{"webhookUrl": "env:TEAMS_HOOK"}}, 201, &teams)
		if teams.Config["webhookUrl"] != "env:TEAMS_HOOK" {
			t.Errorf("env ref rewritten: %+v", teams)
		}
		var chans []model.NotificationChannel
		c.json("GET", "/notification-channels", nil, 200, &chans)
		if len(chans) != 2 {
			t.Errorf("channels: %d", len(chans))
		}
		c.json("DELETE", "/notification-channels/"+nc.ID, nil, 204, nil)
		var left int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE project_id = 9101 AND id = $1`, strings.TrimPrefix(ref, "secret:")).Scan(&left)
		if left != 0 {
			t.Errorf("channel secret not deleted")
		}
	})

	t.Run("integrations", func(t *testing.T) {
		c.t = t
		var res model.IntegrationTestResult
		c.json("POST", "/integrations/test", map[string]any{"provider": "vercel", "config": map[string]any{}, "secret": "vtok"}, 200, &res)
		if !res.OK || cloudSecret != "vtok" {
			t.Errorf("test: %+v", res)
		}
		c.json("POST", "/integrations", map[string]any{"provider": "neon", "name": "x", "config": map[string]any{}}, 400, nil) // no secret
		c.json("POST", "/integrations", map[string]any{"provider": "aws", "name": "x", "config": map[string]any{"secretAccessKey": "k"}}, 400, nil)
		code, raw := c.do("POST", "/integrations", map[string]any{"provider": "vercel", "name": "it-vercel",
			"config": map[string]any{"teamId": "team_it", "projectMap": map[string]any{"it-web-next": "it-web"}}, "secret": `{"token":"vercel-t0ken","drainSecret":"drain-s3cret"}`})
		if code != 201 || strings.Contains(string(raw), "vercel-t0ken") || strings.Contains(string(raw), "drain-s3cret") {
			t.Fatalf("create: %d %s", code, raw)
		}
		var in model.Integration
		_ = json.Unmarshal(raw, &in)
		if !in.HasSecret || in.Status != "pending" || len(in.Resources) != 1 || in.Resources[0].Name != "it-web" {
			t.Errorf("integration: %+v", in)
		}
		var credRef string
		var cfg map[string]any
		if err := pool.QueryRow(ctx, `SELECT credentials_ref, config FROM integrations WHERE id = $1`, in.ID).Scan(&credRef, &cfg); err != nil {
			t.Fatal(err)
		}
		plain, err := secrets.Resolve(ctx, credRef, itTenant, st.SecretsKeyOrDefault(), pgSecrets{pool})
		if err != nil || string(plain) != "vercel-t0ken" {
			t.Errorf("sealed token: %q %v", plain, err)
		}
		if dr, _ := cfg["drainSecretRef"].(string); !strings.HasPrefix(dr, "secret:") || dr == credRef {
			t.Errorf("drain secret ref: %v", cfg)
		} else if p, err := secrets.Resolve(ctx, dr, itTenant, st.SecretsKeyOrDefault(), pgSecrets{pool}); err != nil || string(p) != "drain-s3cret" {
			t.Errorf("sealed drain secret: %q %v", p, err)
		}
		if _, err := secrets.Resolve(ctx, credRef, 1, st.SecretsKeyOrDefault(), pgSecrets{pool}); err == nil {
			t.Errorf("secret readable from another project")
		}
		c.json("POST", "/integrations", map[string]any{"provider": "vercel", "name": "it-vercel", "config": map[string]any{}, "secret": "x"}, 409, nil)

		// rotate: same reference, new plaintext
		c.json("PATCH", "/integrations/"+in.ID, map[string]any{"secret": "vercel-t0ken-2", "config": map[string]any{"teamId": "team_it2"}}, 200, &in)
		var ref2 string
		var rotated *time.Time
		_ = pool.QueryRow(ctx, `SELECT i.credentials_ref, s.rotated_at FROM integrations i JOIN secrets s ON 'secret:' || s.id = i.credentials_ref WHERE i.id = $1`, in.ID).Scan(&ref2, &rotated)
		plain, _ = secrets.Resolve(ctx, ref2, itTenant, st.SecretsKeyOrDefault(), pgSecrets{pool})
		if ref2 != credRef || rotated == nil || string(plain) != "vercel-t0ken-2" {
			t.Errorf("rotation: %s %v %q", ref2, rotated, plain)
		}
		var conf map[string]any
		_ = json.Unmarshal(in.Config, &conf)
		if conf["teamId"] != "team_it2" || conf["projectMap"] == nil {
			t.Errorf("config merge: %v", conf)
		}
		c.json("POST", "/integrations/"+in.ID+"/sync", nil, 202, &in)
		if in.SyncRequestedAt == 0 {
			t.Errorf("sync not stamped: %+v", in)
		}

		// trooper-cloud's discovery writes (simulated): a database, a deploy marker
		for _, q := range []string{
			`INSERT INTO databases (project_id, slug, name, engine, host, port, collector, integration_id, source, version, zone, instance_class, disk_gb, meta)
			 VALUES (9101, 'aws-it-orders', 'it-orders', 'postgres', 'it-orders.rds.amazonaws.com', 5432, 'cloud', ` + in.ID + `, 'AWS RDS · eu-central-1', '16.4', 'eu-central-1a', 'db.r6g.large', 500,
			   '{"state": "available", "storageGb": 120}')`,
			`INSERT INTO deploy_markers (project_id, service, version, source, integration_id, url, commit_message, author, ts, external_id)
			 VALUES (9101, 'it-web', 'dpl_it1', 'vercel', ` + in.ID + `, 'https://it.vercel.app', 'fix: orders', 'deniz', now() - interval '10 minutes', 'dpl_it1')`,
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		c.json("GET", "/integrations/"+in.ID, nil, 200, &in)
		kinds := map[string]bool{}
		for _, r := range in.Resources {
			kinds[r.Kind+":"+r.Name] = true
		}
		if !kinds["database:it-orders"] || !kinds["project:it-web"] {
			t.Errorf("resources: %+v", in.Resources)
		}
		var db model.Database
		c.json("GET", "/infra/databases/aws-it-orders", nil, 200, &db)
		if db.Version != "16.4" || db.Zone != "eu-central-1a" || db.InstanceClass != "db.r6g.large" || db.DiskGb != 500 || db.Source != "AWS RDS · eu-central-1" || db.IntegrationID != in.ID ||
			db.Meta["state"] != "available" || db.Meta["storageGb"] != 120.0 {
			t.Errorf("db metadata: %+v", db)
		}
		var deps []model.DeployMarker
		c.json("GET", "/deploys?range=1h&service=it-web", nil, 200, &deps)
		if len(deps) != 1 || deps[0].Source != "vercel" || deps[0].CommitMessage != "fix: orders" || deps[0].IntegrationID != in.ID {
			t.Errorf("vercel deploys: %+v", deps)
		}
		c.json("GET", "/deploys?range=24h&source=spans", nil, 200, &deps)
		for _, d := range deps {
			if d.Source != "spans" {
				t.Errorf("source filter: %+v", d)
			}
		}
		c.json("GET", "/deploys?source=jenkins", nil, 400, nil)
		var list []model.Integration
		c.json("GET", "/integrations", nil, 200, &list)
		if len(list) != 1 {
			t.Errorf("list: %d", len(list))
		}

		c.json("DELETE", "/integrations/"+in.ID, nil, 204, nil)
		c.json("GET", "/integrations/"+in.ID, nil, 404, nil)
		var nSecrets int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE project_id = 9101 AND name LIKE 'integration:%'`).Scan(&nSecrets)
		if nSecrets != 0 {
			t.Errorf("integration secrets left: %d", nSecrets)
		}
		db = model.Database{}
		c.json("GET", "/infra/databases/aws-it-orders", nil, 200, &db)
		if db.IntegrationID != "" || db.Version != "16.4" {
			t.Errorf("discovered row after delete: %+v", db)
		}
		deps = nil
		c.json("GET", "/deploys?range=1h&service=it-web", nil, 200, &deps)
		if len(deps) != 1 || deps[0].IntegrationID != "" {
			t.Errorf("marker after delete: %+v", deps)
		}
	})

	t.Run("kpis", func(t *testing.T) {
		c.t = t
		in := map[string]any{"name": "Orders succeed", "owner": "team-orders", "sli": "success_rate", "objective": 0.99, "window": "7d",
			"target": map[string]any{"kind": "endpoint", "serviceId": "it-api", "ref": "GET /orders/:id", "endpointId": "get-orders-id"},
			"good":   map[string]any{"statusBelow": 500}, "alerts": []map[string]any{{"kind": "burn_rate", "longWindow": "1h", "shortWindow": "5m", "threshold": 14.4, "severity": "critical"}}}
		var k struct {
			ID      string
			Current float64
			Status  string
		}
		c.json("POST", "/kpis", in, 201, &k)
		if k.Current <= 0.9 || k.Current >= 1 {
			t.Errorf("kpi current: %+v", k)
		}
		in["objective"] = 0.9
		c.json("PUT", "/kpis/"+k.ID, in, 200, &k)
		var full model.Kpi
		c.json("GET", "/kpis/"+k.ID, nil, 200, &full)
		if full.Metric == nil || full.Metric.Unit != "ratio" {
			t.Errorf("ratio headline: %+v", full.Metric)
		}
		// journey KPI: sli_1m only (written by the alert engine)
		journey := map[string]any{"name": "Checkout journey", "sli": "success_rate", "objective": 0.99, "window": "7d",
			"target": map[string]any{"kind": "journey", "serviceId": "it-web", "ref": "checkout"}}
		var jk model.Kpi
		c.json("POST", "/kpis", journey, 201, &jk)
		if !jk.NoData {
			t.Errorf("journey without sli_1m should be noData: %+v", jk)
		}
		if err := ch.Exec(ctx, `INSERT INTO sli_1m (org_id, project_id, kpi_id, minute, good, total)
		  SELECT 9101, 9101, ?, toStartOfMinute(now()) - toIntervalMinute(number), 90, 100 FROM numbers(30)`, mustUint64(jk.ID)); err != nil {
			t.Fatal(err)
		}
		jid := jk.ID
		jk = model.Kpi{}
		c.json("GET", "/kpis/"+jid, nil, 200, &jk)
		if jk.NoData || jk.Current < 0.89 || jk.Current > 0.91 || jk.Status == model.OK || jk.DegradedSince == 0 {
			t.Errorf("journey from sli_1m: %+v", jk)
		}
		c.json("DELETE", "/kpis/"+jk.ID, nil, 204, nil)
		c.json("GET", "/kpis/"+k.ID+"/series?range=6h", nil, 200, nil)
		lat := map[string]any{"name": "Orders fast", "sli": "latency", "objective": 0.95, "window": "28d",
			"target": map[string]any{"kind": "endpoint", "serviceId": "it-api", "ref": "GET /orders/:id"}, "good": map[string]any{"thresholdMs": 90}}
		var lk model.Kpi
		c.json("POST", "/kpis", lat, 201, &lk)
		if lk.Metric == nil || lk.Metric.Unit != "ms" || lk.Metric.Current <= 0 || lk.Metric.Label != "p95 duration" {
			t.Errorf("latency headline: %+v", lk.Metric)
		}
		var lks series
		c.json("GET", "/kpis/"+lk.ID+"/series?range=1h", nil, 200, &lks)
		if lks.sum("metric") <= 0 {
			t.Errorf("kpi metric series: %+v", lks)
		}
		// code insights: endpoints exist, empty lists
		for _, p := range []string{"/suggestions", "/repos", "/inventory"} {
			if code, b := c.do("GET", p, nil); code != 200 || strings.TrimSpace(string(b)) != "[]" {
				t.Errorf("%s: %d %s", p, code, b)
			}
		}
		if lk.Status != model.OK && lk.DegradedSince == 0 {
			t.Errorf("degraded without degradedSince: %+v", lk)
		}
		page := map[string]any{"name": "LCP good", "sli": "latency", "objective": 0.75, "window": "7d",
			"target": map[string]any{"kind": "page", "serviceId": "it-web", "ref": "/orders/:id"}, "good": map[string]any{"metric": "LCP", "thresholdMs": 2500}}
		c.json("POST", "/kpis", page, 201, nil)
		var ks []any
		c.json("GET", "/kpis", nil, 200, &ks)
		if len(ks) != 3 {
			t.Errorf("kpis: %d", len(ks))
		}
		c.json("DELETE", "/kpis/"+k.ID, nil, 204, nil)
	})

	t.Run("loadtests", func(t *testing.T) {
		c.t = t
		var sc struct{ ID string }
		c.json("POST", "/loadtests/scenarios", map[string]any{"name": "orders smoke", "spec": map[string]any{"target": "http://localhost:8080", "stages": []map[string]any{{"duration": "1m", "target": 50}}}}, 201, &sc)
		var run struct{ ID, State string }
		c.json("POST", "/loadtests/scenarios/"+sc.ID+"/runs", nil, 201, &run)
		c.json("POST", "/loadtests/runs/"+run.ID+"/stop", nil, 200, &run)
		if run.State != "aborted" {
			t.Errorf("run state: %s", run.State)
		}
		c.json("POST", "/loadtests/runs/"+run.ID+"/stop", nil, 409, nil)
		c.json("GET", "/loadtests/runs?scenarioId="+sc.ID, nil, 200, nil)
		c.json("GET", "/loadtests/scenarios", nil, 200, nil)
		c.json("DELETE", "/loadtests/scenarios/"+sc.ID, nil, 204, nil)
	})
}

// pgSecrets reads secrets rows directly (verifies what the api sealed).
type pgSecrets struct{ pool *pgxpool.Pool }

func (p pgSecrets) LoadSecret(ctx context.Context, projectID, id int64) ([]byte, []byte, error) {
	var n, c []byte
	err := p.pool.QueryRow(ctx, `SELECT nonce, ciphertext FROM secrets WHERE project_id = $1 AND id = $2`, projectID, id).Scan(&n, &c)
	return n, c, err
}

func mustUint64(s string) uint64 {
	var n uint64
	_, _ = fmt.Sscan(s, &n)
	return n
}
