//go:build integration

// Faz 6 completion integration test (contracts §22): GPUs, pod traces / logs, Azure per-function rows. It owns
// org/project 9811, seeds Postgres + ClickHouse for that tenant and deletes everything it wrote.
// Run: GOWORK=off go test -tags integration ./internal/server/ -run IntegrationPhase6b -v
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

const p6bKey = "tt_it_9811_api_key"

var p6bSeedCH = []string{
	`INSERT INTO hosts_1m (org_id, project_id, cluster, host, host_id, os, arch, node, minute, cpu, cpu_cores, memory, memory_bytes, disk,
	   disk_read, disk_write, net_in, net_out, load1, uptime_s)
	 SELECT 9811, 9811, '', host, concat('mid-', host), 'linux', 'amd64', '', toStartOfMinute(now()) - toIntervalMinute(number),
	   0.3, 16, 0.4, 3e10, 0.2, 0, 0, 0, 0, 1, 3600 FROM numbers(30) ARRAY JOIN ['it-gpu', 'it-cpu'] AS host`,
	// GPU 0 busy, GPU 1 idle; a GPU 2 that disappeared 20 minutes ago (gone).
	`INSERT INTO gpus_1m (org_id, project_id, cluster, host, gpu_index, gpu_uuid, model, minute, utilization, memory, memory_bytes, temperature_c, power_w)
	 SELECT 9811, 9811, '', 'it-gpu', i, concat('GPU-', toString(i)), 'NVIDIA A100', toStartOfMinute(now()) - toIntervalMinute(number),
	   if(i = 0, 0.9, 0.1), 0.5, 2e10, 60 + i, 250 FROM numbers(30) ARRAY JOIN [0, 1] AS i`,
	`INSERT INTO gpus_1m (org_id, project_id, cluster, host, gpu_index, gpu_uuid, model, minute, utilization, memory, memory_bytes, temperature_c, power_w)
	 SELECT 9811, 9811, '', 'it-gpu', 2, 'GPU-2', 'Tesla T4', toStartOfMinute(now()) - toIntervalMinute(20 + number), 0.5, 0.5, 1e9, 50, 70 FROM numbers(5)`,
	// Entry spans of it-pod-svc: 10 on pod a (3 errors), 5 on pod b.
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry, is_error, route, http_method, resource_attrs)
	 SELECT 9811, 9811, now64(6) - toIntervalSecond(number * 20), 5000000, MD5(concat('p', toString(number))), substring(MD5(concat('p', toString(number))), 1, 8),
	   'it-pod-svc', 'prod', 'server', 'GET /x', 1, number < 3, '/x', 'GET',
	   map('k8s.pod.name', if(number < 10, 'it-api-a', 'it-api-b'), 'k8s.namespace.name', 'shop', 'k8s.node.name', 'it-node-1', 'host.name', 'it-node-1')
	 FROM numbers(15)`,
	`INSERT INTO logs (org_id, project_id, ts, service, env, severity, body, attrs)
	 SELECT 9811, 9811, now64(6) - toIntervalSecond(number * 30), 'it-pod-svc', '', 'info', concat('handled request ', toString(number)),
	   map('k8s.pod.name', if(number < 4, 'it-api-a', 'it-api-b'), 'k8s.namespace.name', if(number = 0, 'other', 'shop')) FROM numbers(6)`,
	// Azure app (app-level totals 30 / min) + 2 per-function rows (20 + 10 / min) + a Lambda (5 / min).
	`INSERT INTO faas_1m (org_id, project_id, function_id, provider, region, minute, invocations, errors, throttles, duration_avg, duration_max,
	   duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units)
	 SELECT 9811, 9811, f, if(f LIKE 'aws-%', 'aws', 'azure'), 'westeurope', toStartOfMinute(now()) - toIntervalMinute(1 + number),
	   multiIf(f = 'azure-it-app', 30, f = 'azure-it-app-http', 20, f = 'azure-it-app-timer', 10, 5),
	   multiIf(f = 'azure-it-app', 3, f = 'azure-it-app-http', 3, 0), -inf, 40, 90, 70, 85, -inf, -inf, -inf, if(f = 'azure-it-app', 1024000, -inf)
	 FROM numbers(10) ARRAY JOIN ['azure-it-app', 'azure-it-app-http', 'azure-it-app-timer', 'aws-eu-west-1-it-lambda'] AS f`,
}

func p6bCleanup(t *testing.T, pool *pgxpool.Pool, ch driver.Conn) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM orgs WHERE id = 9811`); err != nil {
		t.Errorf("pg cleanup: %v", err)
	}
	for _, tb := range []string{"hosts_1m", "gpus_1m", "faas_1m", "logs", "spans", "red_1m", "red_1h", "callers_1m", "edges_1m", "errors_1h",
		"trace_index", "log_patterns_1m"} {
		if err := ch.Exec(ctx, fmt.Sprintf("ALTER TABLE telemetry.`%s` DELETE WHERE project_id = 9811 SETTINGS mutations_sync = 1", tb)); err != nil &&
			!strings.Contains(err.Error(), "UNKNOWN_TABLE") {
			t.Errorf("ch cleanup %s: %v", tb, err)
		}
	}
}

