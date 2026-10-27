//go:build integration

// AI quota (ADR-018, contracts §19) end to end against the docker-compose Postgres (make infra && make migrate):
// real enrich.LLM against a fake Messages API, real quota.Gate over aiquota + Postgres.
//
//	GOWORK=off go test -tags integration -run TestAnalyzeQuota ./internal/analyze/
//
// Uses a fresh org + project 9501 + one user (all removed by the test).
package analyze

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/enrich"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/quota"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/source"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

const quotaProject = 9501

func TestAnalyzeQuota(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, env("TT_POSTGRES_URL", "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"))
	if err != nil {
		t.Skip("postgres not reachable:", err)
	}
	t.Cleanup(st.Close)
	exec1 := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.DB.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	const email = "aiquota-it-9501@example.com"
	cleanup := func() {
		bg := context.Background()
		_, _ = st.DB.Exec(bg, `DELETE FROM orgs WHERE id = (SELECT org_id FROM projects WHERE id = $1)`, quotaProject)
		_, _ = st.DB.Exec(bg, `DELETE FROM projects WHERE id = $1`, quotaProject)
		_, _ = st.DB.Exec(bg, `DELETE FROM users WHERE email = $1`, email)
	}
	cleanup()
	t.Cleanup(cleanup)

	var orgID, userID int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO orgs (name, plan) VALUES ('aiquota-it', 'free') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(ctx, `INSERT INTO users (email, name) VALUES ($1, 'Quota IT') RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	exec1(`INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, userID)
	exec1(`INSERT INTO projects (id, org_id, name, slug) VALUES ($1, $2, 'aiquota-it', 'aiquota-it-9501')`, quotaProject, orgID)

	repoDir := t.TempDir()
	copyTree(t, filepath.Join("..", "extract", "testdata", "gorepo"), repoDir)
	_ = os.RemoveAll(filepath.Join(repoDir, ".trooper"))
	git(t, repoDir, "init", "-q", "-b", "main")
	git(t, repoDir, "add", ".")
	git(t, repoDir, "commit", "-q", "-m", "init")
	var repoID int64
	if err := st.DB.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, default_branch, local_path, analyze_requested_by)
		VALUES ($1, 'local', 'acme/quota', 'main', $2, $3) RETURNING id`, quotaProject, repoDir, userID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}

	// Fake Messages API: one result (id 0) per request, 1000 in / 200 out tokens → 0.6 credits at 3/15.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		out := `{"items":[{"id":0,"criticality":"high","business_domain":"orders","summary":"llm summary","failure_modes":[],"suggested_monitors":[],"loadtest_hint":{"payload_example":"","idempotent":true,"notes":""}}]}`
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg", "type": "message", "role": "assistant", "model": "it-model",
			"content": []any{map[string]any{"type": "text", "text": out}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 1000, "output_tokens": 200}})
	}))
	defer srv.Close()
	llm := enrich.NewLLM("k", "it-model", option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	llm.BatchSize = 1
	llm.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := aiquota.Parse("it-model=3/15", "", "")
	if err != nil {
		t.Fatal(err)
	}
	an := &Analyzer{Store: st, Fetcher: &source.Fetcher{}, LLM: llm, MaxLLMItems: 4,
		Quota: &quota.Gate{Store: quota.PGStore{Pool: st.DB}, Config: cfg}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	run := func() (*Report, model.Analysis) {
		t.Helper()
		started, _, ok, err := st.Claim(ctx, repoID, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim ok=%v err=%v", ok, err)
		}
		repo, err := st.Repo(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := an.Run(ctx, repo, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FinishOK(ctx, repoID, started, rep.SHA, rep.Languages, rep.Services, rep.Summary()); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := st.DB.QueryRow(ctx, `SELECT analysis FROM repositories WHERE id = $1`, repoID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var a model.Analysis
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("analysis %s: %v", raw, err)
		}
		return rep, a
	}
	llmItems := func() int {
		var n int
		_ = st.DB.QueryRow(ctx, `SELECT count(*) FROM inventory_items WHERE repository_id = $1 AND llm IS NOT NULL`, repoID).Scan(&n)
		return n
	}

	// 1. Org limit used up (free plan override 10, 10 credits already spent) → no LLM call, static fallback.
	exec1(`UPDATE orgs SET ai_monthly_credits = 10 WHERE id = $1`, orgID)
	exec1(`INSERT INTO ai_usage (org_id, project_id, feature, model, input_tokens, output_tokens, credits) VALUES ($1, $2, 'other', 'it-model', 0, 0, 10)`, orgID, quotaProject)
	rep, a := run()
	if calls.Load() != 0 || rep.LLM.Status != model.LLMQuotaExceeded || rep.LLM.Reason != aiquota.ReasonOrgLimit || rep.LLM.Skipped == 0 || rep.Items == 0 {
		t.Fatalf("org limit: calls %d report %+v", calls.Load(), rep.LLM)
	}
	if a.LLM.Status != "quota_exceeded" || a.LLM.Reason != "org_limit" || a.LLM.Credits != 0 || a.Items != rep.Items {
		t.Fatalf("stored analysis %+v", a)
	}
	if llmItems() != 0 {
		t.Fatal("items enriched despite the quota")
	}

	// 2. Org unlimited, member cap 0 for the requesting user → user_limit.
	exec1(`UPDATE orgs SET ai_monthly_credits = -1 WHERE id = $1`, orgID)
	exec1(`UPDATE memberships SET ai_monthly_credits = 0 WHERE org_id = $1 AND user_id = $2`, orgID, userID)
	if rep, _ = run(); calls.Load() != 0 || rep.LLM.Status != model.LLMQuotaExceeded || rep.LLM.Reason != aiquota.ReasonUserLimit {
		t.Fatalf("member cap: calls %d report %+v", calls.Load(), rep.LLM)
	}

	// 3. Automatic work (webhook → analyze_requested_by NULL) only counts against the org → LLM runs, usage recorded.
	if err := st.RequestAnalysis(ctx, repoID, []string{store.FullScan}); err != nil {
		t.Fatal(err)
	}
	var by *int64
	_ = st.DB.QueryRow(ctx, `SELECT analyze_requested_by FROM repositories WHERE id = $1`, repoID).Scan(&by)
	if by != nil {
		t.Fatalf("webhook request must clear analyze_requested_by, got %d", *by)
	}
	rep, a = run()
	n := calls.Load()
	if n == 0 || int(n) > 4 || rep.LLM.Status != model.LLMOK || rep.LLMItems != int(n) || llmItems() != int(n) {
		t.Fatalf("unlimited: calls %d report %+v items %d", n, rep.LLM, rep.LLMItems)
	}
	var rows int
	var credits float64
	var nullUsers int
	if err := st.DB.QueryRow(ctx, `SELECT count(*), coalesce(sum(credits), 0)::float8, count(*) FILTER (WHERE user_id IS NULL)
		FROM ai_usage WHERE org_id = $1 AND feature = 'codeintel.enrich' AND ref = $2 AND model = 'it-model'
		  AND input_tokens = 1000 AND output_tokens = 200 AND project_id = $3`, orgID, "repo:"+itoa(repoID), quotaProject).Scan(&rows, &credits, &nullUsers); err != nil {
		t.Fatal(err)
	}
	if rows != int(n) || nullUsers != rows || credits < 0.6*float64(n)-1e-6 || credits > 0.6*float64(n)+1e-6 || a.LLM.Credits != credits {
		t.Fatalf("usage rows %d (null users %d) credits %v analysis %+v calls %d", rows, nullUsers, credits, a.LLM, n)
	}
	// The usage the api will show: org total = 10 (seeded) + this analysis.
	s, err := cfg.Usage(ctx, quota.PGX{Q: st.DB}, orgID, &userID, time.Now())
	if err != nil || s.OrgLimit != aiquota.Unlimited || s.UserLimit != 0 || s.UserUsed != 0 || s.OrgUsed < 10+credits-1e-6 || s.OrgUsed > 10+credits+1e-6 {
		t.Fatalf("usage %+v err %v", s, err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
