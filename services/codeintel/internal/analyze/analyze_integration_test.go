//go:build integration

// End-to-end analysis against the docker-compose Postgres + ClickHouse (make infra && make migrate):
//
//	GOWORK=off go test -tags integration ./internal/analyze/
//
// Uses project 9401 (created and removed by the test; its ClickHouse rows are deleted too).
package analyze

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/enrich"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/observe"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/source"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

const testProject = 9401

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type fakeLLM struct {
	mu    sync.Mutex
	items []string
}

func (f *fakeLLM) Model() string { return "fake-model" }
func (f *fakeLLM) Enrich(_ context.Context, _ enrich.RepoSummary, items []*model.Item, _ enrich.Budget) (enrich.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range items {
		f.items = append(f.items, it.Key)
		enrich.Apply(it, &model.LLMResult{Criticality: it.Criticality, Summary: "llm: " + it.Key, Model: "fake-model",
			LoadtestHint: &model.LoadtestHint{PayloadExample: json.RawMessage(`{"sku":"A-1","qty":1}`)}})
	}
	return enrich.Outcome{Status: model.LLMOK}, nil
}
func (f *fakeLLM) reset() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.items
	f.items = nil
	return out
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

const monitorYAML = `name: Orders create error rate
kind: metric_threshold
query: { source: red, service: orders-api, route: POST /v1/orders, metric: error_rate }
condition: { op: '>', threshold: 0.02, for: 5m, min_requests: 50 }
severity: critical
notify: [slack:#orders]
`

