//go:build integration

// Faz 6 integration test (AI quota, hosts / Kubernetes, serverless) against live infra. It owns org/project 9801,
// seeds Postgres + ClickHouse for that tenant and deletes everything it wrote.
// Run: GOWORK=off go test -tags integration ./internal/server/ -run IntegrationPhase6 -v
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

const (
	p6Tenant = 9801
	p6Key    = "tt_it_9801_api_key"
)

// Minutes are relative to now(); "m" = minutes ago. -inf = not reported (contracts §5).
var p6SeedCH = []string{
	// K8s node host: 30 minutes, memory not reported in the newest minute (latest finite value must win).
	`INSERT INTO hosts_1m (org_id, project_id, cluster, host, host_id, os, arch, node, minute, cpu, cpu_cores, memory, memory_bytes, disk,
	   disk_read, disk_write, net_in, net_out, load1, uptime_s)
	 SELECT 9801, 9801, 'it-k8s', 'it-node-1', 'mid-1', 'linux', 'amd64', 'it-node-1', toStartOfMinute(now()) - toIntervalMinute(number),
	   0.5, 4, if(number = 0, -inf, 0.6), 8e9, 0.4, 1000, 2000, 5000, 6000, 1.5, 86400 FROM numbers(30)`,
	// VM that stopped reporting 20 minutes ago with a full disk → stale.
	`INSERT INTO hosts_1m (org_id, project_id, cluster, host, host_id, os, arch, node, minute, cpu, cpu_cores, memory, memory_bytes, disk,
	   disk_read, disk_write, net_in, net_out, load1, uptime_s)
	 SELECT 9801, 9801, '', 'it-vm', 'mid-2', 'linux', 'arm64', '', toStartOfMinute(now()) - toIntervalMinute(20 + number),
	   0.2, 2, 0.3, 4e9, 0.99, -inf, -inf, -inf, -inf, -inf, -inf FROM numbers(10)`,
	`INSERT INTO containers_1m (org_id, project_id, cluster, host, namespace, pod, pod_uid, container, container_id, image, workload_kind, workload,
	   service, minute, cpu, cpu_limit, memory_bytes, memory_limit, restarts, net_in, net_out, throttled)
	 SELECT 9801, 9801, 'it-k8s', 'it-node-1', 'shop', 'it-api-a', 'uid-a', c, concat('cid-', c), 'shop/api:1', 'Deployment', 'it-api',
	   'it-api', toStartOfMinute(now()) - toIntervalMinute(m), 0.25, 1, 2e8, 1e9, if(m > 10, 0, 2), 100, 200, 0
	 FROM (SELECT number AS m FROM numbers(30)) ARRAY JOIN ['app', 'sidecar'] AS c`,
	`INSERT INTO containers_1m (org_id, project_id, cluster, host, namespace, pod, pod_uid, container, container_id, image, workload_kind, workload,
	   service, minute, cpu, cpu_limit, memory_bytes, memory_limit, restarts, net_in, net_out, throttled)
	 SELECT 9801, 9801, 'it-k8s', 'it-node-1', 'shop', 'it-api-b', 'uid-b', 'app', 'cid-b', 'shop/api:1', 'Deployment', 'it-api',
	   'it-api', toStartOfMinute(now()) - toIntervalMinute(number), 0.1, 1, 9.8e8, 1e9, 7, 0, 0, 0 FROM numbers(30)`,
	`INSERT INTO processes_1m (org_id, project_id, host, container_id, pid, exe, command, minute, cpu, memory_bytes, threads, fds)
	 SELECT 9801, 9801, 'it-node-1', if(p = 1, 'cid-app', ''), p, if(p = 1, 'api', 'kubelet'), if(p = 1, '/app/api', '/usr/bin/kubelet'),
	   toStartOfMinute(now()) - toIntervalMinute(m), p / 10, 1e8, 12, -inf
	 FROM (SELECT number AS m FROM numbers(3)) ARRAY JOIN [1, 2] AS p`,
	`INSERT INTO k8s_nodes_1m (org_id, project_id, cluster, node, minute, ready, allocatable_cpu, allocatable_memory, pods)
	 SELECT 9801, 9801, 'it-k8s', 'it-node-1', toStartOfMinute(now()) - toIntervalMinute(number), 1, 4, 16e9, 2 FROM numbers(30)`,
	`INSERT INTO k8s_pods_1m (org_id, project_id, cluster, namespace, pod, pod_uid, node, workload_kind, workload, service, minute, phase, reason,
	   ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit)
	 SELECT 9801, 9801, 'it-k8s', 'shop', 'it-api-a', 'uid-a', 'it-node-1', 'Deployment', 'it-api', 'it-api', toStartOfMinute(now()) - toIntervalMinute(number),
	   'Running', '', 1, if(number > 10, 0, 2), 0.5, 2, 5e8, 2e9 FROM numbers(30)`,
	`INSERT INTO k8s_pods_1m (org_id, project_id, cluster, namespace, pod, pod_uid, node, workload_kind, workload, service, minute, phase, reason,
	   ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit)
	 SELECT 9801, 9801, 'it-k8s', 'shop', 'it-api-b', 'uid-b', 'it-node-1', 'Deployment', 'it-api', 'it-api', toStartOfMinute(now()) - toIntervalMinute(number),
	   'Running', 'CrashLoopBackOff', 0, 7, 0.5, 2, 5e8, 1e9 FROM numbers(30)`,
	`INSERT INTO k8s_workloads_1m (org_id, project_id, cluster, namespace, kind, name, minute, desired, ready, available, updated)
	 SELECT 9801, 9801, 'it-k8s', 'shop', 'Deployment', 'it-api', toStartOfMinute(now()) - toIntervalMinute(number), 2, 1, 1, 2 FROM numbers(30)`,
	`INSERT INTO logs (org_id, project_id, ts, service, env, severity, body, attrs)
	 SELECT 9801, 9801, now64(6) - toIntervalSecond(number * 60), 'k8s', '', 'warn', 'Back-off restarting failed container app',
	   map('k8s.cluster.name', 'it-k8s', 'k8s.namespace.name', 'shop', 'k8s.object.kind', 'Pod', 'k8s.object.name', 'it-api-b',
	       'k8s.event.reason', 'BackOff', 'k8s.event.type', 'Warning', 'k8s.event.count', toString(number + 1)) FROM numbers(3)`,
	// Lambda: 100 invocations / min, 1 error / min (1 %), p95 300 ms; Azure app: 10 / min.
	`INSERT INTO faas_1m (org_id, project_id, function_id, provider, region, minute, invocations, errors, throttles, duration_avg, duration_max,
	   duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units)
	 SELECT 9801, 9801, 'aws-eu-west-1-it-orders', 'aws', 'eu-west-1', toStartOfMinute(now()) - toIntervalMinute(1 + number), 100, 1, 0, 120, 900,
	   300, 600, 4, -inf, -inf, -inf FROM numbers(30)`,
	`INSERT INTO faas_1m (org_id, project_id, function_id, provider, region, minute, invocations, errors, throttles, duration_avg, duration_max,
	   duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units)
	 SELECT 9801, 9801, 'azure-it-billing', 'azure', 'westeurope', toStartOfMinute(now()) - toIntervalMinute(1 + number), 10, 0, -inf, 50, 80,
	   -inf, -inf, -inf, -inf, 1.5e8, 512000 FROM numbers(30)`,
	// Lambda spans: 6 cold starts, 2 errors.
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry, is_error, error_type, attrs)
	 SELECT 9801, 9801, now64(6) - toIntervalSecond(number * 30), 120000000, MD5(concat('f', toString(number))),
	   substring(MD5(concat('f', toString(number))), 1, 8), 'it-orders', 'prod', 'server', 'orders handler', 1, number < 2, if(number < 2, 'Boom', ''),
	   map('faas.name', 'it-orders', 'faas.coldstart', if(number < 6, 'true', 'false'), 'exception.message', if(number < 2, 'order failed', ''))
	 FROM numbers(40)`,
	// eBPF-only service (OBI) and a mixed one (stays sdk).
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry, route, http_method, resource_attrs)
	 SELECT 9801, 9801, now64(6) - toIntervalSecond(number * 20), 5000000, MD5(concat('e', toString(number))), substring(MD5(concat('e', toString(number))), 1, 8),
	   if(number % 2 = 0, 'it-legacy', 'it-mixed'), 'prod', 'server', 'GET /', 1, '/', 'GET',
	   map('telemetry.sdk.name', if(number % 2 = 0 OR number = 1, 'opentelemetry-ebpf-instrumentation', 'opentelemetry'))
	 FROM numbers(40)`,
}

