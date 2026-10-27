package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// Slack App and GitHub App installations (ADR-017, contracts §17/§18). A Slack installation is one per org: the
// row lives in the installing project (its id is the AAD of the sealed bot token) and is read org-wide.

// SlackInstall is an org's Slack App installation with its decrypted bot token.
type SlackInstall struct {
	ID          int64
	ProjectID   int64
	TeamID      string
	TeamName    string
	BotUserID   string
	Token       string
	InstalledAt time.Time
}

type slackConfig struct {
	TeamID    string `json:"teamId"`
	TeamName  string `json:"teamName"`
	BotUserID string `json:"botUserId"`
	Scopes    string `json:"scopes,omitempty"`
}

// SlackInstallOf returns the org's Slack installation (ErrNotFound when none).
func (d *DB) SlackInstallOf(ctx context.Context, orgID int64) (*SlackInstall, error) {
	var in SlackInstall
	var cfg []byte
	var ref *string
	err := d.Pool.QueryRow(ctx, `SELECT i.id, i.project_id, i.config, i.credentials_ref, i.created_at FROM integrations i
	  JOIN projects p ON p.id = i.project_id WHERE p.org_id = $1 AND i.provider = 'slack' AND i.status <> 'disabled'
	  ORDER BY i.id DESC LIMIT 1`, orgID).Scan(&in.ID, &in.ProjectID, &cfg, &ref, &in.InstalledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var c slackConfig
	_ = json.Unmarshal(cfg, &c)
	in.TeamID, in.TeamName, in.BotUserID = c.TeamID, c.TeamName, c.BotUserID
	if ref != nil && *ref != "" {
		k, err := d.key()
		if err != nil {
			return nil, err
		}
		tok, err := secrets.Resolve(ctx, *ref, in.ProjectID, k, rawSecrets{d})
		if err != nil {
			return nil, err
		}
		in.Token = string(tok)
	}
	return &in, nil
}

// rawSecrets loads a secrets row by (project, id) without a request tenant (the installing project may differ
// from the caller's project inside the same org).
type rawSecrets struct{ d *DB }

func (r rawSecrets) LoadSecret(ctx context.Context, projectID, id int64) (nonce, ciphertext []byte, err error) {
	err = r.d.Pool.QueryRow(ctx, `SELECT nonce, ciphertext FROM secrets WHERE project_id = $1 AND id = $2`, projectID, id).Scan(&nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return nonce, ciphertext, err
}

// SaveSlackInstall replaces the org's Slack installation with a new one in the tenant project (bot token sealed).
func (d *DB) SaveSlackInstall(ctx context.Context, orgID int64, teamID, teamName, botUserID, scopes, token string) (int64, error) {
	if err := d.DeleteSlackInstall(ctx, orgID); err != nil && !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	ref, err := d.sealSecret(ctx, "slack bot token "+teamID, []byte(token))
	if err != nil {
		return 0, err
	}
	cfg, _ := json.Marshal(slackConfig{TeamID: teamID, TeamName: teamName, BotUserID: botUserID, Scopes: scopes})
	var id int64
	err = d.pg.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config, credentials_ref, status)
	  VALUES ({{project}}, 'slack', $1, $2, $3, 'ok')
	  ON CONFLICT (project_id, provider, name) DO UPDATE SET config = EXCLUDED.config, credentials_ref = EXCLUDED.credentials_ref,
	    status = 'ok', error = NULL RETURNING id`, "Slack · "+teamName, cfg, ref).Scan(&id)
	if err != nil {
		_ = d.deleteSecret(ctx, ref)
	}
	return id, err
}

// DeleteSlackInstall removes the org's Slack installation(s) and their sealed tokens.
func (d *DB) DeleteSlackInstall(ctx context.Context, orgID int64) error {
	rows, err := d.Pool.Query(ctx, `DELETE FROM integrations i USING projects p WHERE p.id = i.project_id AND p.org_id = $1 AND i.provider = 'slack'
	  RETURNING i.project_id, coalesce(i.credentials_ref, '')`, orgID)
	if err != nil {
		return err
	}
	type ref struct {
		project int64
		ref     string
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.project, &r.ref); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(refs) == 0 {
		return ErrNotFound
	}
	for _, r := range refs {
		if id, ok := secretID(r.ref); ok {
			if _, err := d.Pool.Exec(ctx, `DELETE FROM secrets WHERE project_id = $1 AND id = $2`, r.project, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// SlackStatus is GET /integrations/slack.
func (d *DB) SlackStatus(ctx context.Context, orgID int64) (model.SlackStatus, error) {
	in, err := d.SlackInstallOf(ctx, orgID)
	if errors.Is(err, ErrNotFound) {
		return model.SlackStatus{}, nil
	}
	if err != nil {
		return model.SlackStatus{}, err
	}
	return model.SlackStatus{Installed: true, ID: idStr(in.ID), TeamID: in.TeamID, TeamName: in.TeamName, InstalledAt: ms(in.InstalledAt)}, nil
}

// TenantOfIncident returns the tenant of an incident (Slack buttons carry bare ids).
func (d *DB) TenantOfIncident(ctx context.Context, id int64) (tenant.Tenant, error) {
	return d.tenantOf(ctx, `SELECT p.org_id, p.id FROM incidents x JOIN projects p ON p.id = x.project_id WHERE x.id = $1`, id)
}

// TenantOfMonitor returns the tenant of a monitor.
func (d *DB) TenantOfMonitor(ctx context.Context, id int64) (tenant.Tenant, error) {
	return d.tenantOf(ctx, `SELECT p.org_id, p.id FROM monitors x JOIN projects p ON p.id = x.project_id WHERE x.id = $1`, id)
}

func (d *DB) tenantOf(ctx context.Context, sql string, id int64) (tenant.Tenant, error) {
	var org, project int64
	err := d.Pool.QueryRow(ctx, sql, id).Scan(&org, &project)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.Tenant{}, ErrNotFound
	}
	return tenant.Tenant{OrgID: uint32(org), ProjectID: uint32(project)}, err
}

// MemberByEmail returns a user of the org by email with their role (ErrNotFound when not a member).
func (d *DB) MemberByEmail(ctx context.Context, orgID int64, email string) (int64, string, error) {
	var id int64
	var role string
	err := d.Pool.QueryRow(ctx, `SELECT u.id, m.role FROM users u JOIN memberships m ON m.user_id = u.id AND m.org_id = $1 WHERE lower(u.email) = lower($2)`,
		orgID, strings.TrimSpace(email)).Scan(&id, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return id, role, err
}

// ---------------------------------------------------------------- GitHub App

type githubConfig struct {
	InstallationID int64  `json:"installationId"`
	Account        string `json:"account"`
	AccountType    string `json:"accountType"`
}

// SaveGitHubInstall records an App installation for the tenant project (idempotent per installation).
func (d *DB) SaveGitHubInstall(ctx context.Context, installationID int64, account, accountType string) (int64, error) {
	cfg, _ := json.Marshal(githubConfig{InstallationID: installationID, Account: account, AccountType: accountType})
	var id int64
	err := d.pg.QueryRow(ctx, `SELECT id FROM integrations WHERE {{tenant}} AND provider = 'github' AND config->>'installationId' = $1`,
		strconv.FormatInt(installationID, 10)).Scan(&id)
	if err == nil {
		_, err = d.pg.Exec(ctx, `UPDATE integrations SET config = $2, status = 'ok', error = NULL WHERE {{tenant}} AND id = $1`, id, cfg)
		return id, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	name := "GitHub · " + account
	err = d.pg.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config, status) VALUES ({{project}}, 'github', $1, $2, 'ok')
	  ON CONFLICT (project_id, provider, name) DO UPDATE SET config = EXCLUDED.config, status = 'ok', error = NULL RETURNING id`, name, cfg).Scan(&id)
	return id, err
}