func TestIntegrationPhase6b(t *testing.T) {
	pool, ch := itEnv(t)
	defer ch.Close()
	defer pool.Close()
	ctx := context.Background()
	p6bCleanup(t, pool, ch)
	defer p6bCleanup(t, pool, ch)
	for _, q := range []string{
		`INSERT INTO orgs (id, name, plan) VALUES (9811, 'it-9811', 'team')`,
		`INSERT INTO projects (id, org_id, name, slug) VALUES (9811, 9811, 'it', 'it-9811')`,
		`INSERT INTO api_keys (project_id, name, kind, hash, prefix) VALUES (9811, 'it', 'api', digest('` + p6bKey + `', 'sha256'), 'tt_it_98')`,
		`INSERT INTO serverless_functions (project_id, provider, slug, name, app, region, runtime, triggers, service, meta) VALUES
		   (9811, 'azure', 'azure-it-app', 'it-app', 'it-app', 'westeurope', 'node', '{http,timer}', 'it-app',
		    '{"plan":"Consumption","functions":["http","timer"],"appInsights":"connected"}'),
		   (9811, 'azure', 'azure-it-app-http', 'http', 'it-app', 'westeurope', 'node', '{http}', 'it-app',
		    '{"parent":"azure-it-app","source":"appinsights"}'),
		   (9811, 'azure', 'azure-it-app-timer', 'timer', 'it-app', 'westeurope', 'node', '{timer}', 'it-app',
		    '{"parent":"azure-it-app","source":"appinsights"}'),
		   (9811, 'aws', 'aws-eu-west-1-it-lambda', 'it-lambda', NULL, 'eu-west-1', 'nodejs20.x', '{api}', 'it-lambda', '{}')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("pg seed: %v", err)
		}
	}
	for _, q := range p6bSeedCH {
		if err := ch.Exec(ctx, q); err != nil {
			t.Fatalf("ch seed: %v\n%s", err, q)
		}
	}
	srv := &Server{Store: store.New(ch, pool), Auth: auth.NewPG(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Origins: []string{"http://localhost:5173"}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := itClient{t: t, base: ts.URL, key: p6bKey}

	t.Run("gpus", func(t *testing.T) {
		c.t = t
		var hosts []model.Host
		c.json("GET", "/infra/hosts?range=1h", nil, 200, &hosts)
		by := map[string]model.Host{}
		for _, h := range hosts {
			by[h.ID] = h
		}
		g := by["it-gpu"].GPUs
		if len(hosts) != 2 || len(g) != 2 || g[0].Index != 0 || g[0].Utilization != 0.9 || g[0].Model != "NVIDIA A100" || g[0].UUID != "GPU-0" ||
			g[1].TemperatureC != 61 || g[1].PowerW != 250 || g[1].MemoryBytes != 2e10 || g[1].Memory != 0.5 {
			t.Errorf("host gpus: %+v", hosts)
		}
		if c := by["it-cpu"].GPUs; c == nil || len(c) != 0 {
			t.Errorf("cpu-only host gpus: %#v", c)
		}
		var hd struct {
			model.Host
			Series struct {
				GPU model.SeriesSet `json:"gpu"`
				CPU series          `json:"cpu"`
			} `json:"series"`
		}
		c.json("GET", "/infra/hosts/it-gpu?range=30m", nil, 200, &hd)
		if len(hd.GPUs) != 2 || len(hd.Series.GPU.Series) != 2 || hd.Series.GPU.Series[0].ID != "gpu0" || hd.Series.GPU.Series[1].Label != "GPU 1 · NVIDIA A100" ||
			math.Abs(sumData(hd.Series.GPU.Series[0].Data)-27) > 1.5 || len(hd.Series.GPU.Timestamps) != 30 {
			t.Errorf("host detail gpu: %+v", hd)
		}
		hd.Series.GPU = model.SeriesSet{}
		c.json("GET", "/infra/hosts/it-cpu?range=30m", nil, 200, &hd)
		if len(hd.Series.GPU.Series) != 0 || len(hd.Series.GPU.Timestamps) != 30 {
			t.Errorf("cpu host gpu series: %+v", hd.Series.GPU)
		}
	})

	t.Run("pod traces and logs", func(t *testing.T) {
		c.t = t
		var tr []model.TraceSummary
		c.json("GET", "/traces?service=it-pod-svc&pod=it-api-a&namespace=shop&range=1h&limit=50", nil, 200, &tr)
		if len(tr) != 10 {
			t.Errorf("pod a traces: %d", len(tr))
		}
		c.json("GET", "/traces?pod=it-api-a&errors=true&range=1h", nil, 200, &tr)
		if len(tr) != 3 || !tr[0].IsError {
			t.Errorf("pod a error traces: %+v", tr)
		}
		c.json("GET", "/traces?node=it-node-1&host=it-node-1&range=1h&limit=50", nil, 200, &tr)
		if len(tr) != 15 {
			t.Errorf("node traces: %d", len(tr))
		}
		c.json("GET", "/traces?pod=it-api-a&namespace=other&range=1h", nil, 200, &tr)
		if len(tr) != 0 {
			t.Errorf("namespace mismatch: %d", len(tr))
		}
		var lp model.LogPage
		c.json("GET", "/logs?pod=it-api-a&namespace=shop&range=1h", nil, 200, &lp)
		if len(lp.Lines) != 3 {
			t.Errorf("pod a logs: %+v", lp.Lines)
		}
		c.json("GET", "/logs?pod=it-api-b&q=handled&range=1h", nil, 200, &lp)
		if len(lp.Lines) != 2 {
			t.Errorf("pod b logs + q: %+v", lp.Lines)
		}
	})

	t.Run("azure per-function rows", func(t *testing.T) {
		c.t = t
		var fns []model.ServerlessFunction
		c.json("GET", "/serverless/functions?range=1h", nil, 200, &fns)
		by := map[string]model.ServerlessFunction{}
		for _, f := range fns {
			by[f.Slug] = f
		}
		app, http, lam := by["azure-it-app"], by["azure-it-app-http"], by["aws-eu-west-1-it-lambda"]
		if len(fns) != 4 || app.ParentID != "" || http.ParentID != app.ID || by["azure-it-app-timer"].ParentID != app.ID || lam.ParentID != "" ||
			app.Invocations != 300 || http.Invocations != 200 || app.AppInsights != "connected" || http.AppInsights != "" {
			t.Fatalf("functions: %+v", fns)
		}
		c.json("GET", "/serverless/functions?parent="+app.ID, nil, 200, &fns)
		if len(fns) != 2 || fns[0].Slug != "azure-it-app-http" {
			t.Errorf("parent filter: %+v", fns)
		}
		c.json("GET", "/serverless/functions?parent=azure-it-app&q=tim", nil, 200, &fns)
		if len(fns) != 1 || fns[0].Slug != "azure-it-app-timer" {
			t.Errorf("parent slug + q: %+v", fns)
		}
		c.json("GET", "/serverless/functions?parent="+lam.ID, nil, 200, &fns)
		if len(fns) != 0 {
			t.Errorf("lambda children: %+v", fns)
		}
		var sum model.ServerlessSummary
		c.json("GET", "/serverless/summary?range=1h", nil, 200, &sum)
		var az model.ServerlessProvider
		for _, p := range sum.ByProvider {
			if p.Provider == "azure" {
				az = p
			}
		}
		// app totals only (300 + Lambda 50); functions: 2 Azure functions + 1 Lambda (the app is represented by its functions)
		if sum.Invocations != 350 || sum.Errors != 30 || sum.Functions != 3 || az.Invocations != 300 || az.Functions != 2 || az.CostUSD != app.CostUSD {
			t.Errorf("summary: %+v", sum)
		}
		var fd model.ServerlessFunctionDetail
		c.json("GET", "/serverless/functions/azure-it-app", nil, 200, &fd)
		if len(fd.Children) != 2 || fd.Children[0].Slug != "azure-it-app-http" || fd.Children[0].ParentID != app.ID || len(fd.Functions) != 2 {
			t.Errorf("app detail children: %+v", fd.Children)
		}
		fd = model.ServerlessFunctionDetail{}
		c.json("GET", "/serverless/functions/"+http.ID, nil, 200, &fd)
		if fd.ParentID != app.ID || fd.Children != nil || fd.Functions != nil || fd.Invocations != 200 {
			t.Errorf("child detail: %+v", fd)
		}
	})
}

func sumData(xs []float64) float64 {
	var v float64
	for _, x := range xs {
		v += x
	}
	return v
}
