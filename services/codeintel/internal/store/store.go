// Package store is codeintel's Postgres access (contracts §13/§18; deploy/postgres/005_codeintel.sql, 009_slack_github.sql):
// repository claims, inventory, alert suggestions, generated load test drafts, alert-as-code monitors and webhook state.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/ascode"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/observe"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/suggest"
)

// FullScan in pending_paths forces a full analysis (set by the webhook for big pushes, or by the api for a manual re-scan).
const FullScan = "*"

// MaxPendingPaths: more changed files than this → full scan.
const MaxPendingPaths = 200

// Store wraps a pgx pool.
type Store struct{ DB *pgxpool.Pool }

// Open connects to Postgres.
func Open(ctx context.Context, url string) (*Store, error) {
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{DB: p}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.DB.Close() }

// Ping checks the connection (healthz).
func (s *Store) Ping(ctx context.Context) error { return s.DB.Ping(ctx) }

const repoCols = `r.id, r.project_id, p.org_id, r.provider, r.full_name, r.default_branch, coalesce(r.last_analyzed_sha, ''), r.status,
	coalesce(r.installation_id, 0), coalesce(r.credentials_ref, ''), coalesce(r.local_path, ''), coalesce(r.webhook_secret_ref, ''), coalesce(r.base_url, ''),
	r.analyze_requested_at, r.analyzed_at, r.analysis_started_at, r.analyze_requested_by`

func scanRepo(r pgx.CollectableRow) (model.Repo, error) {
	var x model.Repo
	err := r.Scan(&x.ID, &x.ProjectID, &x.OrgID, &x.Provider, &x.FullName, &x.DefaultBranch, &x.LastAnalyzedSHA, &x.Status,
		&x.InstallationID, &x.CredentialsRef, &x.LocalPath, &x.WebhookSecretRef, &x.BaseURL, &x.AnalyzeRequestedAt, &x.AnalyzedAt, &x.AnalysisStartedAt, &x.AnalyzeRequestedBy)
	return x, err
}