// GitHubInstalls lists the tenant project's GitHub App installations.
func (d *DB) GitHubInstalls(ctx context.Context) ([]model.GitHubInstall, error) {
	rows, err := d.pg.Query(ctx, `SELECT id, config, status, created_at FROM integrations WHERE {{tenant}} AND provider = 'github' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.GitHubInstall{}
	for rows.Next() {
		var g model.GitHubInstall
		var id int64
		var cfg []byte
		var at time.Time
		if err := rows.Scan(&id, &cfg, &g.Status, &at); err != nil {
			return nil, err
		}
		var c githubConfig
		_ = json.Unmarshal(cfg, &c)
		g.ID, g.InstallationID, g.Account, g.AccountType, g.CreatedAt = idStr(id), idStr(c.InstallationID), c.Account, c.AccountType, ms(at)
		out = append(out, g)
	}
	return out, rows.Err()
}

// GitHubInstallation returns the installation id of the tenant's github integration id.
func (d *DB) GitHubInstallation(ctx context.Context, integrationID string) (int64, error) {
	n, err := parseID(integrationID)
	if err != nil {
		return 0, err
	}
	var cfg []byte
	err = d.pg.QueryRow(ctx, `SELECT config FROM integrations WHERE {{tenant}} AND provider = 'github' AND id = $1`, n).Scan(&cfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	var c githubConfig
	if err := json.Unmarshal(cfg, &c); err != nil || c.InstallationID == 0 {
		return 0, Invalid("integration has no installationId")
	}
	return c.InstallationID, nil
}

// RepoNames returns the tenant's connected repositories of a provider (to mark picker entries as added).
func (d *DB) RepoNames(ctx context.Context, provider string) (map[string]bool, error) {
	rows, err := d.pg.Query(ctx, `SELECT lower(full_name) FROM repositories WHERE {{tenant}} AND provider = $1`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}
