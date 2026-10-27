//go:build integration

// End to end against the compose Postgres (013_vcs_sources.sql applied): GitLab / Bitbucket deliveries through the real
// store. Uses project 9402 (created and removed by the test).
// Run: GOWORK=off go test -tags integration ./internal/webhook/ -run Integration -v
package webhook

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/obs"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

const hookProject = 9402

func TestIntegrationRepoHooks(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TT_POSTGRES_URL")
	if url == "" {
		url = "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Skipf("postgres: %v", err)
	}
	defer st.Close()
	cleanup := func() {
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, hookProject)
		_, _ = st.DB.Exec(context.Background(), `DELETE FROM codeintel_webhook_deliveries WHERE id LIKE '%:it-9402-%'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := st.DB.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, 1, 'codeintel-hooks-it', 'codeintel-hooks-it-9402')`, hookProject); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IT_HOOK_SECRET", "s3cret-one")
	var gl, bb int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, default_branch, status, base_url, webhook_secret_ref, analyzed_at)
	  VALUES ($1, 'gitlab', 'acme/platform/orders', 'main', 'ready', 'https://gitlab.example.com', 'env:IT_HOOK_SECRET', now()) RETURNING id`, hookProject).Scan(&gl); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, default_branch, status, webhook_secret_ref, analyzed_at)
	  VALUES ($1, 'bitbucket', 'acme-ws/orders', 'main', 'ready', 'env:IT_HOOK_SECRET', now()) RETURNING id`, hookProject).Scan(&bb); err != nil {
		t.Fatal(err)
	}
	// base_url is gitlab-only and https-only (CHECK)
	if _, err := st.DB.Exec(ctx, `UPDATE repositories SET base_url = 'https://x' WHERE id = $1`, bb); err == nil {
		t.Fatal("base_url accepted on a bitbucket repository")
	}
	repo, err := st.Repo(ctx, gl)
	if err != nil || repo.BaseURL != "https://gitlab.example.com" {
		t.Fatalf("repo %+v %v", repo, err)
	}

	secret := func(ctx context.Context, r model.Repo) ([]byte, error) {
		return secrets.Resolve(ctx, r.WebhookSecretRef, r.ProjectID, nil, nil)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	glh := &RepoHook{Provider: "gitlab", Store: st, RepoSecret: secret, Metrics: obs.NewRegistry(), Log: log}
	bbh := &RepoHook{Provider: "bitbucket", Store: st, RepoSecret: secret, Metrics: obs.NewRegistry(), Log: log}

	state := func(id int64) (requested bool, pending []string, seen bool) {
		var req, hook *time.Time
		if err := st.DB.QueryRow(ctx, `SELECT CASE WHEN analyze_requested_at > analyzed_at THEN analyze_requested_at END, coalesce(pending_paths, '{}'), webhook_received_at
		  FROM repositories WHERE id = $1`, id).Scan(&req, &pending, &hook); err != nil {
			t.Fatal(err)
		}
		return req != nil, pending, hook != nil
	}

	// wrong token: nothing changes
	q := hookReq{fixture: "gitlab_push.json", headers: map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "nope", "X-Gitlab-Event-UUID": "it-9402-0"}}
	if rec := q.do(t, glh, "/"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d", rec.Code)
	}
	if req, _, seen := state(gl); req || seen {
		t.Fatal("unauthorized delivery changed the repo")
	}
	q.headers["X-Gitlab-Token"] = "s3cret-one"
	q.headers["X-Gitlab-Event-UUID"] = "it-9402-1"
	if rec := q.do(t, glh, "/"); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	req, pending, seen := state(gl)
	if !req || !seen || !reflect.DeepEqual(pending, []string{"cmd/api/main.go", "internal/refunds/handler.go", "internal/v0/orders.go"}) {
		t.Fatalf("gitlab push: requested=%v seen=%v pending=%v", req, seen, pending)
	}
	if rec := q.do(t, glh, "/"); rec.Code != http.StatusOK {
		t.Fatalf("duplicate %d", rec.Code)
	}
	q2 := hookReq{fixture: "gitlab_push_truncated.json", headers: map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "s3cret-one", "X-Gitlab-Event-UUID": "it-9402-2"}}
	if rec := q2.do(t, glh, "/"); rec.Code != http.StatusAccepted {
		t.Fatalf("%d", rec.Code)
	}
	if _, pending, _ := state(gl); !reflect.DeepEqual(pending, []string{store.FullScan}) {
		t.Fatalf("truncated → %v", pending)
	}

	qb := hookReq{fixture: "bitbucket_push.json", headers: map[string]string{"X-Event-Key": "repo:push", "X-Request-UUID": "it-9402-3", "X-Hook-UUID": "h"}, sign: "s3cret-one"}
	if rec := qb.do(t, bbh, "/"); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if req, pending, seen := state(bb); !req || !seen || !reflect.DeepEqual(pending, []string{store.FullScan}) {
		t.Fatalf("bitbucket push: %v %v %v", req, pending, seen)
	}
	var n int
	_ = st.DB.QueryRow(ctx, `SELECT count(*) FROM codeintel_webhook_deliveries WHERE id IN ('gitlab:it-9402-1', 'gitlab:it-9402-2', 'bitbucket:h:it-9402-3')`).Scan(&n)
	if n != 3 {
		t.Fatalf("%d delivery rows", n)
	}
}
