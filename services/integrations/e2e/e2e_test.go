//go:build e2e

// End-to-end: fake AWS + fake Vercel → trooper-cloud (store, factory, runner, OTLP sender) → the real
// ingest-gw (:4318) → Redpanda → ingest-writer → ClickHouse, plus Postgres deploy_markers / databases.
// Needs `make infra migrate` and running `make run-gw` + `make run-writer`.
//
//	GOWORK=off go test -tags e2e -count=1 -v ./e2e/
//
// Uses project 9301 (Postgres rows + ClickHouse rows are removed afterwards).
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/integrations/internal/factory"
	"github.com/tasktrooper/telemetry/services/integrations/internal/fakes"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
	"github.com/tasktrooper/telemetry/services/integrations/internal/otlp"
	"github.com/tasktrooper/telemetry/services/integrations/internal/runner"
	"github.com/tasktrooper/telemetry/services/integrations/internal/store"
)

const (
	project   = 9301
	ingestKey = "tt_cloud_e2e_9301_key"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func clickhouse(t *testing.T, q string) string {
	t.Helper()
	u := env("TT_E2E_CLICKHOUSE_HTTP", "http://trooper:trooper@localhost:8123/") + "?query=" + url.QueryEscape(q)
	resp, err := http.Post(u, "text/plain", nil)
	if err != nil {
		t.Fatalf("clickhouse: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("clickhouse %d: %s", resp.StatusCode, b)
	}
	return strings.TrimSpace(string(b))
}

func awsMetric(name string, d map[string]string) (float64, bool) {
	id := d["DBInstanceIdentifier"] + d["CacheClusterId"] + d["QueueName"] + d["Broker ID"] + d["Consumer Group"]
	v, ok := map[string]float64{
		"CPUUtilization/orders-db":                  37,
		"DatabaseConnections/orders-db":             42,
		"FreeStorageSpace/orders-db":                25 * (1 << 30),
		"EngineCPUUtilization/sessions-001":         12,
		"CacheHitRate/sessions-001":                 98,
		"CpuUser/1":                                 20,
		"CpuSystem/1":                               5,
		"BytesInPerSec/1":                           2_500_000,
		"SumOffsetLag/billing":                      1234,
		"ApproximateNumberOfMessagesVisible/emails": 80,
		"ApproximateAgeOfOldestMessage/emails":      65,
		"NumberOfMessagesSent/emails":               120,
	}[name+"/"+id]
	return v, ok
}

func TestCloudToClickHouse(t *testing.T) {
	ctx := context.Background()
	if resp, err := http.Get(env("TT_E2E_GW", "http://localhost:4318") + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Skipf("ingest-gw not running on :4318 (make run-gw): %v", err)
	}
	st, err := store.Open(ctx, env("TT_POSTGRES_URL", "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"))
	if err != nil {
		t.Skipf("postgres: %v", err)
	}
	t.Cleanup(st.Close) // registered first → runs after the row cleanup below (LIFO)

	// ── Postgres fixtures: project 9301, ingest key, secrets, integrations ──
	_, _ = st.DB.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
	mustExec(t, st, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, 1, 'cloud-e2e', 'cloud-e2e-9301')`, project)
	mustExec(t, st, `INSERT INTO api_keys (project_id, name, kind, hash, prefix) VALUES ($1, 'cloud e2e', 'ingest', digest($2::text, 'sha256'), 'tt_cloud')`, project, ingestKey)
	cleanCH := func() {
		for _, tbl := range []string{"db_1m", "broker_lag_1m", "broker_nodes_1m", "metrics"} {
			clickhouse(t, fmt.Sprintf("ALTER TABLE telemetry.%s DELETE WHERE project_id = %d SETTINGS mutations_sync = 1", tbl, project))
		}
	}
	cleanCH()
	t.Cleanup(func() {
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project)
		cleanCH()
	})
	key, _ := secrets.Key()
	nonce, ct, _ := secrets.Seal(key, project, []byte("vercel-token"))
	var secretID int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO secrets (project_id, name, nonce, ciphertext) VALUES ($1, 'vercel', $2, $3) RETURNING id`, project, nonce, ct).Scan(&secretID); err != nil {
		t.Fatal(err)
	}
	var awsID, vercelID int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config) VALUES ($1, 'aws', 'prod', '{"regions":["eu-central-1"],"services":["rds","elasticache","msk","sqs"],"sqsConsumers":{"emails":"mailer"}}') RETURNING id`, project).Scan(&awsID); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config, credentials_ref) VALUES ($1, 'vercel', 'team', '{"teamId":"team_1","projectMap":{"shop-web":"web"}}', $2) RETURNING id`,
		project, fmt.Sprintf("secret:%d", secretID)).Scan(&vercelID); err != nil {
		t.Fatal(err)
	}

	// ── fake providers ──
	minute := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	awsFake := fakes.NewAWS(t, minute, awsMetric)
	defer awsFake.Close()
	vercelFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vercel-token" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v10/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":"prj_web","name":"shop-web"}],"pagination":{"next":null}}`)
		case "/v6/deployments":
			_, _ = fmt.Fprintf(w, `{"deployments":[{"uid":"dpl_e2e","name":"shop-web","url":"shop-web-e2e.vercel.app","created":%d,"readyState":"READY","target":"production","projectId":"prj_web",
				"meta":{"githubCommitSha":"deadbeef","githubCommitMessage":"feat: e2e","githubCommitAuthorLogin":"akif"}}],"pagination":{"next":null}}`, minute.UnixMilli())
		default:
			w.WriteHeader(404)
		}
	}))
	defer vercelFake.Close()

	f := &factory.Factory{Key: key, Store: st, Endpoints: factory.Endpoints{AWS: awsFake.URL, Vercel: vercelFake.URL}}
	r := runner.New(runner.Options{
		Store:   st,
		Sender:  &otlp.Sender{MetricsURL: env("TT_E2E_GW", "http://localhost:4318") + "/v1/metrics", Keys: map[int64]string{project: ingestKey}, Version: "e2e"},
		Factory: f.Build,
		Log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	for _, id := range []int64{awsID, vercelID} {
		in := loadIntegration(t, st, id)
		if err := r.RunOnce(ctx, in); err != nil {
			t.Fatalf("integration %d: %v", id, err)
		}
	}

	// ── Postgres assertions ──
	var status string
	var resources int
	if err := st.DB.QueryRow(ctx, `SELECT status, resources FROM integrations WHERE id = $1`, awsID).Scan(&status, &resources); err != nil || status != "ok" || resources != 6 {
		t.Fatalf("aws integration status %q resources %d err %v", status, resources, err)
	}
	var dbs, brokers int
	_ = st.DB.QueryRow(ctx, `SELECT count(*) FROM databases WHERE project_id = $1 AND collector = 'cloud' AND integration_id = $2`, project, awsID).Scan(&dbs)
	_ = st.DB.QueryRow(ctx, `SELECT count(*) FROM broker_clusters WHERE project_id = $1 AND integration_id = $2`, project, awsID).Scan(&brokers)
	if dbs != 4 || brokers != 2 {
		t.Fatalf("discovered rows: databases %d brokers %d", dbs, brokers)
	}
	var svc, version, state, author string
	if err := st.DB.QueryRow(ctx, `SELECT service, version, state, author FROM deploy_markers WHERE project_id = $1 AND source = 'vercel' AND external_id = 'dpl_e2e'`, project).
		Scan(&svc, &version, &state, &author); err != nil || svc != "web" || version != "deadbeef" || state != "ready" || author != "akif" {
		t.Fatalf("deploy marker %s %s %s %s %v", svc, version, state, author, err)
	}

	// ── ClickHouse assertions (writer flushes ≤ 1 s; poll) ──
	minuteStr := minute.Format("2006-01-02 15:04:05")
	checks := []struct{ name, q, want string }{
		{"db_1m rds", fmt.Sprintf(`SELECT round(max(cpu),2), max(connections), round(max(disk),2) FROM telemetry.db_1m WHERE project_id=%d AND database_id='aws-orders-db' AND minute='%s'`, project, minuteStr), "0.37\t42\t0.75"},
		{"db_1m redis", fmt.Sprintf(`SELECT round(max(cpu),2), round(min(hit_ratio),2) FROM telemetry.db_1m WHERE project_id=%d AND database_id='aws-sessions' AND node='sessions-001'`, project), "0.12\t0.98"},
		{"broker_lag_1m sqs", fmt.Sprintf(`SELECT max(lag), round(max(produce_rate),2) FROM telemetry.broker_lag_1m WHERE project_id=%d AND cluster_id='aws-sqs-eu-central-1' AND consumer_group='mailer' AND topic='emails'`, project), "80\t2"},
		{"broker_lag_1m msk", fmt.Sprintf(`SELECT max(lag) FROM telemetry.broker_lag_1m WHERE project_id=%d AND cluster_id='aws-events' AND consumer_group='billing' AND topic='orders'`, project), "1234"},
		{"broker_nodes_1m", fmt.Sprintf(`SELECT round(max(cpu),2), round(max(net_in),2) FROM telemetry.broker_nodes_1m WHERE project_id=%d AND cluster_id='aws-events' AND node='1'`, project), "0.25\t2.5"},
	}
	deadline := time.Now().Add(45 * time.Second)
	for _, c := range checks {
		var got string
		for {
			got = clickhouse(t, c.q)
			if got == c.want || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// trooper.broker.oldest_age_s → broker_lag_1m.oldest_age_s (contracts §5, deploy/clickhouse/002_infra.sql).
	if got := clickhouse(t, fmt.Sprintf(`SELECT max(oldest_age_s) FROM telemetry.broker_lag_1m WHERE project_id=%d AND consumer_group='mailer'`, project)); got != "65" {
		t.Errorf("oldest_age_s: got %q want 65", got)
	}
}

func mustExec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.DB.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func loadIntegration(t *testing.T, st *store.Store, id int64) model.Integration {
	t.Helper()
	list, err := st.Integrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list {
		if in.ID == id {
			return in
		}
	}
	t.Fatalf("integration %d not found", id)
	return model.Integration{}
}