// Repos returns every connected repository.
func (s *Store) Repos(ctx context.Context) ([]model.Repo, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+repoCols+` FROM repositories r JOIN projects p ON p.id = r.project_id
		WHERE r.status <> 'disconnected' ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRepo)
}

// Repo returns one repository.
func (s *Store) Repo(ctx context.Context, id int64) (model.Repo, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+repoCols+` FROM repositories r JOIN projects p ON p.id = r.project_id WHERE r.id = $1`, id)
	if err != nil {
		return model.Repo{}, err
	}
	return pgx.CollectExactlyOneRow(rows, scanRepo)
}

// ReposByFullName returns GitHub repositories with this owner/name (any project; case-insensitive).
func (s *Store) ReposByFullName(ctx context.Context, fullName string) ([]model.Repo, error) {
	return s.ReposByName(ctx, "github", fullName)
}

// ReposByName returns the repositories of a provider with this full name (any project; case-insensitive). Webhook
// handlers then keep only those whose webhook secret verifies the delivery.
func (s *Store) ReposByName(ctx context.Context, provider, fullName string) ([]model.Repo, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+repoCols+` FROM repositories r JOIN projects p ON p.id = r.project_id
		WHERE r.provider = $1 AND lower(r.full_name) = lower($2) ORDER BY r.id`, provider, fullName)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRepo)
}

// WebhookReceived records a verified webhook delivery for these repositories (api: webhookActive).
func (s *Store) WebhookReceived(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.DB.Exec(ctx, `UPDATE repositories SET webhook_received_at = now() WHERE id = ANY($1)`, ids)
	return err
}

// Claim marks the repository as analyzing. ok=false when another worker holds a fresh claim or it is disconnected.
// pending is the changed-file list collected since the last claim (cleared by the claim; restored by FinishError).
func (s *Store) Claim(ctx context.Context, id int64, staleAfter time.Duration) (started time.Time, pending []string, ok bool, err error) {
	err = s.DB.QueryRow(ctx, `WITH old AS (SELECT id, coalesce(pending_paths, '{}') AS pp FROM repositories WHERE id = $1 FOR UPDATE)
		UPDATE repositories r SET status = 'analyzing', analysis_started_at = now(), error = NULL, pending_paths = NULL
		FROM old WHERE r.id = old.id AND r.status <> 'disconnected'
		  AND (r.status <> 'analyzing' OR r.analysis_started_at IS NULL OR r.analysis_started_at < now() - make_interval(secs => $2))
		RETURNING r.analysis_started_at, old.pp`, id, staleAfter.Seconds()).Scan(&started, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil, false, nil
	}
	return started, pending, err == nil, err
}

// FinishOK records a successful analysis. A re-analysis requested while this one ran keeps the repo due
// (analyzed_at = start time). No-op when the claim was lost (stale retake or disconnect). summary (nil: kept) is the
// repositories.analysis jsonb the api passes through (LLM status, contracts §19).
func (s *Store) FinishOK(ctx context.Context, id int64, started time.Time, sha string, languages, services []string, summary *model.Analysis) error {
	var sum []byte
	if summary != nil {
		sum, _ = json.Marshal(summary)
	}
	_, err := s.DB.Exec(ctx, `UPDATE repositories SET status = 'ready', error = NULL, analysis_started_at = NULL,
		analyzed_at = CASE WHEN analyze_requested_at > $2 THEN $2 ELSE now() END,
		last_analyzed_sha = $3, languages = $4, services = $5, analysis = coalesce($6::jsonb, analysis)
		WHERE id = $1 AND status = 'analyzing' AND analysis_started_at = $2`, id, started, sha, nonNil(languages), nonNil(services), sum)
	return err
}

// FinishError records a failed analysis and puts its changed files back (full scan → "*").
func (s *Store) FinishError(ctx context.Context, id int64, started time.Time, msg string, pending []string) error {
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	_, err := s.DB.Exec(ctx, `UPDATE repositories SET status = 'error', error = $3, analysis_started_at = NULL,
		pending_paths = (SELECT array_agg(DISTINCT x ORDER BY x) FROM unnest(coalesce(pending_paths, '{}') || $4::text[]) x)
		WHERE id = $1 AND status = 'analyzing' AND analysis_started_at = $2`, id, started, msg, nonNil(pending))
	return err
}

// RequestAnalysis marks a repository due and adds changed files (FullScan entry or >MaxPendingPaths → full scan).
func (s *Store) RequestAnalysis(ctx context.Context, id int64, paths []string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var cur []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(pending_paths, '{}') FROM repositories WHERE id = $1 FOR UPDATE`, id).Scan(&cur); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE repositories SET analyze_requested_at = now(), analyze_requested_by = NULL, pending_paths = $2 WHERE id = $1`, id, MergePaths(cur, paths))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MergePaths unions changed-file lists; anything over MaxPendingPaths collapses to [FullScan].
func MergePaths(a, b []string) []string {
	set := map[string]bool{}
	for _, l := range [][]string{a, b} {
		for _, p := range l {
			if p == FullScan {
				return []string{FullScan}
			}
			if p != "" {
				set[p] = true
			}
		}
	}
	if len(set) > MaxPendingPaths {
		return []string{FullScan}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// SetInstallationRepos marks repositories of an installation disconnected (removed from the App) or reconnects them
// (added back → pending + full re-analysis). Returns the number of rows changed.
func (s *Store) SetInstallationRepos(ctx context.Context, installationID int64, fullNames []string, connected bool) (int64, error) {
	if len(fullNames) == 0 {
		return 0, nil
	}
	lower := make([]string, len(fullNames))
	for i, n := range fullNames {
		lower[i] = strings.ToLower(n)
	}
	var sql string
	if connected {
		sql = `UPDATE repositories SET status = 'pending', analyze_requested_at = now(), analyze_requested_by = NULL, pending_paths = '{*}', installation_id = $1
			WHERE provider = 'github' AND lower(full_name) = ANY($2) AND status = 'disconnected'
			  AND (installation_id = $1 OR installation_id IS NULL)`
	} else {
		sql = `UPDATE repositories SET status = 'disconnected', analysis_started_at = NULL
			WHERE provider = 'github' AND installation_id = $1 AND lower(full_name) = ANY($2) AND status <> 'disconnected'`
	}
	tag, err := s.DB.Exec(ctx, sql, installationID, lower)
	return tag.RowsAffected(), err
}

// InstallationGone handles `installation` deleted/suspended: the github integration rows → error, their repos → disconnected.
func (s *Store) InstallationGone(ctx context.Context, installationID int64, reason string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE integrations SET status = 'error', error = $2
		WHERE provider = 'github' AND config->>'installationId' = $1`, strconv.FormatInt(installationID, 10), reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE repositories SET status = 'disconnected', analysis_started_at = NULL
		WHERE installation_id = $1 AND status <> 'disconnected'`, installationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InstallationBack handles `installation` unsuspend: integration ok, repos back to pending with a full re-analysis.
func (s *Store) InstallationBack(ctx context.Context, installationID int64) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE integrations SET status = 'ok', error = NULL
		WHERE provider = 'github' AND config->>'installationId' = $1`, strconv.FormatInt(installationID, 10)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE repositories SET status = 'pending', analyze_requested_at = now(), analyze_requested_by = NULL, pending_paths = '{*}'
		WHERE installation_id = $1 AND status = 'disconnected'`, installationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordDelivery stores a webhook delivery id; false when it was seen before (redelivery). Prunes ids older than 7 days.
func (s *Store) RecordDelivery(ctx context.Context, id, event string) (bool, error) {
	tag, err := s.DB.Exec(ctx, `INSERT INTO codeintel_webhook_deliveries (id, event) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, event)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 && time.Now().UnixNano()%50 == 0 {
		_, _ = s.DB.Exec(ctx, `DELETE FROM codeintel_webhook_deliveries WHERE received_at < now() - interval '7 days'`)
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetDelivery removes a delivery id so GitHub's redelivery is processed (after a processing failure).
func (s *Store) ForgetDelivery(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM codeintel_webhook_deliveries WHERE id = $1`, id)
	return err
}

// LoadSecret implements secrets.Store.
func (s *Store) LoadSecret(ctx context.Context, projectID, id int64) (nonce, ciphertext []byte, err error) {
	err = s.DB.QueryRow(ctx, `SELECT nonce, ciphertext FROM secrets WHERE id = $1 AND project_id = $2`, id, projectID).Scan(&nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, errors.New("secret not found")
	}
	return nonce, ciphertext, err
}

// ---------- inventory ----------

// Items returns the repository's stored inventory (no shadows).
func (s *Store) Items(ctx context.Context, repoID int64) ([]model.Item, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, kind, key, coalesce(source, ''), coalesce(service, ''), coalesce(file, ''), coalesce(line, 0),
		meta, criticality, observed, monitored, dead, coalesce(code_hash, ''), coalesce(summary, ''), llm
		FROM inventory_items WHERE repository_id = $1 AND NOT shadow ORDER BY id`, repoID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.Item, error) {
		var it model.Item
		var meta, llm []byte
		if err := r.Scan(&it.ID, &it.Kind, &it.Key, &it.Source, &it.Service, &it.File, &it.Line, &meta, &it.Criticality,
			&it.Observed, &it.Monitored, &it.Dead, &it.CodeHash, &it.Summary, &llm); err != nil {
			return it, err
		}
		_ = json.Unmarshal(meta, &it.Meta)
		if it.Meta != nil {
			it.Framework, _ = it.Meta["framework"].(string)
			it.Handler, _ = it.Meta["handler"].(string)
			it.Language, _ = it.Meta["language"].(string)
			delete(it.Meta, "framework")
			delete(it.Meta, "handler")
			delete(it.Meta, "language")
			if len(it.Meta) == 0 {
				it.Meta = nil
			}
		}
		if len(llm) > 0 && string(llm) != "null" {
			it.LLM = &model.LLMResult{}
			if err := json.Unmarshal(llm, it.LLM); err != nil {
				it.LLM = nil
			}
		}
		return it, nil
	})
}

