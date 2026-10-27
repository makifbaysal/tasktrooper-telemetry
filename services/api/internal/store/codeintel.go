package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/tq"
	"github.com/tasktrooper/telemetry/services/pkg/vcs"
)

// Code intelligence (ADR-014, contracts §13/§16). services/codeintel writes repositories / inventory_items /
// alert_suggestions; the api lists them, registers repos and queues analyses (analyze_requested_at).

// tqTx runs tenant-guarded statements inside a transaction.
type tqTx struct {
	ctx context.Context
	tx  pgx.Tx
}

type pgErrRow struct{ err error }

func (r pgErrRow) Scan(...any) error { return r.err }

func (t tqTx) QueryRow(sql string, args ...any) pgx.Row {
	q, err := tq.Bind(t.ctx, tq.Postgres, sql)
	if err != nil {
		return pgErrRow{err}
	}
	return t.tx.QueryRow(t.ctx, q, args...)
}

func (t tqTx) Exec(sql string, args ...any) (pgconn.CommandTag, error) {
	q, err := tq.Bind(t.ctx, tq.Postgres, sql)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return t.tx.Exec(t.ctx, q, args...)
}

// inTx runs fn in a transaction (committed when fn returns nil).
func (d *DB) inTx(ctx context.Context, fn func(t tqTx) error) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(tqTx{ctx: ctx, tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UICriticality maps codeintel criticality (critical|high|medium|low) to the UI's (critical|important|low).
func UICriticality(c string) string {
	switch c {
	case "critical":
		return "critical"
	case "high", "medium", "important":
		return "important"
	}
	return "low"
}

// ---------------------------------------------------------------- repositories

const repoSelect = `SELECT r.id, r.provider, r.full_name, r.default_branch, coalesce(r.last_analyzed_sha, ''), coalesce(r.status, ''),
  r.analyze_requested_at, r.analyzed_at, coalesce(r.error, ''), coalesce(r.languages, '{}'), coalesce(r.services, '{}'),
  coalesce(r.installation_id, 0), coalesce(r.local_path, ''), coalesce(r.pending_paths, '{}'), r.analysis,
  coalesce(r.base_url, ''), r.webhook_received_at FROM repositories r`

func scanRepo(row pgx.Row) (model.Repo, error) {
	var r model.Repo
	var id, inst int64
	var status string
	var requested, analyzed *time.Time
	var analysis []byte
	var hookSeen *time.Time
	if err := row.Scan(&id, &r.Provider, &r.Name, &r.Branch, &r.Sha, &status, &requested, &analyzed, &r.Error, &r.Languages,
		&r.Services, &inst, &r.LocalPath, &r.PendingPaths, &analysis,
		&r.BaseURL, &hookSeen); err != nil {
		return r, err
	}
	if len(analysis) > 0 && string(analysis) != "null" {
		r.Analysis = repoAnalysis(analysis) // codeintel's summary, incl. llm {status, reason?, credits} (contracts §19)
	}
	r.ID, r.InstallationID, r.LastAnalyzedSha = idStr(id), idStr(inst), r.Sha
	// GitHub App installations deliver push webhooks to codeintel; other repos once a verified delivery arrived.
	r.WebhookActive = inst > 0 || hookSeen != nil
	if hookSeen != nil {
		r.WebhookReceivedAt = ms(*hookSeen)
	}
	if r.Provider != vcs.Local {
		r.URL = strings.TrimSuffix(vcs.CloneURL(vcs.Host(r.Provider, r.BaseURL), r.Name), ".git")
	}
	if analyzed != nil {
		r.AnalyzedAt = ms(*analyzed)
	}
	if requested != nil {
		r.AnalyzeRequestedAt = ms(*requested)
	}
	if len(r.Languages) > 0 {
		r.Language = r.Languages[0]
	}
	r.Status = repoStatus(status, r.Error, requested, analyzed)
	return r, nil
}

// repoStatus derives the UI status: a pending analysis request wins over an older error.
func repoStatus(status, errMsg string, requested, analyzed *time.Time) string {
	switch {
	case status == "disconnected":
		return "disconnected"
	case requested != nil && (analyzed == nil || requested.After(*analyzed)):
		return "analyzing"
	case status == "analyzing":
		return "analyzing"
	case status == "error" || errMsg != "":
		return "error"
	case analyzed != nil:
		return "ready"
	}
	return "analyzing"
}

// ListRepos returns the project's connected repositories.
func (d *DB) ListRepos(ctx context.Context) ([]model.Repo, error) {
	rows, err := d.pg.Query(ctx, repoSelect+` WHERE {{tenant:r}} ORDER BY r.full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Repo{}
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRepo returns one repository.
func (d *DB) GetRepo(ctx context.Context, id string) (*model.Repo, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	r, err := scanRepo(d.pg.QueryRow(ctx, repoSelect+` WHERE {{tenant:r}} AND r.id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

var branchName = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*$`)

// hookSecretBytes is the size of generated repository webhook secrets.
const hookSecretBytes = 32

// usesRepoWebhook: repositories notified by a per-repository webhook (not the GitHub App).
func usesRepoWebhook(provider string, installationID int64) bool {
	switch provider {
	case vcs.GitLab, vcs.Bitbucket:
		return true
	case vcs.GitHub:
		return installationID == 0
	}
	return false
}

// CreateRepo registers a repository and queues its first analysis. A token is sealed into secrets
// (credentials_ref); a GitHub App installation must be one of the project's github integrations. GitLab, Bitbucket
// and App-less GitHub repositories get a generated webhook secret (webhook_secret_ref), returned once in
// Repo.Webhook (the server adds the URL) — contracts §18a.
func (d *DB) CreateRepo(ctx context.Context, in model.RepoInput) (*model.Repo, error) {
	in.FullName = strings.Trim(strings.TrimSpace(in.FullName), "/")
	in.LocalPath = strings.TrimPrefix(strings.TrimSpace(in.LocalPath), "file://")
	in.DefaultBranch = strings.TrimSpace(in.DefaultBranch)
	in.Token = strings.TrimSpace(in.Token)
	switch in.Provider {
	case vcs.GitHub, vcs.GitLab, vcs.Bitbucket:
		if err := vcs.ValidFullName(in.Provider, in.FullName); err != nil {
			return nil, Invalid(err.Error())
		}
		if in.LocalPath != "" {
			return nil, Invalid("localPath only applies to provider local")
		}
	case vcs.Local:
		if in.LocalPath == "" || !filepath.IsAbs(in.LocalPath) {
			return nil, Invalid("provider local needs an absolute localPath")
		}
		if in.FullName == "" {
			in.FullName = "local/" + filepath.Base(in.LocalPath)
		}
	default:
		return nil, Invalid("provider must be github|gitlab|bitbucket|local")
	}
	var base *string
	if strings.TrimSpace(in.BaseURL) != "" {
		if in.Provider != vcs.GitLab {
			return nil, Invalid("baseUrl only applies to provider gitlab (self-managed)")
		}
		b, err := vcs.NormalizeBaseURL(in.BaseURL)
		if err != nil {
			return nil, Invalid(err.Error())
		}
		if b != vcs.GitLabURL {
			base = &b
		}
	}
	if in.DefaultBranch == "" {
		in.DefaultBranch = "main"
	}
	if !branchName.MatchString(in.DefaultBranch) || strings.Contains(in.DefaultBranch, "..") || len(in.DefaultBranch) > 255 {
		return nil, Invalid("defaultBranch is not a valid branch name")
	}
	var inst *int64
	if in.InstallationID != 0 {
		if in.Provider != vcs.GitHub {
			return nil, Invalid("installationId only applies to provider github")
		}
		if in.Token != "" {
			return nil, Invalid("send either token or installationId")
		}
		var ok bool
		if err := d.pg.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM integrations WHERE {{tenant}} AND provider = 'github'
		  AND config->>'installationId' = $1)`, strconv.FormatInt(int64(in.InstallationID), 10)).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, Invalid("installationId is not a GitHub App installation of this project (install the app first)")
		}
		v := int64(in.InstallationID)
		inst = &v
	}
	var sealed []string
	cleanup := func() {
		for _, r := range sealed {
			_ = d.deleteSecret(context.WithoutCancel(ctx), r)
		}
	}
	var ref *string
	if in.Token != "" {
		r, err := d.sealSecret(ctx, "repo token "+in.FullName, []byte(in.Token))
		if err != nil {
			return nil, err
		}
		sealed = append(sealed, r)
		ref = &r
	}
	var hookRef *string
	var hookSecret string
	if usesRepoWebhook(in.Provider, int64(in.InstallationID)) {
		hookSecret = randomToken(hookSecretBytes)
		r, err := d.sealSecret(ctx, "repo webhook "+in.FullName, []byte(hookSecret))
		if err != nil {
			cleanup()
			return nil, err
		}
		sealed = append(sealed, r)
		hookRef = &r
	}
	var local *string
	if in.LocalPath != "" {
		local = &in.LocalPath
	}
	var id int64
	err := d.pg.QueryRow(ctx, `INSERT INTO repositories (project_id, provider, full_name, default_branch, status, credentials_ref,
	    installation_id, local_path, base_url, webhook_secret_ref, analyze_requested_at)
	  VALUES ({{project}}, $1, $2, $3, 'pending', $4, $5, $6, $7, $8, now()) RETURNING id`,
		in.Provider, in.FullName, in.DefaultBranch, ref, inst, local, base, hookRef).Scan(&id)
	if err != nil {
		cleanup()
		if isUnique(err) {
			return nil, Conflict("repository already connected")
		}
		return nil, err
	}
	r, err := d.GetRepo(ctx, strconv.FormatInt(id, 10))
	if err == nil && hookSecret != "" {
		r.Webhook = &model.RepoWebhook{Secret: hookSecret}
	}
	return r, err
}

