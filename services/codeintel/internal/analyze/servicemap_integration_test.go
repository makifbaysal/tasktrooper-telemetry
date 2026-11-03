//go:build integration

// Folder → service mapping end to end (ADR-036) against the docker-compose Postgres + ClickHouse:
//
//	GOWORK=off go test -tags integration -run TestServiceMapping ./internal/analyze/
package analyze

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/observe"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/servicemap"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/source"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

const mapProject = 9402

const goHandler = `package main

import "net/http"

func main() {
	http.HandleFunc("POST /v1/%[1]s", create)
	http.ListenAndServe(":8080", nil)
}

func create(w http.ResponseWriter, r *http.Request) {}
`

func TestServiceMapping(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, env("TT_POSTGRES_URL", "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"))
	if err != nil {
		t.Skip("postgres not reachable:", err)
	}
	t.Cleanup(st.Close)
	chURL := env("TT_CLICKHOUSE_URL", "clickhouse://trooper:trooper@localhost:9000/telemetry")
	opts, err := clickhouse.ParseDSN(chURL)
	if err != nil {
		t.Fatal(err)
	}
	chw, err := clickhouse.Open(opts)
	if err != nil || chw.Ping(ctx) != nil {
		t.Skip("clickhouse not reachable")
	}
	t.Cleanup(func() { _ = chw.Close() })
	chr, err := observe.Open(ctx, chURL)
	if err != nil {
		t.Fatal(err)
	}
	exec1 := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.DB.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cleanup := func() {
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, mapProject)
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM orgs WHERE id = $1`, mapProject)
		_ = chw.Exec(context.Background(), `ALTER TABLE telemetry.red_1m DELETE WHERE project_id = 9402 SETTINGS mutations_sync = 1`)
	}
	cleanup()
	t.Cleanup(cleanup)
	exec1(`INSERT INTO orgs (id, name) VALUES ($1, 'servicemap-it')`, mapProject)
	exec1(`INSERT INTO projects (id, org_id, name, slug) VALUES ($1, $1, 'servicemap-it', 'servicemap-it-9402')`, mapProject)
	// telemetry knows "orders-api" only
	if err := chw.Exec(ctx, `INSERT INTO telemetry.red_1m (org_id, project_id, service, env, version, route, method, minute, count, errors,
		s2xx, s3xx, s4xx, s5xx, duration_sum, duration)
		SELECT 9402, 9402, 'orders-api', 'production', 'v1', '/v1/orders', 'POST', toStartOfMinute(now() - INTERVAL 1 HOUR), 100, 0, 0, 0, 0, 0, 0,
		  quantilesTDigestWeightedState(0.5, 0.75, 0.95, 0.99)(toFloat64(20), toUInt32(1))`); err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	for _, svc := range []string{"orders", "billing"} {
		dir := filepath.Join(repoDir, "svc", svc)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/"+svc+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(strings.ReplaceAll(goHandler, "%[1]s", svc)), 0o644)
	}
	git(t, repoDir, "init", "-q", "-b", "main")
	git(t, repoDir, "add", ".")
	git(t, repoDir, "commit", "-q", "-m", "init")
	var repoID int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, default_branch, local_path)
		VALUES ($1, 'local', 'acme/mono', 'main', $2) RETURNING id`, mapProject, repoDir).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	an := &Analyzer{Store: st, Fetcher: &source.Fetcher{}, Traffic: chr, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	run := func() *Report {
		t.Helper()
		repo, err := st.Repo(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := an.Run(ctx, repo, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	mappings := func() map[string]servicemap.Mapping {
		t.Helper()
		ms, err := st.Mappings(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]servicemap.Mapping{}
		for _, m := range ms {
			out[m.Path] = m
		}
		return out
	}
	services := func() []string {
		t.Helper()
		items, err := st.Items(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, it := range items {
			if it.Kind == model.KindEndpoint {
				set[it.Service+" "+it.Key] = true
			}
		}
		var out []string
		for s := range set {
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}

	// 1. orders → orders-api by stem; billing has no telemetry service
	rep := run()
	ms := mappings()
	if m := ms["svc/orders"]; m.Status != servicemap.Matched || m.Service != "orders-api" || m.Detected != "orders" || m.Kind != "backend" || m.Evidence != "module" {
		t.Fatalf("orders mapping = %+v", m)
	}
	if m := ms["svc/billing"]; m.Status != servicemap.Unmatched || m.Service != "" {
		t.Fatalf("billing mapping = %+v", m)
	}
	if rep.Unmatched != 1 || rep.Folders != 2 {
		t.Fatalf("report folders=%d unmatched=%d", rep.Folders, rep.Unmatched)
	}
	if got := strings.Join(services(), ","); got != "billing POST /v1/billing,orders-api POST /v1/orders" {
		t.Fatalf("items after first analysis: %s", got)
	}

	// 2. the user links billing (what PUT /repos/{id}/services writes); the next analysis renames its items
	exec1(`UPDATE repo_service_mappings SET status = 'confirmed', service = 'billing-svc' WHERE repository_id = $1 AND path = 'svc/billing'`, repoID)
	run()
	if got := strings.Join(services(), ","); got != "billing-svc POST /v1/billing,orders-api POST /v1/orders" {
		t.Fatalf("items after confirm: %s", got)
	}
	if m := mappings()["svc/billing"]; m.Status != servicemap.Confirmed || m.Service != "billing-svc" {
		t.Fatalf("decision not kept: %+v", m)
	}

	// 3. ignore billing → its items leave the inventory, the row stays
	exec1(`UPDATE repo_service_mappings SET status = 'ignored', service = NULL WHERE repository_id = $1 AND path = 'svc/billing'`, repoID)
	run()
	if got := strings.Join(services(), ","); got != "orders-api POST /v1/orders" {
		t.Fatalf("items after ignore: %s", got)
	}
	if m := mappings()["svc/billing"]; m.Status != servicemap.Ignored {
		t.Fatalf("ignored row: %+v", m)
	}
}