func metaOf(it *model.Item) []byte {
	m := map[string]any{}
	for k, v := range it.Meta {
		m[k] = v
	}
	for k, v := range map[string]string{"framework": it.Framework, "handler": it.Handler, "language": it.Language} {
		if v != "" {
			m[k] = v
		}
	}
	if it.Stats != nil {
		m["stats"] = map[string]any{"count": it.Stats.Count, "errors": it.Stats.Errors, "p95": it.Stats.P95, "rpm": it.Stats.RPM,
			"errorRate": it.Stats.ErrorRate}
	}
	b, _ := json.Marshal(m)
	return b
}

func llmOf(it *model.Item) []byte {
	if it.LLM == nil {
		return nil
	}
	b, _ := json.Marshal(it.LLM)
	return b
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SaveItems replaces the repository's inventory: rows are matched by identity (kind, service, key) so ids — and the
// suggestions pointing at them — survive re-analysis; vanished items are deleted. items get their ids set. Shadow
// items (observed only) of the repository's services are replaced too.
func (s *Store) SaveItems(ctx context.Context, repo model.Repo, items []*model.Item, shadows []model.Item, services []string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id, kind, coalesce(service, ''), key FROM inventory_items WHERE repository_id = $1 AND NOT shadow`, repo.ID)
	if err != nil {
		return err
	}
	existing := map[string]int64{}
	for rows.Next() {
		var id int64
		var it model.Item
		if err := rows.Scan(&id, &it.Kind, &it.Service, &it.Key); err != nil {
			rows.Close()
			return err
		}
		existing[it.Identity()] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	keep := make([]int64, 0, len(items))
	for _, it := range items {
		args := []any{repo.ProjectID, repo.ID, it.Kind, it.Key, nullStr(it.Source), nullStr(it.File), it.Line, metaOf(it), it.Service,
			critOr(it.Criticality), it.Observed, it.Monitored, nullStr(it.CodeHash), nullStr(it.Summary), it.Dead, llmOf(it)}
		if id, ok := existing[it.Identity()]; ok {
			delete(existing, it.Identity())
			it.ID = id
			_, err = tx.Exec(ctx, `UPDATE inventory_items SET project_id = $1, repository_id = $2, kind = $3, key = $4, source = $5, file = $6,
				line = $7, meta = $8, service = $9, criticality = $10, observed = $11, monitored = $12, code_hash = $13, summary = $14,
				dead = $15, llm = $16, shadow = false, updated_at = now() WHERE id = $17`, append(args, id)...)
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO inventory_items (project_id, repository_id, kind, key, source, file, line, meta, service,
				criticality, observed, monitored, code_hash, summary, dead, llm)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id`, args...).Scan(&it.ID)
		}
		if err != nil {
			return fmt.Errorf("save %s %s: %w", it.Kind, it.Key, err)
		}
		keep = append(keep, it.ID)
	}
	gone := make([]int64, 0, len(existing))
	for _, id := range existing {
		gone = append(gone, id)
	}
	if len(gone) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM inventory_items WHERE id = ANY($1)`, gone); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM inventory_items WHERE project_id = $1 AND repository_id IS NULL AND shadow AND service = ANY($2)`,
		repo.ProjectID, nonNil(services)); err != nil {
		return err
	}
	for i := range shadows {
		sh := &shadows[i]
		if _, err := tx.Exec(ctx, `INSERT INTO inventory_items (project_id, repository_id, kind, key, service, criticality, observed, shadow, meta)
			VALUES ($1, NULL, $2, $3, $4, $5, true, true, $6)`, repo.ProjectID, sh.Kind, sh.Key, sh.Service, critOr(sh.Criticality), metaOf(sh)); err != nil {
			return err
		}
	}
	// Proposed suggestions whose item vanished (ON DELETE SET NULL) are stale.
	if _, err := tx.Exec(ctx, `DELETE FROM alert_suggestions WHERE project_id = $1 AND inventory_item_id IS NULL AND state = 'proposed'`,
		repo.ProjectID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func critOr(c string) string {
	if model.CriticalityRank(c) == 0 {
		return model.Low
	}
	return c
}

// MonitorRefs returns what the project's enabled monitors watch.
func (s *Store) MonitorRefs(ctx context.Context, projectID int64) ([]observe.MonitorRef, error) {
	rows, err := s.DB.Query(ctx, `SELECT coalesce(target->>'service', ''), coalesce(target->>'endpoint', ''),
		coalesce(target->>'process', query->>'process', ''), coalesce(query->>'route', '')
		FROM monitors WHERE project_id = $1 AND status IN ('active', 'muted')`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (observe.MonitorRef, error) {
		var m observe.MonitorRef
		err := r.Scan(&m.Service, &m.Endpoint, &m.Process, &m.Route)
		return m, err
	})
}

// Clusters returns the project's broker clusters.
func (s *Store) Clusters(ctx context.Context, projectID int64) ([]suggest.Cluster, error) {
	rows, err := s.DB.Query(ctx, `SELECT slug, kind FROM broker_clusters WHERE project_id = $1 ORDER BY slug`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (suggest.Cluster, error) {
		var c suggest.Cluster
		err := r.Scan(&c.Slug, &c.Kind)
		return c, err
	})
}

// SaveSuggestions upserts proposed suggestions (one per item and kind; accepted/rejected rows are never touched) and
// drops proposed suggestions of the repository's items that are no longer suggested (now monitored, less critical…).
func (s *Store) SaveSuggestions(ctx context.Context, repo model.Repo, list []suggest.Suggestion) (int, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	keepIDs := make([]int64, 0, len(list))
	keepKinds := make([]string, 0, len(list))
	written := 0
	for _, sg := range list {
		if sg.ItemID == 0 {
			continue
		}
		draft, _ := json.Marshal(sg.Draft)
		tag, err := tx.Exec(ctx, `INSERT INTO alert_suggestions (project_id, inventory_item_id, monitor_draft, rationale, confidence, title, kind,
			target, condition, criticality, code_ref, calibrated)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (inventory_item_id, kind) WHERE inventory_item_id IS NOT NULL DO UPDATE SET
			  monitor_draft = EXCLUDED.monitor_draft, rationale = EXCLUDED.rationale, confidence = EXCLUDED.confidence,
			  title = EXCLUDED.title, target = EXCLUDED.target, condition = EXCLUDED.condition, criticality = EXCLUDED.criticality,
			  code_ref = EXCLUDED.code_ref, calibrated = EXCLUDED.calibrated
			WHERE alert_suggestions.state = 'proposed'`,
			repo.ProjectID, sg.ItemID, draft, sg.Rationale, sg.Confidence, sg.Title, sg.Kind, sg.Target, sg.Condition, sg.Criticality,
			nullStr(sg.CodeRef), sg.Calibrated)
		if err != nil {
			return 0, err
		}
		written += int(tag.RowsAffected())
		keepIDs = append(keepIDs, sg.ItemID)
		keepKinds = append(keepKinds, sg.Kind)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM alert_suggestions a USING inventory_items i
		WHERE a.inventory_item_id = i.id AND i.repository_id = $1 AND a.state = 'proposed'
		  AND NOT EXISTS (SELECT 1 FROM unnest($2::bigint[], $3::text[]) k(item, kind) WHERE k.item = a.inventory_item_id AND k.kind = a.kind)`,
		repo.ID, keepIDs, keepKinds); err != nil {
		return 0, err
	}
	return written, tx.Commit(ctx)
}

// SaveScenarios upserts generated load test drafts by name (a draft someone promoted to 'ready' is left alone) and
// removes this repository's drafts that are no longer generated.
func (s *Store) SaveScenarios(ctx context.Context, repo model.Repo, list []suggest.Scenario) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	names := make([]string, 0, len(list))
	for _, sc := range list {
		spec, _ := json.Marshal(sc.Spec)
		var id int64
		var state string
		err := tx.QueryRow(ctx, `SELECT id, state FROM loadtest_scenarios WHERE project_id = $1 AND source = 'generated' AND name = $2
			ORDER BY id LIMIT 1`, repo.ProjectID, sc.Name).Scan(&id, &state)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `INSERT INTO loadtest_scenarios (project_id, name, spec, source, state) VALUES ($1, $2, $3, 'generated', 'draft')`,
				repo.ProjectID, sc.Name, spec)
		case err == nil && state == "draft":
			_, err = tx.Exec(ctx, `UPDATE loadtest_scenarios SET spec = $2 WHERE id = $1`, id, spec)
		}
		if err != nil {
			return err
		}
		names = append(names, sc.Name)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM loadtest_scenarios WHERE project_id = $1 AND source = 'generated' AND state = 'draft'
		AND spec->'generated'->>'repo' = $2 AND NOT (name = ANY($3))
		AND NOT EXISTS (SELECT 1 FROM loadtest_runs r WHERE r.scenario_id = loadtest_scenarios.id)`, repo.ProjectID, repo.FullName, names); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SyncResult counts alert-as-code changes.
type SyncResult struct{ Created, Updated, Deleted int }

// SyncCodeMonitors upserts the repository's `.trooper/monitors` (source='code', code_ref='<repo>:<path>', keyed by
// code_ref + name) and deletes code monitors whose file or entry vanished. Files that failed to parse keep their monitors.
func (s *Store) SyncCodeMonitors(ctx context.Context, repo model.Repo, mons []ascode.Monitor, broken []string) (SyncResult, error) {
	var res SyncResult
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	prefix := repo.FullName + ":"
	rows, err := tx.Query(ctx, `SELECT id, code_ref, name FROM monitors WHERE project_id = $1 AND source = 'code' AND starts_with(code_ref, $2)`,
		repo.ProjectID, prefix)
	if err != nil {
		return res, err
	}
	existing := map[string]int64{}
	refOf := map[int64]string{}
	for rows.Next() {
		var id int64
		var ref, name string
		if err := rows.Scan(&id, &ref, &name); err != nil {
			rows.Close()
			return res, err
		}
		existing[ref+"\x00"+name] = id
		refOf[id] = ref
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, m := range mons {
		ref := prefix + m.Path
		target, _ := json.Marshal(m.Target)
		query, _ := json.Marshal(m.Query)
		cond, _ := json.Marshal(m.Condition)
		if id, ok := existing[ref+"\x00"+m.Name]; ok {
			delete(existing, ref+"\x00"+m.Name)
			tag, err := tx.Exec(ctx, `UPDATE monitors SET kind = $2, target = $3, query = $4, condition = $5, severity = $6, channels = $7,
				status = $8, updated_at = now() WHERE id = $1 AND (kind, target, query, condition, severity, channels, status)
				IS DISTINCT FROM ($2, $3::jsonb, $4::jsonb, $5::jsonb, $6, $7::text[], $8)`,
				id, m.Kind, target, query, cond, m.Severity, m.Channels, m.Status)
			if err != nil {
				return res, fmt.Errorf("%s %q: %w", m.Path, m.Name, err)
			}
			res.Updated += int(tag.RowsAffected())
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO monitors (project_id, name, kind, target, query, condition, severity, channels, source, status, code_ref)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'code', $9, $10)`,
			repo.ProjectID, m.Name, m.Kind, target, query, cond, m.Severity, m.Channels, m.Status, ref); err != nil {
			return res, fmt.Errorf("%s %q: %w", m.Path, m.Name, err)
		}
		res.Created++
	}
	keepRef := map[string]bool{}
	for _, p := range broken {
		keepRef[prefix+p] = true
	}
	var gone []int64
	for _, id := range existing {
		if !keepRef[refOf[id]] {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		tag, err := tx.Exec(ctx, `DELETE FROM monitors WHERE id = ANY($1)`, gone)
		if err != nil {
			return res, err
		}
		res.Deleted = int(tag.RowsAffected())
	}
	return res, tx.Commit(ctx)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