func p6Cleanup(t *testing.T, pool *pgxpool.Pool, ch driver.Conn) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, p6Tenant); err != nil {
		t.Errorf("pg cleanup: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE email LIKE 'it-9801-%@example.com'`); err != nil {
		t.Errorf("pg cleanup users: %v", err)
	}
	for _, tb := range []string{"hosts_1m", "containers_1m", "processes_1m", "k8s_nodes_1m", "k8s_pods_1m", "k8s_workloads_1m", "faas_1m", "logs", "spans",
		"red_1m", "red_1h", "callers_1m", "edges_1m", "errors_1h", "trace_index", "log_patterns_1m"} {
		if err := ch.Exec(ctx, fmt.Sprintf("ALTER TABLE telemetry.`%s` DELETE WHERE project_id = %d SETTINGS mutations_sync = 1", tb, p6Tenant)); err != nil &&
			!strings.Contains(err.Error(), "UNKNOWN_TABLE") {
			t.Errorf("ch cleanup %s: %v", tb, err)
		}
	}
}

func TestIntegrationPhase6(t *testing.T) {
	pool, ch := itEnv(t)
	defer ch.Close()
	defer pool.Close()
	ctx := context.Background()
	p6Cleanup(t, pool, ch)
	defer p6Cleanup(t, pool, ch)
	for _, q := range []string{
		`INSERT INTO orgs (id, name, plan) VALUES (9801, 'it-9801', 'team')`,
		`INSERT INTO projects (id, org_id, name, slug) VALUES (9801, 9801, 'it', 'it-9801')`,
		`INSERT INTO api_keys (project_id, name, kind, hash, prefix) VALUES (9801, 'it', 'api', digest('` + p6Key + `', 'sha256'), 'tt_it_98')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("pg seed: %v", err)
		}
	}
	for _, q := range p6SeedCH {
		if err := ch.Exec(ctx, q); err != nil {
			t.Fatalf("ch seed: %v\n%s", err, q)
		}
	}
	st := store.New(ch, pool)
	srv := &Server{Store: st, Auth: auth.NewPG(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Origins: []string{"http://localhost:5173"}, DevLogin: true}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := itClient{t: t, base: ts.URL, key: p6Key}

	t.Run("ai_quota", func(t *testing.T) {
		c.t = t
		var admin, member model.Member
		c.json("POST", "/orgs/9801/members", map[string]any{"email": "it-9801-adm@example.com", "role": "admin"}, 201, &admin)
		c.json("POST", "/orgs/9801/members", map[string]any{"email": "it-9801-mem@example.com", "role": "member"}, 201, &member)
		if _, err := pool.Exec(ctx, `INSERT INTO ai_usage (org_id, project_id, user_id, feature, model, input_tokens, output_tokens, credits, ref, created_at) VALUES
		    (9801, 9801, $1, 'codeintel.enrich', 'm', 1000, 100, 12.3456, 'repo:1', now()),
		    (9801, 9801, NULL, 'codeintel.suggest', 'm', 1000, 100, 7.5, 'repo:1', now()),
		    (9801, 9801, $1, 'codeintel.enrich', 'm', 1000, 100, 99, 'repo:1', date_trunc('month', now()) - interval '1 day')`, member.UserID); err != nil {
			t.Fatal(err)
		}
		var u model.AIUsage
		c.json("GET", "/usage/ai", nil, 200, &u)
		if u.Plan != "team" || u.Org.Limit == nil || *u.Org.Limit != 5000 || u.Org.Used != 19.85 || u.Org.Remaining == nil || *u.Org.Remaining != 4980.15 {
			t.Errorf("org usage: %+v", u.Org)
		}
		if len(u.ByUser) != 2 || len(u.ByFeature) != 2 || u.ByFeature[0].Feature != "codeintel.enrich" || len(u.ByProject) != 1 || u.ByProject[0].Name != "it" ||
			u.AIQuota == nil || u.AIQuota.MonthlyCredits != nil {
			t.Errorf("breakdowns: %+v", u)
		}
		var daily float64
		for _, s := range u.Daily.Series {
			for _, v := range s.Data {
				daily += v
			}
		}
		if math.Abs(daily-19.85) > 0.02 || len(u.Daily.Timestamps) < 28 {
			t.Errorf("daily: %v %d", daily, len(u.Daily.Timestamps))
		}
		var prev model.AIUsage
		c.json("GET", "/usage/ai?month="+u.Period[:5]+"01", nil, 200, &prev) // January of this year: no usage (unless it is January)
		c.json("GET", "/usage/ai?month=2026-13", nil, 400, nil)

		var q model.AIQuota
		c.json("PUT", "/orgs/9801/ai-quota", map[string]any{"monthlyCredits": 15, "notify": []string{"email:ops@example.com"}}, 200, &q)
		if q.MonthlyCredits == nil || *q.MonthlyCredits != 15 || len(q.Notify) != 1 {
			t.Errorf("quota: %+v", q)
		}
		c.json("PUT", "/orgs/9801/members/"+member.UserID+"/ai-quota", map[string]any{"monthlyCredits": 10}, 200, nil)
		c.json("PUT", "/orgs/9801/members/999999999/ai-quota", map[string]any{"monthlyCredits": 10}, 404, nil)
		c.json("PUT", "/orgs/9801/ai-quota", map[string]any{"monthlyCredits": -2}, 400, nil)
		var actions []string
		rows, _ := pool.Query(ctx, `SELECT action FROM audit_log WHERE org_id = 9801 AND action LIKE 'ai\_quota.%' ORDER BY id`)
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			actions = append(actions, a)
		}
		rows.Close()
		if strings.Join(actions, ",") != "ai_quota.update,ai_quota.member" {
			t.Errorf("audit: %v", actions)
		}

		// member session: own usage, no byUser, quota exceeded remaining 0
		jar, _ := cookiejar.New(nil)
		sc := &http.Client{Jar: jar}
		sess := func(method, path string, body any) (int, []byte) {
			var rd io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
			req, _ := http.NewRequest(method, ts.URL+"/api/v1"+path, rd)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Trooper-Project", "9801")
			res, err := sc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			return res.StatusCode, b
		}
		if code, b := sess("POST", "/auth/dev-login", map[string]any{"email": "it-9801-mem@example.com"}); code != 200 {
			t.Fatalf("dev login: %d %s", code, b)
		}
		code, b := sess("GET", "/usage/ai", nil)
		var mu model.AIUsage
		_ = json.Unmarshal(b, &mu)
		if code != 200 || len(mu.ByUser) != 0 || mu.AIQuota != nil || mu.Me.Used != 12.35 || mu.Me.Limit == nil || *mu.Me.Limit != 10 ||
			*mu.Org.Limit != 15 || *mu.Org.Remaining != 0 {
			t.Errorf("member usage: %d %s", code, b)
		}
		code, b = sess("GET", "/me", nil)
		var me model.Me
		_ = json.Unmarshal(b, &me)
		if code != 200 || me.AI == nil || me.AI.Used != 19.85 || me.AI.UserUsed != 12.35 || me.AI.UserLimit == nil || *me.AI.UserLimit != 10 {
			t.Errorf("me.ai: %d %s", code, b)
		}
		if code, _ := sess("PUT", "/orgs/9801/ai-quota", map[string]any{"monthlyCredits": 1}); code != 403 {
			t.Errorf("member sets quota: %d", code)
		}
		var members []model.Member
		c.json("GET", "/orgs/9801/members", nil, 200, &members)
		for _, m := range members {
			if m.UserID == member.UserID && (m.AICredits.Limit == nil || *m.AICredits.Limit != 10 || m.AICredits.Used != 12.35) {
				t.Errorf("member aiCredits: %+v", m)
			}
			if m.UserID == admin.UserID && (m.AICredits.Limit != nil || m.AICredits.Used != 0) {
				t.Errorf("admin aiCredits: %+v", m)
			}
		}

		// analyze_requested_by: the session user; api key → NULL. analysis summary passes through (credits 2 decimals).
		var repoID int64
		if err := pool.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, analysis) VALUES (9801, 'github', 'acme/it-9801',
		    '{"sha":"abc","items":3,"llmItems":1,"incremental":false,"llm":{"status":"quota_exceeded","reason":"user_limit","credits":1.23456}}') RETURNING id`).Scan(&repoID); err != nil {
			t.Fatal(err)
		}
		if code, b := sess("POST", fmt.Sprintf("/repos/%d/analyze", repoID), nil); code != 202 {
			t.Fatalf("analyze: %d %s", code, b)
		}
		var by *int64
		_ = pool.QueryRow(ctx, `SELECT analyze_requested_by FROM repositories WHERE id = $1`, repoID).Scan(&by)
		if by == nil || fmt.Sprint(*by) != member.UserID {
			t.Errorf("analyze_requested_by: %v", by)
		}
		var repo map[string]any
		c.json("POST", fmt.Sprintf("/repos/%d/analyze", repoID), nil, 202, &repo)
		_ = pool.QueryRow(ctx, `SELECT analyze_requested_by FROM repositories WHERE id = $1`, repoID).Scan(&by)
		if by != nil {
			t.Errorf("api key analyze_requested_by: %v", *by)
		}
		llm, _ := repo["analysis"].(map[string]any)["llm"].(map[string]any)
		if llm["status"] != "quota_exceeded" || llm["credits"] != 1.23 {
			t.Errorf("analysis: %v", repo["analysis"])
		}
	})

	t.Run("infra", func(t *testing.T) {
		c.t = t
		var hosts []model.Host
		c.json("GET", "/infra/hosts?range=1h", nil, 200, &hosts)
		if len(hosts) != 2 {
			t.Fatalf("hosts: %+v", hosts)
		}
		byID := map[string]model.Host{}
		for _, h := range hosts {
			byID[h.ID] = h
		}
		n1, vm := byID["it-node-1"], byID["it-vm"]
		if n1.Status != model.InfraOK || n1.CPU != 0.5 || n1.Memory != 0.6 || n1.Containers != 3 || n1.Cluster != "it-k8s" || len(n1.Spark.CPU) != 60 || n1.Spark.CPU[59] == 0 && n1.Spark.CPU[58] == 0 {
			t.Errorf("node host: %+v", n1)
		}
		if vm.Status != model.InfraStale || vm.Disk != 0.99 || vm.NetIn != 0 || vm.Arch != "arm64" {
			t.Errorf("vm: %+v", vm)
		}
		c.json("GET", "/infra/hosts?cluster=it-k8s&range=15m", nil, 200, &hosts)
		if len(hosts) != 1 {
			t.Errorf("cluster filter: %d", len(hosts))
		}
		c.json("GET", "/infra/hosts?q=VM", nil, 200, &hosts)
		if len(hosts) != 1 || hosts[0].ID != "it-vm" {
			t.Errorf("q filter: %+v", hosts)
		}
		var hd struct {
			model.Host
			Series struct {
				CPU     series `json:"cpu"`
				Network series `json:"network"`
			} `json:"series"`
			ContainerList []model.Container   `json:"containerList"`
			Processes     []model.HostProcess `json:"processes"`
			Services      []string            `json:"services"`
		}
		c.json("GET", "/infra/hosts/it-node-1?range=30m", nil, 200, &hd)
		if hd.CPU != 0.5 || len(hd.Series.CPU.Timestamps) != 30 || hd.Series.CPU.sum("cpu") < 10 || hd.Series.Network.sum("out") < 1000 || len(hd.ContainerList) != 3 ||
			len(hd.Processes) != 2 || hd.Processes[0].Exe != "kubelet" || strings.Join(hd.Services, ",") != "it-api" {
			t.Errorf("host detail: %+v", hd)
		}
		c.json("GET", "/infra/hosts/nope", nil, 404, nil)

		var ctrs []model.Container
		c.json("GET", "/infra/containers?service=it-api", nil, 200, &ctrs)
		st := map[string]model.InfraStatus{}
		for _, x := range ctrs {
			st[x.ID] = x.Status
		}
		if len(ctrs) != 3 || st["cid-b"] != model.InfraCritical || st["cid-app"] != model.InfraWarning {
			t.Errorf("containers: %+v", ctrs)
		}

		var clusters []model.K8sCluster
		c.json("GET", "/infra/k8s/clusters", nil, 200, &clusters)
		if len(clusters) != 1 {
			t.Fatalf("clusters: %+v", clusters)
		}
		k := clusters[0]
		if k.Name != "it-k8s" || k.Nodes != 1 || k.NodesReady != 1 || k.Pods != 2 || k.PodsRunning != 2 || k.PodsFailing != 1 || k.Namespaces != 1 ||
			k.Workloads != 1 || math.Abs(k.CPUUsed-0.6) > 1e-9 || k.CPUAllocatable != 4 || k.WarningEvents != 3 || k.Status != model.InfraCritical {
			t.Errorf("cluster: %+v", k)
		}
		var cd struct {
			model.K8sCluster
			NodeList      []model.K8sNode      `json:"nodeList"`
			NamespaceList []model.K8sNamespace `json:"namespaceList"`
			Series        struct {
				CPU series `json:"cpu"`
			} `json:"series"`
		}
		c.json("GET", "/infra/k8s/clusters/it-k8s", nil, 200, &cd)
		if cd.Pods != 2 || len(cd.NodeList) != 1 || cd.NodeList[0].Host != "it-node-1" || cd.NodeList[0].CPU != 2 || cd.NodeList[0].Status != model.InfraOK ||
			len(cd.NamespaceList) != 1 || cd.NamespaceList[0].Failing != 1 || cd.Series.CPU.sum("allocatable") < 100 || cd.Series.CPU.sum("requested") < 25 {
			t.Errorf("cluster detail: %+v", cd)
		}
		c.json("GET", "/infra/k8s/clusters/nope", nil, 404, nil)

		var wl []model.K8sWorkload
		c.json("GET", "/infra/k8s/workloads?cluster=it-k8s&namespace=shop", nil, 200, &wl)
		if len(wl) != 1 || wl[0].Status != model.InfraWarning || wl[0].Restarts != 2 || wl[0].Service != "it-api" || math.Abs(wl[0].CPU-0.6) > 1e-9 {
			t.Errorf("workloads: %+v", wl)
		}
		var pods []model.Pod
		c.json("GET", "/infra/k8s/pods?service=it-api", nil, 200, &pods)
		if len(pods) != 2 || pods[0].UID != "uid-b" || pods[0].Status != model.InfraCritical || pods[1].Status != model.InfraWarning ||
			pods[1].RestartsInRange != 2 || len(pods[1].Containers) != 2 || pods[1].CPU != 0.5 {
			t.Errorf("pods: %+v", pods)
		}
		var pd struct {
			model.Pod
			Series struct {
				CPU      series `json:"cpu"`
				Restarts series `json:"restarts"`
			} `json:"series"`
			Events []model.K8sEvent `json:"events"`
		}
		c.json("GET", "/infra/k8s/pods/uid-a", nil, 200, &pd)
		if pd.Restarts != 2 || pd.Series.Restarts.sum("restarts") != 2 || pd.Series.CPU.sum("limit") < 50 || pd.Series.CPU.sum("usage") < 10 || len(pd.Events) != 0 {
			t.Errorf("pod detail: %+v", pd)
		}
		c.json("GET", "/infra/k8s/pods/uid-b", nil, 200, &pd)
		if len(pd.Events) != 3 || pd.Events[0].Reason != "BackOff" || pd.Events[0].Cluster != "it-k8s" {
			t.Errorf("pod events: %+v", pd.Events)
		}
		var evs []model.K8sEvent
		c.json("GET", "/infra/k8s/events?cluster=it-k8s&limit=2", nil, 200, &evs)
		if len(evs) != 2 || evs[0].Count != 1 || evs[0].ObjectKind != "Pod" {
			t.Errorf("events: %+v", evs)
		}
		var si model.ServiceInfra
		c.json("GET", "/services/it-api/infra", nil, 200, &si)
		if len(si.Pods) != 2 || len(si.Containers) != 3 || len(si.Hosts) != 1 || si.Hosts[0].ID != "it-node-1" {
			t.Errorf("service infra: %+v", si)
		}

		var svcs []model.Service
		c.json("GET", "/services?range=1h", nil, 200, &svcs)
		src := map[string]string{}
		for _, s := range svcs {
			src[s.ID] = s.Source
		}
		if src["it-legacy"] != "ebpf" || src["it-mixed"] != "sdk" || src["it-orders"] != "sdk" {
			t.Errorf("service source: %v", src)
		}

		mon := func(kind string, q any, want int) {
			c.json("POST", "/monitors", map[string]any{"name": "it " + kind, "kind": kind, "target": map[string]any{}, "query": q,
				"condition": map[string]any{"op": ">", "threshold": 3}, "severity": "warning", "channels": []string{}}, want, nil)
		}
		mon("infra", map[string]any{"entity": "pod", "metric": "restarts", "filter": map[string]any{"namespace": "shop"}}, 201)
		mon("infra", map[string]any{"entity": "pod", "metric": "disk"}, 400)
		mon("faas", map[string]any{"functionId": "aws-eu-west-1-it-orders", "metric": "error_rate"}, 201)
		mon("faas", map[string]any{"metric": "gb_seconds"}, 400)
		var tpl []store.MonitorTemplate
		c.json("GET", "/monitor-templates", nil, 200, &tpl)
		if len(tpl) != len(store.MonitorTemplates) {
			t.Errorf("templates: %d", len(tpl))
		}
	})

	t.Run("serverless", func(t *testing.T) {
		c.t = t
		var integ model.Integration
		c.json("POST", "/integrations", map[string]any{"provider": "azure", "name": "it-azure",
			"config": map[string]any{"tenantId": "t-1", "clientId": "c-1", "subscriptionIds": []string{"s-1"}}, "credentials": map[string]any{"clientSecret": "az-secret"}}, 201, &integ)
		c.json("POST", "/integrations", map[string]any{"provider": "azure", "name": "it-azure-2", "config": map[string]any{"tenantId": "t-1"}, "secret": "x"}, 400, nil)
		c.json("POST", "/integrations", map[string]any{"provider": "azure", "name": "it-azure-3", "config": map[string]any{"tenantId": "t-1", "clientId": "c"}}, 400, nil)
		c.json("POST", "/integrations", map[string]any{"provider": "azure", "name": "it-azure-4", "config": map[string]any{"tenantId": "t-1", "clientId": "c", "clientSecret": "leak"}, "secret": "x"}, 400, nil)
		var ref string
		_ = pool.QueryRow(ctx, `SELECT credentials_ref FROM integrations WHERE id = $1`, integ.ID).Scan(&ref)
		if !strings.HasPrefix(ref, "secret:") || !integ.HasSecret || strings.Contains(string(integ.Config), "az-secret") {
			t.Errorf("azure secret not sealed: %q %+v", ref, integ)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO serverless_functions (project_id, integration_id, provider, slug, name, app, region, runtime, memory_mb, timeout_s,
		    arch, triggers, service, meta, missing) VALUES
		    (9801, NULL, 'aws', 'aws-eu-west-1-it-orders', 'it-orders', NULL, 'eu-west-1', 'nodejs20.x', 1024, 1, 'arm64', '{api,sqs:orders}', 'it-orders', '{"arn":"arn:x"}', false),
		    (9801, $1, 'azure', 'azure-it-billing', 'it-billing', 'it-billing', 'westeurope', 'dotnet', NULL, NULL, '', '{http}', 'it-billing',
		     '{"plan":"Consumption","functions":[{"name":"Invoice","trigger":"http"},{"name":"Nightly","trigger":"timer"}]}', false),
		    (9801, $1, 'azure', 'azure-it-gone', 'it-gone', 'it-gone', 'westeurope', '', NULL, NULL, '', '{}', 'it-gone', '{}', true)`, integ.ID); err != nil {
			t.Fatal(err)
		}
		var list []model.Integration
		c.json("GET", "/integrations?provider=azure", nil, 200, &list)
		if len(list) != 1 || len(list[0].Resources) != 2 || list[0].Resources[0].Kind != "function" {
			t.Errorf("azure integrations: %+v", list)
		}

		var fns []model.ServerlessFunction
		c.json("GET", "/serverless/functions?range=1h", nil, 200, &fns)
		if len(fns) != 3 {
			t.Fatalf("functions: %+v", fns)
		}
		f := fns[0]
		if f.Slug != "aws-eu-west-1-it-orders" || f.Invocations != 3000 || f.Errors != 30 || math.Abs(f.ErrorRate-0.01) > 1e-9 || f.P95Ms != 300 ||
			f.ColdStarts != 6 || f.ConcurrencyMax != 4 || f.Status != "ok" || len(f.Spark) != 60 || f.CostUSD <= 0 {
			t.Errorf("lambda: %+v", f)
		}
		// GB-s = 3000 × 0.12 s × 1 GB = 360 → arm64 price; + 3000 requests
		if want := 360*0.0000133334 + 3000*0.20/1e6; math.Abs(f.CostUSD-want) > 1e-6 {
			t.Errorf("lambda cost %v want %v", f.CostUSD, want)
		}
		st := map[string]string{}
		for _, x := range fns {
			st[x.Slug] = x.Status
		}
		if st["azure-it-billing"] != "ok" || st["azure-it-gone"] != "missing" {
			t.Errorf("statuses: %v", st)
		}
		c.json("GET", "/serverless/functions?provider=azure&q=bill", nil, 200, &fns)
		if len(fns) != 1 || fns[0].MemoryMb != nil || fns[0].P95Ms != 0 {
			t.Errorf("azure filter: %+v", fns)
		}
		c.json("GET", "/serverless/functions?provider=gcp", nil, 400, nil)

		var sum model.ServerlessSummary
		c.json("GET", "/serverless/summary", nil, 200, &sum)
		if sum.Functions != 3 || sum.Invocations != 3300 || sum.Errors != 30 || sum.ColdStarts != 6 || len(sum.ByProvider) != 2 || sum.ByProvider[1].Provider != "azure" {
			t.Errorf("summary: %+v", sum)
		}
		var fd struct {
			model.ServerlessFunction
			Series struct {
				Invocations series `json:"invocations"`
				Duration    series `json:"duration"`
				ColdStarts  series `json:"coldStarts"`
			} `json:"series"`
			RecentErrors []model.FunctionError `json:"recentErrors"`
			Functions    []string              `json:"functions"`
			Integration  *model.IntegrationRef `json:"integration"`
		}
		c.json("GET", "/serverless/functions/aws-eu-west-1-it-orders", nil, 200, &fd)
		if fd.Invocations != 3000 || fd.Series.Invocations.sum("invocations") != 3000 || fd.Series.Invocations.sum("errors") != 30 ||
			fd.Series.ColdStarts.sum("coldStarts") != 6 || len(fd.RecentErrors) != 2 || fd.RecentErrors[0].Message != "order failed" ||
			len(fd.RecentErrors[0].TraceID) != 32 || fd.Integration != nil || fd.Functions != nil {
			t.Errorf("lambda detail: %+v", fd)
		}
		c.json("GET", "/serverless/functions/azure-it-billing", nil, 200, &fd)
		if strings.Join(fd.Functions, ",") != "Invoice,Nightly" || fd.Integration == nil || fd.Integration.Name != "it-azure" || fd.ID == "" {
			t.Errorf("azure detail: %+v", fd)
		}
		// numeric id works too
		c.json("GET", "/serverless/functions/"+fd.ID, nil, 200, nil)
		c.json("GET", "/serverless/functions/nope", nil, 404, nil)
		var si model.ServiceInfra
		c.json("GET", "/services/it-orders/infra", nil, 200, &si)
		if len(si.Functions) != 1 || si.Functions[0].Provider != "aws" {
			t.Errorf("service functions: %+v", si.Functions)
		}
	})
}