func TestAnalyzeEndToEnd(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, env("TT_POSTGRES_URL", "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"))
	if err != nil {
		t.Skip("postgres not reachable:", err)
	}
	t.Cleanup(st.Close) // registered first: runs after the row cleanup
	chURL := env("TT_CLICKHOUSE_URL", "clickhouse://trooper:trooper@localhost:9000/telemetry")
	opts, err := clickhouse.ParseDSN(chURL)
	if err != nil {
		t.Fatal(err)
	}
	chw, err := clickhouse.Open(opts) // test writer; codeintel itself only reads
	if err != nil || chw.Ping(ctx) != nil {
		t.Skip("clickhouse not reachable")
	}
	t.Cleanup(func() { _ = chw.Close() })
	chr, err := observe.Open(ctx, chURL)
	if err != nil {
		t.Fatal(err)
	}

	// --- setup (009 adds pending_paths; keep the test independent of migration order) ---
	exec1 := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.DB.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec1(`ALTER TABLE repositories ADD COLUMN IF NOT EXISTS pending_paths text[]`)
	cleanup := func() {
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, testProject)
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM codeintel_webhook_deliveries WHERE id LIKE 'it-9401-%'`)
		_ = chw.Exec(context.Background(), `ALTER TABLE telemetry.red_1m DELETE WHERE project_id = 9401 SETTINGS mutations_sync = 1`)
	}
	cleanup()
	t.Cleanup(cleanup)
	exec1(`INSERT INTO projects (id, org_id, name, slug) VALUES ($1, 1, 'codeintel-it', 'codeintel-it-9401')`, testProject)
	exec1(`INSERT INTO broker_clusters (project_id, slug, name, kind, endpoint) VALUES ($1, 'kafka-main', 'Kafka', 'kafka', 'localhost:9092')`, testProject)

	ins := func(service, route, method string, count, errs, p95 float64) {
		t.Helper()
		err := chw.Exec(ctx, `INSERT INTO telemetry.red_1m (org_id, project_id, service, env, version, route, method, minute, count, errors,
			s2xx, s3xx, s4xx, s5xx, duration_sum, duration)
			SELECT 1, 9401, ?, 'production', 'v1', ?, ?, toStartOfMinute(now() - INTERVAL 1 HOUR), ?, ?, 0, 0, 0, 0, 0,
			  quantilesTDigestWeightedState(0.5, 0.75, 0.95, 0.99)(toFloat64(?), toUInt32(1))`, service, route, method, count, errs, p95)
		if err != nil {
			t.Fatal(err)
		}
	}
	ins("orders-api", "/v1/orders", "POST", 20000, 100, 240)
	ins("orders-api", "/v1/orders/123", "GET", 900, 0, 30) // raw path → template :id
	ins("orders-api", "/legacy/export", "GET", 40, 0, 10)  // traffic without code → shadow

	repoDir := t.TempDir()
	copyTree(t, filepath.Join("..", "extract", "testdata", "gorepo"), repoDir)
	_ = os.MkdirAll(filepath.Join(repoDir, ".trooper", "monitors"), 0o755)
	_ = os.WriteFile(filepath.Join(repoDir, ".trooper", "monitors", "orders.yaml"), []byte(monitorYAML), 0o644)
	git(t, repoDir, "init", "-q", "-b", "main")
	git(t, repoDir, "add", ".")
	git(t, repoDir, "commit", "-q", "-m", "init")

	var repoID int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, default_branch, local_path)
		VALUES ($1, 'local', 'acme/shop', 'main', $2) RETURNING id`, testProject, repoDir).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{}
	an := &Analyzer{Store: st, Fetcher: &source.Fetcher{}, Traffic: chr, LLM: llm, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	analyze := func(wantIncremental bool) *Report {
		t.Helper()
		started, pending, ok, err := st.Claim(ctx, repoID, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim ok=%v err=%v", ok, err)
		}
		if _, _, again, _ := st.Claim(ctx, repoID, time.Minute); again {
			t.Fatal("second claim of a running analysis must fail")
		}
		repo, err := st.Repo(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := an.Run(ctx, repo, pending)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Incremental != wantIncremental {
			t.Fatalf("incremental=%v pending=%v", rep.Incremental, pending)
		}
		if err := st.FinishOK(ctx, repoID, started, rep.SHA, rep.Languages, rep.Services, rep.Summary()); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	itemID := func(service, key string) (id int64, crit string, observed, monitored bool) {
		t.Helper()
		err := st.DB.QueryRow(ctx, `SELECT id, criticality, observed, monitored FROM inventory_items
			WHERE repository_id = $1 AND service = $2 AND key = $3`, repoID, service, key).Scan(&id, &crit, &observed, &monitored)
		if err != nil {
			t.Fatalf("%s %s: %v", service, key, err)
		}
		return
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := st.DB.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	// --- 1. full analysis ---
	rep := analyze(false)
	if rep.Items != 34 || len(rep.SHA) != 40 || rep.Monitors.Created != 1 || rep.Shadows != 1 {
		t.Fatalf("report %+v", rep)
	}
	var status string
	var langs, svcs []string
	if err := st.DB.QueryRow(ctx, `SELECT status, languages, services FROM repositories WHERE id = $1`, repoID).Scan(&status, &langs, &svcs); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || strings.Join(langs, ",") != "go" || !strings.Contains(strings.Join(svcs, ","), "orders-api") {
		t.Fatalf("repo %s %v %v", status, langs, svcs)
	}
	postID, postCrit, postObs, postMon := itemID("orders-api", "POST /v1/orders")
	if postCrit != "critical" || !postObs || !postMon {
		t.Fatalf("POST /v1/orders crit=%s observed=%v monitored=%v", postCrit, postObs, postMon)
	}
	if _, _, obs, _ := itemID("orders-api", "GET /v1/orders/:id"); !obs {
		t.Fatal("raw path traffic not matched to the route template")
	}
	if n := count(`SELECT count(*) FROM inventory_items WHERE project_id = $1 AND shadow AND repository_id IS NULL AND key = 'GET /legacy/export'`, testProject); n != 1 {
		t.Fatalf("shadow rows %d", n)
	}
	if n := count(`SELECT count(*) FROM monitors WHERE project_id = $1 AND source = 'code' AND code_ref = 'acme/shop:.trooper/monitors/orders.yaml'
		AND target->>'endpoint' = 'POST /v1/orders' AND (condition->>'forMin')::float = 5`, testProject); n != 1 {
		t.Fatalf("code monitors %d", n)
	}
	if n := count(`SELECT count(*) FROM alert_suggestions WHERE inventory_item_id = $1`, postID); n != 0 {
		t.Fatal("monitored item got a suggestion")
	}
	nSug := count(`SELECT count(*) FROM alert_suggestions WHERE project_id = $1 AND state = 'proposed'`, testProject)
	if nSug == 0 || nSug != rep.Suggestions {
		t.Fatalf("suggestions %d report %d", nSug, rep.Suggestions)
	}
	var draft []byte
	if err := st.DB.QueryRow(ctx, `SELECT monitor_draft FROM alert_suggestions WHERE project_id = $1 ORDER BY id LIMIT 1`, testProject).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	_ = json.Unmarshal(draft, &d)
	if d["name"] == "" || d["kind"] == "" || d["severity"] == "" || d["target"] == nil {
		t.Fatalf("draft %s", draft)
	}
	if n := count(`SELECT count(*) FROM loadtest_scenarios WHERE project_id = $1 AND source = 'generated' AND state = 'draft'
		AND spec->'generated'->>'repo' = 'acme/shop'`, testProject); n == 0 || n != rep.Scenarios {
		t.Fatalf("scenarios %d report %d", n, rep.Scenarios)
	}
	if first := llm.reset(); len(first) == 0 || len(first) != rep.LLMItems {
		t.Fatalf("llm items %d report %d", len(first), rep.LLMItems)
	}
	// An accepted suggestion must survive re-analysis untouched.
	var acceptedID int64
	if err := st.DB.QueryRow(ctx, `UPDATE alert_suggestions SET state = 'accepted', title = 'kept' WHERE id =
		(SELECT id FROM alert_suggestions WHERE project_id = $1 ORDER BY id LIMIT 1) RETURNING id`, testProject).Scan(&acceptedID); err != nil {
		t.Fatal(err)
	}

	// --- 2. push → incremental: drop a route and a whole file ---
	chi := filepath.Join(repoDir, "services", "orders", "chi.go")
	src, _ := os.ReadFile(chi)
	var kept []string
	for _, l := range strings.Split(string(src), "\n") {
		if !strings.Contains(l, "/profile") {
			kept = append(kept, l)
		}
	}
	_ = os.WriteFile(chi, []byte(strings.Join(kept, "\n")), 0o644)
	_ = os.Remove(filepath.Join(repoDir, "services", "catalog", "echo.go"))
	git(t, repoDir, "add", "-A")
	git(t, repoDir, "commit", "-q", "-m", "change")
	time.Sleep(10 * time.Millisecond)
	if err := st.RequestAnalysis(ctx, repoID, []string{"services/orders/chi.go", "services/catalog/echo.go"}); err != nil {
		t.Fatal(err)
	}
	repo, _ := st.Repo(ctx, repoID)
	if !repo.Due() {
		t.Fatal("push did not make the repo due")
	}
	rep2 := analyze(true)
	if rep2.Changed != 2 || rep2.Items != 34-4-1 {
		t.Fatalf("incremental report %+v", rep2)
	}
	if id, _, _, _ := itemID("orders-api", "POST /v1/orders"); id != postID {
		t.Fatal("ids of untouched items must be stable")
	}
	if n := count(`SELECT count(*) FROM inventory_items WHERE repository_id = $1 AND (service = 'catalog' OR key = 'PUT /v2/profile')`, repoID); n != 0 {
		t.Fatalf("removed items still stored: %d", n)
	}
	if again := llm.reset(); len(again) != 0 {
		t.Fatalf("unchanged code re-sent to the LLM: %v", again)
	}
	var accTitle, accState string
	if err := st.DB.QueryRow(ctx, `SELECT title, state FROM alert_suggestions WHERE id = $1`, acceptedID).Scan(&accTitle, &accState); err != nil ||
		accTitle != "kept" || accState != "accepted" {
		t.Fatalf("accepted suggestion changed: %s %s %v", accTitle, accState, err)
	}
	repo, _ = st.Repo(ctx, repoID)
	if repo.Due() {
		t.Fatal("still due after analysis")
	}

	// --- 3. monitor file removed → code monitor deleted (full scan via "*") ---
	_ = os.Remove(filepath.Join(repoDir, ".trooper", "monitors", "orders.yaml"))
	git(t, repoDir, "add", "-A")
	git(t, repoDir, "commit", "-q", "-m", "drop monitor")
	if err := st.RequestAnalysis(ctx, repoID, []string{store.FullScan}); err != nil {
		t.Fatal(err)
	}
	rep3 := analyze(false)
	if rep3.Monitors.Deleted != 1 {
		t.Fatalf("monitors %+v", rep3.Monitors)
	}
	if _, _, _, mon := itemID("orders-api", "POST /v1/orders"); mon {
		t.Fatal("item still monitored after its monitor was deleted")
	}
	if n := count(`SELECT count(*) FROM alert_suggestions s JOIN inventory_items i ON i.id = s.inventory_item_id
		WHERE i.repository_id = $1 AND i.key = 'POST /v1/orders' AND s.state = 'proposed'`, repoID); n == 0 {
		t.Fatal("unmonitored critical endpoint got no suggestion")
	}

	// --- 4. same repository as a (file://) GitHub clone: a manual request ("*") is incremental via git diff ---
	remotes := t.TempDir()
	git(t, remotes, "clone", "-q", "--bare", repoDir, filepath.Join(remotes, "acme", "shop.git"))
	exec1(`UPDATE repositories SET provider = 'github', local_path = NULL WHERE id = $1`, repoID)
	an.Fetcher = &source.Fetcher{WorkDir: t.TempDir(), CloneBase: "file://" + remotes + "/"}
	push := func(msg string) {
		t.Helper()
		git(t, repoDir, "add", "-A")
		git(t, repoDir, "commit", "-q", "--allow-empty", "-m", msg)
		git(t, repoDir, "push", "-q", filepath.Join(remotes, "acme", "shop.git"), "main")
		time.Sleep(10 * time.Millisecond)
		if err := st.RequestAnalysis(ctx, repoID, []string{store.FullScan}); err != nil {
			t.Fatal(err)
		}
	}
	kafkaItems := count(`SELECT count(*) FROM inventory_items WHERE repository_id = $1 AND file = 'services/orders/kafka.go'`, repoID)
	if kafkaItems == 0 {
		t.Fatal("fixture has no items in kafka.go")
	}
	_ = os.Remove(filepath.Join(repoDir, "services", "orders", "kafka.go"))
	push("drop consumers")
	rep4 := analyze(true)
	if rep4.DiffSource != DiffGit || rep4.Changed != 1 || rep4.Items != rep3.Items-kafkaItems {
		t.Fatalf("git-diff report %+v (before %d items, kafka.go %d)", rep4, rep3.Items, kafkaItems)
	}
	if n := count(`SELECT count(*) FROM inventory_items WHERE repository_id = $1 AND file = 'services/orders/kafka.go'`, repoID); n != 0 {
		t.Fatalf("items of a deleted file still stored: %d", n)
	}
	if again := llm.reset(); len(again) != 0 {
		t.Fatalf("unchanged code re-sent to the LLM: %v", again)
	}

	// HEAD == last_analyzed_sha → zero-change incremental (monitors/suggestions still refreshed).
	push("nothing")
	rep5 := analyze(true)
	if rep5.Changed != 0 || rep5.Items != rep4.Items {
		t.Fatalf("no-change report %+v", rep5)
	}

	// Service layout change → full scan.
	_ = os.WriteFile(filepath.Join(repoDir, ".trooper", "config.yaml"), []byte("services: [{path: services/orders, name: orders-api}]\n"), 0o644)
	push("layout")
	if rep6 := analyze(false); rep6.FullReason == "" || rep6.Items != rep4.Items {
		t.Fatalf("layout report %+v", rep6)
	}
}

func TestWebhookState(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, env("TT_POSTGRES_URL", "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"))
	if err != nil {
		t.Skip("postgres not reachable:", err)
	}
	t.Cleanup(st.Close) // registered first: runs after the row cleanup
	_, _ = st.DB.Exec(ctx, `ALTER TABLE repositories ADD COLUMN IF NOT EXISTS pending_paths text[]`)
	cleanup := func() {
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, testProject)
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM codeintel_webhook_deliveries WHERE id LIKE 'it-9401-%'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := st.DB.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, 1, 'codeintel-it', 'codeintel-it-9401')`, testProject); err != nil {
		t.Fatal(err)
	}
	var integ, repoID int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config, status)
		VALUES ($1, 'github', 'acme', '{"installationId": 940100}', 'ok') RETURNING id`, testProject).Scan(&integ); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, default_branch, installation_id, status)
		VALUES ($1, 'github', 'Acme/Shop', 'main', 940100, 'ready') RETURNING id`, testProject).Scan(&repoID); err != nil {
		t.Fatal(err)
	}

	if fresh, err := st.RecordDelivery(ctx, "it-9401-a", "push"); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	if fresh, _ := st.RecordDelivery(ctx, "it-9401-a", "push"); fresh {
		t.Fatal("duplicate delivery accepted")
	}
	_ = st.ForgetDelivery(ctx, "it-9401-a")
	if fresh, _ := st.RecordDelivery(ctx, "it-9401-a", "push"); !fresh {
		t.Fatal("forgotten delivery not accepted again")
	}

	repos, err := st.ReposByFullName(ctx, "acme/shop")
	if err != nil || len(repos) != 1 || repos[0].ID != repoID || repos[0].InstallationID != 940100 {
		t.Fatalf("%+v %v", repos, err)
	}
	_ = st.RequestAnalysis(ctx, repoID, []string{"b.go", "a.go"})
	_ = st.RequestAnalysis(ctx, repoID, []string{"a.go", "c.go"})
	var pending []string
	_ = st.DB.QueryRow(ctx, `SELECT pending_paths FROM repositories WHERE id = $1`, repoID).Scan(&pending)
	if strings.Join(pending, ",") != "a.go,b.go,c.go" {
		t.Fatalf("pending %v", pending)
	}
	big := make([]string, store.MaxPendingPaths+1)
	for i := range big {
		big[i] = "f" + string(rune('a'+i%26)) + strings.Repeat("x", i)
	}
	_ = st.RequestAnalysis(ctx, repoID, big)
	_ = st.DB.QueryRow(ctx, `SELECT pending_paths FROM repositories WHERE id = $1`, repoID).Scan(&pending)
	if strings.Join(pending, ",") != "*" {
		t.Fatalf("big push must collapse to full scan: %d", len(pending))
	}

	// Claim → error keeps the work: pending restored, status error.
	started, got, ok, err := st.Claim(ctx, repoID, time.Minute)
	if err != nil || !ok || strings.Join(got, ",") != "*" {
		t.Fatal(got, ok, err)
	}
	if err := st.FinishError(ctx, repoID, started, "boom", got); err != nil {
		t.Fatal(err)
	}
	var status, msg string
	_ = st.DB.QueryRow(ctx, `SELECT status, coalesce(error, ''), pending_paths FROM repositories WHERE id = $1`, repoID).Scan(&status, &msg, &pending)
	if status != "error" || msg != "boom" || strings.Join(pending, ",") != "*" {
		t.Fatalf("%s %s %v", status, msg, pending)
	}

	if n, err := st.SetInstallationRepos(ctx, 940100, []string{"acme/shop"}, false); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, _, ok, _ := st.Claim(ctx, repoID, time.Minute); ok {
		t.Fatal("disconnected repo claimed")
	}
	if n, _ := st.SetInstallationRepos(ctx, 940100, []string{"ACME/shop"}, true); n != 1 {
		t.Fatal("reconnect")
	}
	if err := st.InstallationGone(ctx, 940100, "removed"); err != nil {
		t.Fatal(err)
	}
	var istatus, rstatus string
	_ = st.DB.QueryRow(ctx, `SELECT i.status, r.status FROM integrations i, repositories r WHERE i.id = $1 AND r.id = $2`, integ, repoID).Scan(&istatus, &rstatus)
	if istatus != "error" || rstatus != "disconnected" {
		t.Fatalf("integration %s repo %s", istatus, rstatus)
	}
	if err := st.InstallationBack(ctx, 940100); err != nil {
		t.Fatal(err)
	}
	_ = st.DB.QueryRow(ctx, `SELECT i.status, r.status FROM integrations i, repositories r WHERE i.id = $1 AND r.id = $2`, integ, repoID).Scan(&istatus, &rstatus)
	if istatus != "ok" || rstatus != "pending" {
		t.Fatalf("unsuspend: integration %s repo %s", istatus, rstatus)
	}
}
