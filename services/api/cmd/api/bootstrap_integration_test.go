//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/config"
)

// TestIntegrationBootstrap runs `api bootstrap` repeatedly against live Postgres (TT_POSTGRES_URL): idempotent ids,
// key reuse from the out file, rotation when the file is missing or stale, no rotation without an out file.
func TestIntegrationBootstrap(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, config.Load().PostgresURL)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close) // after the row cleanup below
	db := store.New(nil, pool)
	suffix := strings.ToLower(store.RandomToken(6))
	org, owner := "it-bootstrap-"+suffix, "it-bootstrap-"+suffix+"@example.com"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM orgs WHERE name = $1`, org)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE email = $1`, owner)
	})
	keyFile := filepath.Join(t.TempDir(), "nested", "run", "ingest-key")
	opts := bootstrapOpts{Org: org, Project: "Self Project", Owner: owner, KeyOut: keyFile, IngestURL: "http://localhost:4318", JSON: true}

	type step struct {
		name    string
		prepare func(t *testing.T)
		opts    func(o bootstrapOpts) bootstrapOpts
		created [3]bool // org, project, key
		sameDSN bool    // DSN equals the previous step's
		noDSN   bool
	}
	var orgID, projectID int64
	var prevDSN string
	steps := []step{
		{name: "first run creates everything", created: [3]bool{true, true, true}},
		{name: "second run reuses the key file", sameDSN: true},
		{name: "missing file rotates", prepare: func(t *testing.T) { _ = os.Remove(keyFile) }, created: [3]bool{false, false, true}},
		{name: "stale file rotates", prepare: func(t *testing.T) { _ = os.WriteFile(keyFile, []byte("tt_ingest_stale\n"), 0o600) }, created: [3]bool{false, false, true}},
		{name: "file holding a DSN is accepted", prepare: func(t *testing.T) {
			_ = os.WriteFile(keyFile, []byte(prevDSN+"\n"), 0o600)
		}, sameDSN: true},
		{name: "no out file keeps the existing key", opts: func(o bootstrapOpts) bootstrapOpts { o.KeyOut = ""; return o }, noDSN: true},
		{name: "project found by name, slug matches", opts: func(o bootstrapOpts) bootstrapOpts { o.Project = "self-project"; return o }, sameDSN: true},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.prepare != nil {
				s.prepare(t)
			}
			o := opts
			if s.opts != nil {
				o = s.opts(o)
			}
			var stdout, stderr bytes.Buffer
			if err := runBootstrap(ctx, db, o, &stdout, &stderr); err != nil {
				t.Fatalf("bootstrap: %v (%s)", err, stderr.String())
			}
			var out bootstrapJSON
			dec := json.NewDecoder(&stdout)
			if err := dec.Decode(&out); err != nil || dec.More() {
				t.Fatalf("stdout must be exactly one JSON object: %v %q", err, stdout.String())
			}
			if got := [3]bool{out.Created.Org, out.Created.Project, out.Created.Key}; got != s.created {
				t.Errorf("created = %v, want %v (%s)", got, s.created, stderr.String())
			}
			if orgID == 0 {
				orgID, projectID = out.OrgID, out.ProjectID
			}
			if out.OrgID != orgID || out.ProjectID != projectID {
				t.Errorf("ids changed: %d/%d, want %d/%d", out.OrgID, out.ProjectID, orgID, projectID)
			}
			switch {
			case s.noDSN:
				if out.DSN != "" {
					t.Errorf("dsn without key file: %q", out.DSN)
				}
				return
			case s.sameDSN && out.DSN != prevDSN:
				t.Errorf("dsn changed: %q, want %q", out.DSN, prevDSN)
			case !s.sameDSN && out.DSN == prevDSN:
				t.Errorf("dsn not rotated: %q", out.DSN)
			}
			prevDSN = out.DSN
			fi, err := os.Stat(keyFile)
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("key file: %v %v", err, fi)
			}
			if di, _ := os.Stat(filepath.Dir(keyFile)); di.Mode().Perm() != 0o700 {
				t.Errorf("key dir mode %v", di.Mode().Perm())
			}
			var active int
			_ = pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE project_id = $1 AND name = 'bootstrap' AND revoked_at IS NULL`, projectID).Scan(&active)
			if active != 1 {
				t.Errorf("active bootstrap keys: %d", active)
			}
			if !strings.HasPrefix(out.DSN, "http://tt_ingest_") || !strings.HasSuffix(out.DSN, "@localhost:4318/"+itoa(projectID)) {
				t.Errorf("dsn: %q", out.DSN)
			}
		})
	}
	var role string
	if err := pool.QueryRow(ctx, `SELECT m.role FROM memberships m JOIN users u ON u.id = m.user_id WHERE u.email = $1 AND m.org_id = $2`, owner, orgID).Scan(&role); err != nil || role != "owner" {
		t.Errorf("owner membership: %v %q", err, role)
	}
	var orgs int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM orgs WHERE name = $1`, org).Scan(&orgs)
	if orgs != 1 {
		t.Errorf("orgs named %s: %d", org, orgs)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
