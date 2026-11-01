//go:build integration

// Service presence integration test (contracts §4: job entries, SDK heartbeat): services without entry traffic are
// listed, background jobs are workers without queues. It owns org/project 9815 and deletes everything it wrote.
// Run: GOWORK=off go test -tags integration ./internal/server/ -run IntegrationPresence -v
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

const presKey = "tt_it_9815_api_key"

var presSeedCH = []string{
	// it-jobs: alert.cycle-like job entries every 30 s (5 % errors) with a ClickHouse child; no messages.
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry, is_error,
	   route, http_method, entry_route, entry_method, messaging_system)
	 SELECT 9815, 9815, now64(6) - toIntervalSecond(number * 30), 40000000, MD5(concat('j', toString(number))),
	   substring(MD5(concat('j', toString(number))), 1, 8), 'it-jobs', 'prod', 'consumer', 'report.build', 1, number % 20 = 0,
	   'report.build', 'JOB', 'report.build', 'JOB', 'job'
	 FROM numbers(100)`,
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, parent_span_id, service, env, kind, name, is_entry,
	   route, entry_route, entry_method, dep, dep_kind)
	 SELECT 9815, 9815, now64(6) - toIntervalSecond(number * 30) + toIntervalMillisecond(1), 10000000, MD5(concat('j', toString(number))),
	   substring(MD5(concat('jc', toString(number))), 1, 8), substring(MD5(concat('j', toString(number))), 1, 8), 'it-jobs', 'prod', 'client',
	   'SELECT telemetry', 0, 'report.build', 'report.build', 'JOB', 'clickhouse · telemetry', 'db'
	 FROM numbers(100)`,
	// it-idle: an SDK with no traffic at all — only the heartbeat and runtime metrics (last one 2 minutes ago).
	`INSERT INTO metrics (org_id, project_id, ts, service, env, name, type, value)
	 SELECT 9815, 9815, now64(3) - toIntervalSecond(120 + number * 30), 'it-idle', 'prod', n, 'gauge', 1
	 FROM numbers(20) ARRAY JOIN ['trooper.sdk.heartbeat', 'go.goroutine.count'] AS n`,
	// it-internal: only internal / client spans (a cron without the job convention).
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry)
	 SELECT 9815, 9815, now64(6) - toIntervalSecond(number * 60), 1000000, MD5(concat('i', toString(number))),
	   substring(MD5(concat('i', toString(number))), 1, 8), 'it-internal', 'prod', 'internal', 'cleanup', 0
	 FROM numbers(10)`,
	// it-registered: registered in Postgres as an api, sends only the heartbeat.
	`INSERT INTO metrics (org_id, project_id, ts, service, env, name, type, value)
	 SELECT 9815, 9815, now64(3) - toIntervalSecond(number * 30), 'it-registered', 'prod', 'trooper.sdk.heartbeat', 'gauge', 1 FROM numbers(5)`,
	// Platform metrics that must not create services: the collector's health gauge and loadtest runner points.
	`INSERT INTO metrics (org_id, project_id, ts, service, env, name, type, value)
	 SELECT 9815, 9815, now64(3) - toIntervalSecond(number * 30), s, 'prod', n, 'gauge', 1
	 FROM numbers(5) ARRAY JOIN ['trooper-agent', 'loadtest'] AS s, ['trooper.collector.up', 'trooper.loadtest.vus'] AS n`,
	// it-old: heartbeat 3 hours ago — outside a 1 h window.
	`INSERT INTO metrics (org_id, project_id, ts, service, env, name, type, value)
	 VALUES (9815, 9815, now64(3) - toIntervalHour(3), 'it-old', 'prod', 'trooper.sdk.heartbeat', 'gauge', 1)`,
	// it-kafka: a real Kafka consumer next to the jobs (must stay a consumer with its topic).
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry, route, topic, consumer_group, messaging_system)
	 SELECT 9815, 9815, now64(6) - toIntervalSecond(number * 10), 5000000, MD5(concat('k', toString(number))),
	   substring(MD5(concat('k', toString(number))), 1, 8), 'it-kafka', 'prod', 'consumer', 'process orders', 1, 'billing', 'orders', 'billing', 'kafka'
	 FROM numbers(100)`,
}

