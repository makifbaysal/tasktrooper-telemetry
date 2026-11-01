//go:build integration

package server

import (
	"context"
	"crypto/sha256"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

// itFixture is a tenant an integration test creates for itself (ADR-028: no test relies on seed rows such as
// org 1 / project 1 / tt_dev_* keys). Ids come from the sequences; t.Cleanup deletes the org (cascades).
type itFixture struct {
	OrgID, ProjectID  int64
	APIKey, IngestKey string
}

func (f itFixture) Project() string { return strconv.FormatInt(f.ProjectID, 10) }

func newFixture(t *testing.T, pool *pgxpool.Pool, label string) itFixture {
	t.Helper()
	ctx := context.Background()
	f := itFixture{APIKey: "tt_api_it_" + store.RandomToken(18), IngestKey: "tt_ingest_it_" + store.RandomToken(18)}
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ($1) RETURNING id`, "it-"+label).Scan(&f.OrgID); err != nil {
		t.Fatalf("fixture org: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM orgs WHERE id = $1`, f.OrgID); err != nil {
			t.Errorf("fixture cleanup: %v", err)
		}
	})
	if err := pool.QueryRow(ctx, `INSERT INTO projects (org_id, name, slug) VALUES ($1, $2, $2) RETURNING id`, f.OrgID, "it-"+label).Scan(&f.ProjectID); err != nil {
		t.Fatalf("fixture project: %v", err)
	}
	for kind, key := range map[string]string{"api": f.APIKey, "ingest": f.IngestKey} {
		h := sha256.Sum256([]byte(key))
		if _, err := pool.Exec(ctx, `INSERT INTO api_keys (project_id, name, kind, hash, prefix) VALUES ($1, 'it', $2, $3, $4)`,
			f.ProjectID, kind, h[:], key[:12]); err != nil {
			t.Fatalf("fixture %s key: %v", kind, err)
		}
	}
	return f
}