// RotateRepoWebhookSecret replaces a repository's webhook secret and returns the new one once (Repo.Webhook).
// GitHub App repositories have no per-repository webhook.
func (d *DB) RotateRepoWebhookSecret(ctx context.Context, id string) (*model.Repo, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	r, err := d.GetRepo(ctx, id)
	if err != nil {
		return nil, err
	}
	inst, _ := strconv.ParseInt(r.InstallationID, 10, 64)
	if !usesRepoWebhook(r.Provider, inst) {
		return nil, Invalid("this repository is notified by the GitHub App (or is local); it has no webhook secret")
	}
	secret := randomToken(hookSecretBytes)
	ref, err := d.sealSecret(ctx, "repo webhook "+r.Name, []byte(secret))
	if err != nil {
		return nil, err
	}
	var old *string
	err = d.pg.QueryRow(ctx, `WITH prev AS (SELECT id, webhook_secret_ref FROM repositories WHERE {{tenant}} AND id = $1 FOR UPDATE)
	  UPDATE repositories r SET webhook_secret_ref = $2, webhook_received_at = NULL FROM prev WHERE r.id = prev.id
	  RETURNING prev.webhook_secret_ref`, n, ref).Scan(&old)
	if err != nil {
		_ = d.deleteSecret(context.WithoutCancel(ctx), ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if old != nil {
		if err := d.deleteSecret(ctx, *old); err != nil {
			return nil, err
		}
	}
	r, err = d.GetRepo(ctx, id)
	if err == nil {
		r.Webhook = &model.RepoWebhook{Secret: secret}
	}
	return r, err
}

// DeleteRepo disconnects a repository (inventory cascades) and deletes its sealed secrets.
func (d *DB) DeleteRepo(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	var cred, hook *string
	err = d.pg.QueryRow(ctx, `DELETE FROM repositories WHERE {{tenant}} AND id = $1 RETURNING credentials_ref, webhook_secret_ref`, n).Scan(&cred, &hook)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	for _, ref := range []*string{cred, hook} {
		if ref != nil {
			if err := d.deleteSecret(ctx, *ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// AnalyzeRepo queues an analysis (codeintel polls analyze_requested_at).
func (d *DB) AnalyzeRepo(ctx context.Context, id string) (*model.Repo, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE repositories SET analyze_requested_at = now(), pending_paths = '{*}', analyze_requested_by = $2
	  WHERE {{tenant}} AND id = $1`, n, nullID(ActorOf(ctx).UserID)) // session user; api keys / automation → NULL
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.GetRepo(ctx, id)
}

// ---------------------------------------------------------------- inventory

// InventoryFilter narrows GET /inventory.
type InventoryFilter struct {
	RepoID, Kind, Service string
}

// ListInventory returns the project's inventory items (inventory_items.project_id, so shadow items without a
// repository are included).
func (d *DB) ListInventory(ctx context.Context, f InventoryFilter) ([]model.InventoryItem, error) {
	where, args := "", []any{}
	if f.RepoID != "" {
		n, err := parseID(f.RepoID)
		if err != nil {
			return nil, Invalid("repoId must be numeric")
		}
		args = append(args, n)
		where += " AND i.repository_id = $" + strconv.Itoa(len(args))
	}
	if f.Kind != "" {
		args = append(args, f.Kind)
		where += " AND i.kind = $" + strconv.Itoa(len(args))
	}
	if f.Service != "" {
		args = append(args, f.Service)
		where += " AND i.service = $" + strconv.Itoa(len(args))
	}
	rows, err := d.pg.Query(ctx, `SELECT i.id, i.kind, i.key, coalesce(i.source, ''), coalesce(i.service, ''), coalesce(i.file, ''),
	    coalesce(i.line, 0), coalesce(i.criticality, ''), coalesce(i.observed, false), coalesce(i.monitored, false),
	    coalesce(i.dead, false), coalesce(i.shadow, false), coalesce(i.summary, ''), coalesce(i.repository_id, 0), i.llm
	  FROM inventory_items i WHERE {{tenant:i}}`+where+` ORDER BY i.kind, i.service, i.key LIMIT 10000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.InventoryItem{}
	for rows.Next() {
		var it model.InventoryItem
		var id, repo int64
		var crit string
		var llm []byte
		if err := rows.Scan(&id, &it.Kind, &it.Key, &it.Source, &it.Service, &it.File, &it.Line, &crit, &it.Observed, &it.Monitored,
			&it.Dead, &it.Shadow, &it.Summary, &repo, &llm); err != nil {
			return nil, err
		}
		it.ID, it.RepoID, it.Criticality = idStr(id), idStr(repo), UICriticality(crit)
		if len(llm) > 0 && string(llm) != "null" {
			it.Analysis = json.RawMessage(llm)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- suggestions

const suggestionSelect = `SELECT s.id, coalesce(nullif(s.title, ''), s.monitor_draft->>'name', ''), coalesce(nullif(s.kind, ''), s.monitor_draft->>'kind', ''),
  coalesce(s.target, ''), coalesce(s.condition, ''), coalesce(s.criticality, ''), s.confidence, s.rationale, coalesce(s.code_ref, ''),
  coalesce(s.calibrated, false), s.state, s.created_at, s.monitor_draft,
  coalesce((SELECT max(m.id) FROM monitors m WHERE m.suggestion_id = s.id AND m.project_id = s.project_id), 0)
  FROM alert_suggestions s`

func scanSuggestion(row pgx.Row) (model.CodeSuggestion, error) {
	var s model.CodeSuggestion
	var id, mon int64
	var crit string
	var created time.Time
	var draft []byte
	var conf float32
	if err := row.Scan(&id, &s.Title, &s.Kind, &s.Target, &s.Condition, &crit, &conf, &s.Rationale, &s.CodeRef, &s.Calibrated,
		&s.State, &created, &draft, &mon); err != nil {
		return s, err
	}
	s.ID, s.Criticality, s.Confidence, s.CreatedAt, s.MonitorID = idStr(id), UICriticality(crit), float64(conf), ms(created), idStr(mon)
	s.MonitorDraft = json.RawMessage(draft)
	return s, nil
}

// ListSuggestions returns monitor suggestions in a state (proposed by default; "all" = every state).
func (d *DB) ListSuggestions(ctx context.Context, state string) ([]model.CodeSuggestion, error) {
	where, args := "", []any{}
	switch state {
	case "":
		state = "proposed"
		fallthrough
	case "proposed", "accepted", "rejected":
		where, args = " AND s.state = $1", []any{state}
	case "all":
	default:
		return nil, Invalid("state must be proposed|accepted|rejected|all")
	}
	rows, err := d.pg.Query(ctx, suggestionSelect+` WHERE {{tenant:s}}`+where+` ORDER BY s.confidence DESC, s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CodeSuggestion{}
	for rows.Next() {
		s, err := scanSuggestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) getSuggestion(ctx context.Context, n int64) (*model.CodeSuggestion, error) {
	s, err := scanSuggestion(d.pg.QueryRow(ctx, suggestionSelect+` WHERE {{tenant:s}} AND s.id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// AcceptSuggestion creates the monitor of a proposed suggestion (monitor_draft = POST /monitors body, source
// 'suggestion', overrides from the request applied) and marks the suggestion accepted, in one transaction.
func (d *DB) AcceptSuggestion(ctx context.Context, id string, o model.SuggestionAccept) (*model.SuggestionAccepted, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	var monitorID int64
	err = d.inTx(ctx, func(t tqTx) error {
		var draft []byte
		var state string
		err := t.QueryRow(`SELECT monitor_draft, state FROM alert_suggestions WHERE {{tenant}} AND id = $1 FOR UPDATE`, n).Scan(&draft, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != "proposed" {
			return Conflict("suggestion is already " + state)
		}
		var in model.MonitorInput
		if err := json.Unmarshal(draft, &in); err != nil {
			return Invalid("monitor_draft is not a monitor: " + err.Error())
		}
		if o.Name != "" {
			in.Name = o.Name
		}
		if o.Severity != "" {
			in.Severity = o.Severity
		}
		if o.Channels != nil {
			in.Channels = o.Channels
		}
		if o.Status != "" {
			in.Status = o.Status
		}
		in.Source = "suggestion"
		target, query, cond, source, err := normalizeMonitor(&in)
		if err != nil {
			var inv ErrInvalid
			if errors.As(err, &inv) {
				return Invalid("monitor_draft: " + inv.Msg)
			}
			return err
		}
		if err := t.QueryRow(`INSERT INTO monitors (project_id, name, kind, target, query, condition, severity, channels, source, status, suggestion_id)
		  VALUES ({{project}}, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			in.Name, in.Kind, target, query, cond, in.Severity, in.Channels, source, in.Status, n).Scan(&monitorID); err != nil {
			return err
		}
		_, err = t.Exec(`UPDATE alert_suggestions SET state = 'accepted' WHERE {{tenant}} AND id = $1`, n)
		return err
	})
	if err != nil {
		return nil, err
	}
	s, err := d.getSuggestion(ctx, n)
	if err != nil {
		return nil, err
	}
	m, err := d.GetMonitor(ctx, strconv.FormatInt(monitorID, 10))
	if err != nil {
		return nil, err
	}
	return &model.SuggestionAccepted{Suggestion: *s, Monitor: *m}, nil
}

// RejectSuggestion marks a proposed suggestion rejected.
func (d *DB) RejectSuggestion(ctx context.Context, id string) (*model.CodeSuggestion, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE alert_suggestions SET state = 'rejected' WHERE {{tenant}} AND id = $1 AND state = 'proposed'`, n)
	if err != nil {
		return nil, err
	}
	s, err := d.getSuggestion(ctx, n)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, Conflict("suggestion is already " + s.State)
	}
	return s, nil
}

// repoAnalysis passes codeintel's analysis summary through, with llm.credits rounded to 2 decimals.
func repoAnalysis(raw []byte) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return json.RawMessage(raw)
	}
	if llm, ok := m["llm"].(map[string]any); ok {
		if c, ok := llm["credits"].(float64); ok {
			llm["credits"] = math.Round(c*100) / 100
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(raw)
	}
	return b
}