func presCleanup(t *testing.T, pool *pgxpool.Pool, ch driver.Conn) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM orgs WHERE id = 9815`); err != nil {
		t.Errorf("pg cleanup: %v", err)
	}
	for _, tb := range []string{"spans", "metrics", "red_1m", "red_1h", "consumer_1m", "callers_1m", "edges_1m", "errors_1h", "trace_index"} {
		if err := ch.Exec(ctx, fmt.Sprintf("ALTER TABLE telemetry.`%s` DELETE WHERE project_id = 9815 SETTINGS mutations_sync = 1", tb)); err != nil &&
			!strings.Contains(err.Error(), "UNKNOWN_TABLE") {
			t.Errorf("ch cleanup %s: %v", tb, err)
		}
	}
}

func TestIntegrationPresence(t *testing.T) {
	pool, ch := itEnv(t)
	defer ch.Close()
	defer pool.Close()
	ctx := context.Background()
	presCleanup(t, pool, ch)
	defer presCleanup(t, pool, ch)
	for _, q := range []string{
		`INSERT INTO orgs (id, name) VALUES (9815, 'it-9815')`,
		`INSERT INTO projects (id, org_id, name, slug) VALUES (9815, 9815, 'it', 'it-9815')`,
		`INSERT INTO api_keys (project_id, name, kind, hash, prefix) VALUES (9815, 'it', 'api', digest('` + presKey + `', 'sha256'), 'tt_it_98')`,
		`INSERT INTO services (project_id, name, type, language) VALUES (9815, 'it-registered', 'api', 'go')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("pg seed: %v", err)
		}
	}
	for _, q := range presSeedCH {
		if err := ch.Exec(ctx, q); err != nil {
			t.Fatalf("ch seed: %v\n%s", err, q)
		}
	}
	srv := &Server{Store: store.New(ch, pool), Auth: auth.NewPG(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Origins: []string{"http://localhost:5173"}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := itClient{t: t, base: ts.URL, key: presKey}
	now := time.Now().UnixMilli()

	t.Run("list", func(t *testing.T) {
		c.t = t
		var svcs []model.Service
		c.json("GET", "/services?range=1h", nil, 200, &svcs)
		by := map[string]model.Service{}
		for _, s := range svcs {
			by[s.ID] = s
		}
		tests := []struct {
			id, typ  string
			traffic  bool
			lag      bool
			maxAgeMs int64 // lastSeen within this age
		}{
			{id: "it-jobs", typ: "worker", traffic: true, maxAgeMs: 3 * 60_000},
			{id: "it-kafka", typ: "consumer", traffic: true, lag: true, maxAgeMs: 3 * 60_000},
			{id: "it-idle", typ: "worker", maxAgeMs: 4 * 60_000},
			{id: "it-internal", typ: "worker", maxAgeMs: 3 * 60_000},
			{id: "it-registered", typ: "http", maxAgeMs: 3 * 60_000},
		}
		for _, tt := range tests {
			s, ok := by[tt.id]
			if !ok {
				t.Errorf("%s missing from %v", tt.id, svcs)
				continue
			}
			if s.Type != tt.typ || (s.Rpm > 0) != tt.traffic || (s.Lag != nil) != tt.lag || (!tt.traffic && s.Status != model.OK) {
				t.Errorf("%s: type=%s rpm=%v lag=%v status=%s", tt.id, s.Type, s.Rpm, s.Lag, s.Status)
			}
			if age := now - s.LastSeen; s.LastSeen == 0 || age > tt.maxAgeMs || age < -60_000 {
				t.Errorf("%s: lastSeen %d is %d ms old", tt.id, s.LastSeen, age)
			}
		}
		for _, id := range []string{"trooper-agent", "loadtest", "it-old"} {
			if _, ok := by[id]; ok {
				t.Errorf("%s must not be listed", id)
			}
		}
		// a wider window reaches the old heartbeat
		var wide []model.Service
		c.json("GET", "/services?range=6h", nil, 200, &wide)
		if !strings.Contains(fmt.Sprint(wide), "it-old") {
			t.Errorf("it-old missing from the 6h list")
		}
	})

	t.Run("detail of a service without traffic", func(t *testing.T) {
		c.t = t
		var s model.Service
		c.json("GET", "/services/it-idle?range=1h", nil, 200, &s)
		if s.ID != "it-idle" || s.Rpm != 0 || s.LastSeen == 0 {
			t.Errorf("it-idle: %+v", s)
		}
		var red series
		c.json("GET", "/services/it-idle/series?range=1h", nil, 200, &red)
		if red.sum("rpm") != 0 || len(red.Timestamps) == 0 {
			t.Errorf("it-idle series: %+v", red)
		}
		var eps []model.Endpoint
		c.json("GET", "/services/it-idle/endpoints?range=1h", nil, 200, &eps)
		if len(eps) != 0 {
			t.Errorf("it-idle endpoints: %+v", eps)
		}
		c.json("GET", "/services/it-idle/callers?range=1h", nil, 200, nil)
		c.json("GET", "/services/it-idle/dependencies?range=1h", nil, 200, nil)
		c.json("GET", "/services/it-nothing?range=1h", nil, 404, nil)
	})

	t.Run("job processes", func(t *testing.T) {
		c.t = t
		var eps []model.Endpoint
		c.json("GET", "/services/it-jobs/endpoints?range=1h", nil, 200, &eps)
		if len(eps) != 1 || eps[0].ID != "report.build" || eps[0].Method != "JOB" || eps[0].Route != "" || eps[0].Sub == nil ||
			eps[0].Sub.Source != "cron" || eps[0].Sub.Group != "" || eps[0].ErrorRate <= 0 {
			t.Fatalf("job processes: %+v", eps)
		}
		var callers []model.CallerTraffic
		c.json("GET", "/services/it-jobs/callers?range=1h", nil, 200, &callers)
		if len(callers) != 1 || callers[0].Caller.Kind != "schedule" || callers[0].Rpm <= 0 {
			t.Errorf("job callers: %+v", callers)
		}
		var red series
		c.json("GET", "/services/it-jobs/series?range=1h", nil, 200, &red)
		if red.sum("rpm") <= 0 {
			t.Errorf("job series: %+v", red)
		}
		var kafka []model.Endpoint
		c.json("GET", "/services/it-kafka/endpoints?range=1h", nil, 200, &kafka)
		if len(kafka) != 1 || kafka[0].Sub == nil || kafka[0].Sub.Source != "kafka" || kafka[0].Route != "orders" {
			t.Errorf("kafka processes: %+v", kafka)
		}
	})
}
