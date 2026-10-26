//go:build integration

// Runs against the docker-compose Postgres (make infra && make migrate):
//
//	GOWORK=off go test -tags integration ./internal/store/
//
// Uses project 9301 (created and removed by the test).
package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

const testProject = 9301

func pgURL() string {
	if v := os.Getenv("TT_POSTGRES_URL"); v != "" {
		return v
	}
	return "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"
}

// Setup creates project 9301 + an integration; cleanup deletes the project (cascades).
func Setup(t *testing.T, s *Store, provider string) model.Integration {
	t.Helper()
	ctx := context.Background()
	_, _ = s.DB.Exec(ctx, `DELETE FROM projects WHERE id = $1`, testProject)
	if _, err := s.DB.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, 1, 'cloud-it', 'cloud-it-9301')`, testProject); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, testProject) })
	in := model.Integration{ProjectID: testProject, Provider: provider, Name: "it-" + provider}
	if err := s.DB.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config) VALUES ($1, $2, $3, '{}') RETURNING id`,
		testProject, provider, in.Name).Scan(&in.ID); err != nil {
		t.Fatal(err)
	}
	return in
}

func open(t *testing.T) *Store {
	s, err := Open(context.Background(), pgURL())
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestSyncDatabasesAndBrokers(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	in := Setup(t, s, "aws")
	// A row added by hand with a slug discovery will also produce.
	if _, err := s.DB.Exec(ctx, `INSERT INTO databases (project_id, slug, name, engine, host, port, collector) VALUES ($1, 'aws-manual', 'manual', 'postgres', 'h', 5432, 'agent')`, testProject); err != nil {
		t.Fatal(err)
	}
	dbs := []model.Database{
		{Name: "orders-db", Engine: "postgres", Host: "orders.rds", Port: 5432, Source: "AWS RDS · eu-central-1", Version: "16.3",
			Zone: "eu-central-1a", InstanceClass: "db.r6g.large", DiskGB: model.F(100), Meta: map[string]any{"role": "primary"}},
		{Name: "sessions", Engine: "redis", Host: "sessions.cache", Port: 6379},
		{Name: "manual", Engine: "mysql", Host: "other", Port: 3306},
	}
	slugs, err := s.SyncDatabases(ctx, in, dbs)
	if err != nil {
		t.Fatal(err)
	}
	if len(slugs) != 2 || slugs[0] != "aws-orders-db" || slugs[1] != "aws-sessions" {
		t.Fatalf("owned slugs %v", slugs)
	}
	var engine, collector string
	var iid *int64
	if err := s.DB.QueryRow(ctx, `SELECT engine, collector, integration_id FROM databases WHERE project_id=$1 AND slug='aws-manual'`, testProject).Scan(&engine, &collector, &iid); err != nil {
		t.Fatal(err)
	}
	if engine != "postgres" || collector != "agent" || iid != nil {
		t.Fatalf("manual row was touched: %s %s %v", engine, collector, iid)
	}
	var src, class, zone, ver string
	var disk float64
	var meta []byte
	if err := s.DB.QueryRow(ctx, `SELECT source, instance_class, zone, version, disk_gb, meta, collector FROM databases WHERE project_id=$1 AND slug='aws-orders-db'`, testProject).
		Scan(&src, &class, &zone, &ver, &disk, &meta, &collector); err != nil {
		t.Fatal(err)
	}
	if src != "AWS RDS · eu-central-1" || class != "db.r6g.large" || zone != "eu-central-1a" || ver != "16.3" || disk != 100 || collector != "cloud" {
		t.Fatalf("row %s %s %s %s %v %s", src, class, zone, ver, disk, collector)
	}

	// sessions vanished → meta.missing; orders-db updated.
	dbs[0].Version = "16.4"
	if _, err := s.SyncDatabases(ctx, in, dbs[:1]); err != nil {
		t.Fatal(err)
	}
	missing := func(slug string) bool {
		var m map[string]any
		var b []byte
		if err := s.DB.QueryRow(ctx, `SELECT meta FROM databases WHERE project_id=$1 AND slug=$2`, testProject, slug).Scan(&b); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(b, &m)
		return m["missing"] == true
	}
	if !missing("aws-sessions") || missing("aws-orders-db") || missing("aws-manual") {
		t.Fatal("missing flags wrong")
	}
	// Reappears → missing cleared.
	if _, err := s.SyncDatabases(ctx, in, dbs[:2]); err != nil {
		t.Fatal(err)
	}
	if missing("aws-sessions") {
		t.Fatal("missing not cleared")
	}
	if err := s.PatchDatabaseMeta(ctx, in, map[string]map[string]any{"aws-orders-db": {"state": "idle"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT meta->>'state' || ver FROM (SELECT meta, version AS ver FROM databases WHERE project_id=$1 AND slug='aws-orders-db') x`, testProject).Scan(&ver); err != nil || ver != "idle16.4" {
		t.Fatalf("patch: %v %s", err, ver)
	}

	brokers := []model.Broker{{Name: "sqs-eu-central-1", Kind: "sqs", Endpoint: "https://sqs.eu-central-1.amazonaws.com", Source: "Amazon SQS · eu-central-1"}}
	bs, err := s.SyncBrokers(ctx, in, brokers)
	if err != nil || len(bs) != 1 || bs[0] != "aws-sqs-eu-central-1" {
		t.Fatalf("brokers %v %v", bs, err)
	}
	if _, err := s.SyncBrokers(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	var bm []byte
	_ = s.DB.QueryRow(ctx, `SELECT meta FROM broker_clusters WHERE project_id=$1 AND slug='aws-sqs-eu-central-1'`, testProject).Scan(&bm)
	if string(bm) != `{"missing": true}` {
		t.Fatalf("broker meta %s", bm)
	}

	if err := s.SetStatus(ctx, in.ID, "ok", "", 3); err != nil {
		t.Fatal(err)
	}
	var st string
	var n int
	var last *time.Time
	_ = s.DB.QueryRow(ctx, `SELECT status, resources, last_sync_at FROM integrations WHERE id=$1`, in.ID).Scan(&st, &n, &last)
	if st != "ok" || n != 3 || last == nil {
		t.Fatalf("status %s %d %v", st, n, last)
	}
	list, err := s.Integrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range list {
		found = found || x.ID == in.ID
	}
	if !found {
		t.Fatal("integration not listed")
	}
}

func TestDeploysAndSecrets(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	in := Setup(t, s, "vercel")
	ts := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	d := model.DeployMarker{Service: "web", Env: "production", Version: "abc", Source: "vercel", State: "building", TS: ts, ExternalID: "dpl_1"}
	if err := s.UpsertDeploys(ctx, in, []model.DeployMarker{d}); err != nil {
		t.Fatal(err)
	}
	d.State = "ready"
	if err := s.UpsertDeploys(ctx, in, []model.DeployMarker{d}); err != nil {
		t.Fatal(err)
	}
	var n int
	var state string
	_ = s.DB.QueryRow(ctx, `SELECT count(*), max(state) FROM deploy_markers WHERE project_id=$1`, testProject).Scan(&n, &state)
	if n != 1 || state != "ready" {
		t.Fatalf("deploys %d %s", n, state)
	}

	key, _ := secrets.Key()
	nonce, ct, err := secrets.Seal(key, testProject, []byte(`neon-api-key`))
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.DB.QueryRow(ctx, `INSERT INTO secrets (project_id, name, nonce, ciphertext) VALUES ($1, 'neon', $2, $3) RETURNING id`, testProject, nonce, ct).Scan(&id); err != nil {
		t.Fatal(err)
	}
	got, err := secrets.Resolve(ctx, "secret:"+itoa(id), testProject, key, s)
	if err != nil || string(got) != "neon-api-key" {
		t.Fatalf("resolve %q %v", got, err)
	}
	if _, err := secrets.Resolve(ctx, "secret:"+itoa(id), 1, key, s); err == nil {
		t.Fatal("secret of another project must not resolve")
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }
