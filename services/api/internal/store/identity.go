package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// Identity (ADR-016, contracts §15): users, sessions, org memberships, projects, api keys and the audit log.
// Session, user and membership lookups are how a tenant is found, so they run on the raw pool (like api key
// resolution); every statement is still keyed by the authenticated user or org id. Project-scoped tables
// (api_keys) go through the tenant guard.

// ConflictMsg is a 409 with a message; errors.Is(err, ErrConflict) holds.
type ConflictMsg struct{ Msg string }

func (e ConflictMsg) Error() string        { return e.Msg }
func (e ConflictMsg) Is(target error) bool { return target == ErrConflict }

// Conflict returns a ConflictMsg.
func Conflict(msg string) error { return ConflictMsg{Msg: msg} }

// Roles in ascending privilege.
var roleRank = map[string]int{"viewer": 1, "member": 2, "admin": 3, "owner": 4}

// RoleAtLeast reports whether role has at least the privileges of min.
func RoleAtLeast(role, min string) bool { return roleRank[role] >= roleRank[min] && roleRank[min] > 0 }

// ValidRole reports whether r is an org role.
func ValidRole(r string) bool { return roleRank[r] > 0 }

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomToken returns a URL-safe random string of n random bytes (state, nonce, PKCE verifier, session).
func RandomToken(n int) string { return randomToken(n) }

func sessionID(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func idStr(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

// ---------------------------------------------------------------- sessions

// CreateSession stores a session for userID and returns the cookie value (only its sha256 is stored).
func (d *DB) CreateSession(ctx context.Context, userID int64, userAgent string, ttl time.Duration) (string, error) {
	token := randomToken(32)
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	_, err := d.Pool.Exec(ctx, `INSERT INTO sessions (id, user_id, expires_at, user_agent) VALUES ($1, $2, now() + $3::interval, $4)`,
		sessionID(token), userID, strconv.FormatInt(int64(ttl/time.Second), 10)+" seconds", userAgent)
	if err != nil {
		return "", err
	}
	_, _ = d.Pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, userID)
	return token, nil
}

// SessionUser returns the user of a live session. Sessions seen more than a minute ago slide to now + ttl
// (refreshed = true: the caller re-sends the cookie with the new expiry). Unknown or expired → ErrNotFound.
func (d *DB) SessionUser(ctx context.Context, token string, ttl time.Duration) (userID int64, refreshed bool, err error) {
	if token == "" {
		return 0, false, ErrNotFound
	}
	id := sessionID(token)
	var stale bool
	err = d.Pool.QueryRow(ctx, `SELECT user_id, last_seen_at < now() - interval '1 minute' FROM sessions WHERE id = $1 AND expires_at > now()`, id).
		Scan(&userID, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, err
	}
	if stale {
		_, err = d.Pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now(), expires_at = now() + $2::interval WHERE id = $1`,
			id, strconv.FormatInt(int64(ttl/time.Second), 10)+" seconds")
		if err != nil {
			return 0, false, err
		}
	}
	return userID, stale, nil
}

// DeleteSession removes a session (logout); unknown tokens are ignored. Expired sessions are swept too.
func (d *DB) DeleteSession(ctx context.Context, token string) error {
	_, err := d.Pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1 OR expires_at < now() - interval '1 day'`, sessionID(token))
	return err
}

// SaveLoginState stores a pending OIDC login (10 minutes).
func (d *DB) SaveLoginState(ctx context.Context, state, verifier, nonce, redirect string) error {
	_, err := d.Pool.Exec(ctx, `DELETE FROM oidc_login_states WHERE expires_at < now()`)
	if err != nil {
		return err
	}
	_, err = d.Pool.Exec(ctx, `INSERT INTO oidc_login_states (state, verifier, nonce, redirect, expires_at) VALUES ($1, $2, $3, $4, now() + interval '10 minutes')`,
		state, verifier, nonce, redirect)
	return err
}

// TakeLoginState consumes a pending OIDC login (single use). Unknown or expired → ErrNotFound.
func (d *DB) TakeLoginState(ctx context.Context, state string) (verifier, nonce, redirect string, err error) {
	var live bool
	err = d.Pool.QueryRow(ctx, `DELETE FROM oidc_login_states WHERE state = $1 RETURNING verifier, nonce, redirect, expires_at > now()`, state).
		Scan(&verifier, &nonce, &redirect, &live)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return "", "", "", ErrNotFound
	}
	return verifier, nonce, redirect, err
}

// UserClaims identify a user at login.
type UserClaims struct {
	Subject       string // OIDC sub; empty for dev login
	Email         string
	EmailVerified bool
	Name          string
}

// UpsertUser finds or creates the user of a login. OIDC: by subject, then by verified email (links invited users);
// dev login: by email.
func (d *DB) UpsertUser(ctx context.Context, c UserClaims) (int64, error) {
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	if c.Email == "" || !strings.Contains(c.Email, "@") {
		return 0, Invalid("a valid email is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		c.Name, _, _ = strings.Cut(c.Email, "@")
	}
	var id int64
	if c.Subject != "" {
		err := d.Pool.QueryRow(ctx, `UPDATE users SET email = CASE WHEN $3 THEN $2 ELSE email END WHERE oidc_subject = $1 RETURNING id`,
			c.Subject, c.Email, c.EmailVerified && !d.emailTaken(ctx, c.Email, c.Subject)).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		if c.EmailVerified { // link an invited / dev-created user
			err = d.Pool.QueryRow(ctx, `UPDATE users SET oidc_subject = $1 WHERE email = $2 AND oidc_subject IS NULL RETURNING id`, c.Subject, c.Email).Scan(&id)
			if err == nil {
				return id, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return 0, err
			}
		}
		err = d.Pool.QueryRow(ctx, `INSERT INTO users (email, name, oidc_subject) VALUES ($1, $2, $3) RETURNING id`, c.Email, c.Name, c.Subject).Scan(&id)
		if isUnique(err) {
			return 0, Conflict("a user with this email already exists; log in with the identity linked to it")
		}
		return id, err
	}
	err := d.Pool.QueryRow(ctx, `INSERT INTO users (email, name) VALUES ($1, $2) ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email RETURNING id`,
		c.Email, c.Name).Scan(&id)
	return id, err
}

func (d *DB) emailTaken(ctx context.Context, email, subject string) bool {
	var n int
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = $1 AND oidc_subject IS DISTINCT FROM $2`, email, subject).Scan(&n)
	return n > 0
}

// ---------------------------------------------------------------- access

// ProjectAccess returns the org and the user's role for a project; ErrNotFound when the project is not in one of
// the user's orgs.
func (d *DB) ProjectAccess(ctx context.Context, userID, projectID int64) (orgID int64, role string, err error) {
	err = d.Pool.QueryRow(ctx, `SELECT p.org_id, m.role FROM projects p JOIN memberships m ON m.org_id = p.org_id AND m.user_id = $1 WHERE p.id = $2`,
		userID, projectID).Scan(&orgID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return orgID, role, err
}

// DefaultProject is the user's first project (lowest org id, then project id); ErrNotFound when there is none.
func (d *DB) DefaultProject(ctx context.Context, userID int64) (projectID, orgID int64, role string, err error) {
	err = d.Pool.QueryRow(ctx, `SELECT p.id, p.org_id, m.role FROM memberships m JOIN projects p ON p.org_id = m.org_id
	  WHERE m.user_id = $1 ORDER BY m.org_id, p.id LIMIT 1`, userID).Scan(&projectID, &orgID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, "", ErrNotFound
	}
	return projectID, orgID, role, err
}

// OrgRole returns the user's role in an org; ErrNotFound when not a member.
func (d *DB) OrgRole(ctx context.Context, userID, orgID int64) (string, error) {
	var role string
	err := d.Pool.QueryRow(ctx, `SELECT role FROM memberships WHERE user_id = $1 AND org_id = $2`, userID, orgID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// Me describes a user: their orgs (with role) and projects.
func (d *DB) Me(ctx context.Context, userID int64) (model.Me, error) {
	me := model.Me{Orgs: []model.MeOrg{}}
	var u model.User
	var uid int64
	err := d.Pool.QueryRow(ctx, `SELECT id, email, name FROM users WHERE id = $1`, userID).Scan(&uid, &u.Email, &u.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return me, ErrNotFound
	}
	if err != nil {
		return me, err
	}
	u.ID = idStr(uid)
	me.User = &u
	rows, err := d.Pool.Query(ctx, `SELECT o.id, o.name, m.role, p.id, coalesce(p.name, ''), coalesce(p.slug, '')
	  FROM memberships m JOIN orgs o ON o.id = m.org_id LEFT JOIN projects p ON p.org_id = o.id
	  WHERE m.user_id = $1 ORDER BY o.id, p.id`, userID)
	if err != nil {
		return me, err
	}
	defer rows.Close()
	for rows.Next() {
		var oid int64
		var pid *int64
		var name, role, pname, slug string
		if err := rows.Scan(&oid, &name, &role, &pid, &pname, &slug); err != nil {
			return me, err
		}
		if n := len(me.Orgs); n == 0 || me.Orgs[n-1].ID != idStr(oid) {
			me.Orgs = append(me.Orgs, model.MeOrg{ID: idStr(oid), Name: name, Role: role, Projects: []model.MeProject{}})
		}
		if pid != nil {
			o := &me.Orgs[len(me.Orgs)-1]
			o.Projects = append(o.Projects, model.MeProject{ID: idStr(*pid), Name: pname, Slug: slug})
		}
	}
	return me, rows.Err()
}

// ProjectMe describes an api key caller: the key's org and project only.
func (d *DB) ProjectMe(ctx context.Context, projectID int64, role string) (model.Me, error) {
	var oid int64
	var oname, pname, slug string
	err := d.Pool.QueryRow(ctx, `SELECT o.id, o.name, p.name, p.slug FROM projects p JOIN orgs o ON o.id = p.org_id WHERE p.id = $1`, projectID).
		Scan(&oid, &oname, &pname, &slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Me{}, ErrNotFound
	}
	if err != nil {
		return model.Me{}, err
	}
	return model.Me{Orgs: []model.MeOrg{{ID: idStr(oid), Name: oname, Role: role,
		Projects: []model.MeProject{{ID: idStr(projectID), Name: pname, Slug: slug}}}}}, nil
}

// ---------------------------------------------------------------- members

const memberSelect = `SELECT u.id, u.email, u.name, m.role, u.last_login_at FROM memberships m JOIN users u ON u.id = m.user_id`

func scanMember(row pgx.Row) (model.Member, error) {
	var m model.Member
	var id int64
	var last *time.Time
	if err := row.Scan(&id, &m.Email, &m.Name, &m.Role, &last); err != nil {
		return m, err
	}
	m.UserID = idStr(id)
	if last != nil {
		m.LastLoginAt = ms(*last)
	}
	return m, nil
}

// ListMembers returns the members of an org.
func (d *DB) ListMembers(ctx context.Context, orgID int64) ([]model.Member, error) {
	rows, err := d.Pool.Query(ctx, memberSelect+` WHERE m.org_id = $1 ORDER BY u.email`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) getMember(ctx context.Context, orgID, userID int64) (*model.Member, error) {
	m, err := scanMember(d.Pool.QueryRow(ctx, memberSelect+` WHERE m.org_id = $1 AND m.user_id = $2`, orgID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}

// MemberRole returns the role of userID in orgID (ErrNotFound when not a member).
func (d *DB) MemberRole(ctx context.Context, orgID, userID int64) (string, error) {
	return d.OrgRole(ctx, userID, orgID)
}

// AddMember adds (inviting by email: the user row is created and linked on first login) a member.
func (d *DB) AddMember(ctx context.Context, orgID int64, in model.MemberInput) (*model.Member, error) {
	if !ValidRole(in.Role) {
		return nil, Invalid("role must be owner|admin|member|viewer")
	}
	uid, err := d.UpsertUser(ctx, UserClaims{Email: in.Email, Name: in.Name})
	if err != nil {
		return nil, err
	}
	tag, err := d.Pool.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, orgID, uid, in.Role)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, Conflict("already a member of this org")
	}
	return d.getMember(ctx, orgID, uid)
}

// UpdateMember changes a member's role; the last owner cannot be demoted.
func (d *DB) UpdateMember(ctx context.Context, orgID, userID int64, role string) (*model.Member, error) {
	if !ValidRole(role) {
		return nil, Invalid("role must be owner|admin|member|viewer")
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockOwners(ctx, tx, orgID, userID, role != "owner"); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `UPDATE memberships SET role = $3 WHERE org_id = $1 AND user_id = $2`, orgID, userID, role)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d.getMember(ctx, orgID, userID)
}

// RemoveMember removes a member (and their sessions keep working only for other orgs); not the last owner.
func (d *DB) RemoveMember(ctx context.Context, orgID, userID int64) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockOwners(ctx, tx, orgID, userID, true); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// lockOwners locks the org's owner rows and fails when losing userID as an owner would leave none.
func lockOwners(ctx context.Context, tx pgx.Tx, orgID, userID int64, losing bool) error {
	rows, err := tx.Query(ctx, `SELECT user_id FROM memberships WHERE org_id = $1 AND role = 'owner' FOR UPDATE`, orgID)
	if err != nil {
		return err
	}
	var owners []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if losing && len(owners) == 1 && owners[0] == userID {
		return Conflict("an org needs at least one owner")
	}
	return nil
}

// ---------------------------------------------------------------- projects

// ListProjects returns the projects in the user's orgs (with the user's role).
func (d *DB) ListProjects(ctx context.Context, userID int64) ([]model.Project, error) {
	rows, err := d.Pool.Query(ctx, `SELECT p.id, p.org_id, p.name, p.slug, p.created_at, m.role FROM projects p
	  JOIN memberships m ON m.org_id = p.org_id AND m.user_id = $1 ORDER BY p.org_id, p.id`, userID)
	if err != nil {
		return nil, err
	}
	return scanProjects(rows)
}

// GetProject returns one project (api key callers see only their own).
func (d *DB) GetProject(ctx context.Context, projectID int64, role string) ([]model.Project, error) {
	rows, err := d.Pool.Query(ctx, `SELECT id, org_id, name, slug, created_at, $2::text FROM projects WHERE id = $1`, projectID, role)
	if err != nil {
		return nil, err
	}
	return scanProjects(rows)
}

func scanProjects(rows pgx.Rows) ([]model.Project, error) {
	defer rows.Close()
	out := []model.Project{}
	for rows.Next() {
		var p model.Project
		var id, org int64
		var at time.Time
		if err := rows.Scan(&id, &org, &p.Name, &p.Slug, &at, &p.Role); err != nil {
			return nil, err
		}
		p.ID, p.OrgID, p.CreatedAt = idStr(id), idStr(org), ms(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateProject adds a project to an org.
func (d *DB) CreateProject(ctx context.Context, orgID int64, in model.ProjectInput) (*model.Project, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, Invalid("name is required")
	}
	if in.Slug == "" {
		in.Slug = LowerSlug(in.Name)
	}
	if in.Slug != LowerSlug(in.Slug) || in.Slug == "" {
		return nil, Invalid("slug must be lowercase letters, digits and dashes")
	}
	var p model.Project
	var id int64
	var at time.Time
	err := d.Pool.QueryRow(ctx, `INSERT INTO projects (org_id, name, slug) VALUES ($1, $2, $3) RETURNING id, created_at`, orgID, in.Name, in.Slug).Scan(&id, &at)
	if isUnique(err) {
		return nil, Conflict("a project with this slug already exists in the org")
	}
	if err != nil {
		return nil, err
	}
	p.ID, p.OrgID, p.Name, p.Slug, p.CreatedAt = idStr(id), idStr(orgID), in.Name, in.Slug, ms(at)
	return &p, nil
}

// ---------------------------------------------------------------- api keys (tenant scoped)

const apiKeySelect = `SELECT k.id, k.name, k.kind, k.prefix, k.allowed_origins, k.created_at, k.last_used_at, k.revoked_at,
  coalesce(k.created_by, 0), coalesce(u.email, '') FROM api_keys k LEFT JOIN users u ON u.id = k.created_by`

func scanAPIKey(row pgx.Row) (model.APIKey, error) {
	var k model.APIKey
	var id, by int64
	var created time.Time
	var used, revoked *time.Time
	if err := row.Scan(&id, &k.Name, &k.Kind, &k.Prefix, &k.AllowedOrigins, &created, &used, &revoked, &by, &k.CreatedByEmail); err != nil {
		return k, err
	}
	k.ID, k.CreatedAt, k.CreatedBy = idStr(id), ms(created), idStr(by)
	if used != nil {
		k.LastUsedAt = ms(*used)
	}
	if revoked != nil {
		k.RevokedAt = ms(*revoked)
	}
	if k.AllowedOrigins == nil {
		k.AllowedOrigins = []string{}
	}
	return k, nil
}

// ListAPIKeys returns the project's keys (never the key itself), active first.
func (d *DB) ListAPIKeys(ctx context.Context) ([]model.APIKey, error) {
	rows, err := d.pg.Query(ctx, apiKeySelect+` WHERE {{tenant:k}} ORDER BY k.revoked_at IS NOT NULL, k.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// CreateAPIKey mints a key (sha256 stored, like the seeded keys); the plaintext is only in this response.
func (d *DB) CreateAPIKey(ctx context.Context, in model.APIKeyInput, createdBy int64) (*model.APIKey, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, Invalid("name is required")
	}
	if in.Kind != "ingest" && in.Kind != "api" {
		return nil, Invalid("kind must be ingest|api")
	}
	if in.AllowedOrigins == nil {
		in.AllowedOrigins = []string{}
	}
	if in.Kind == "api" && len(in.AllowedOrigins) > 0 {
		return nil, Invalid("allowedOrigins only applies to ingest keys")
	}
	key := "tt_" + in.Kind + "_" + strings.NewReplacer("-", "", "_", "").Replace(randomToken(30))
	h := sha256.Sum256([]byte(key))
	var by *int64
	if createdBy > 0 {
		by = &createdBy
	}
	var id int64
	if err := d.pg.QueryRow(ctx, `INSERT INTO api_keys (project_id, name, kind, hash, prefix, allowed_origins, created_by)
	  VALUES ({{project}}, $1, $2, $3, $4, $5, $6) RETURNING id`, in.Name, in.Kind, h[:], key[:12], in.AllowedOrigins, by).Scan(&id); err != nil {
		return nil, err
	}
	k, err := scanAPIKey(d.pg.QueryRow(ctx, apiKeySelect+` WHERE {{tenant:k}} AND k.id = $1`, id))
	if err != nil {
		return nil, err
	}
	k.Key = key
	return &k, nil
}

// RevokeAPIKey revokes a key and returns its hash (to drop it from auth caches). Revoked keys → ErrNotFound.
func (d *DB) RevokeAPIKey(ctx context.Context, id string) ([]byte, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	var hash []byte
	err = d.pg.QueryRow(ctx, `UPDATE api_keys SET revoked_at = now() WHERE {{tenant}} AND id = $1 AND revoked_at IS NULL RETURNING hash`, n).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return hash, err
}

// ---------------------------------------------------------------- audit log

// AuditRecord is one audit_log row.
type AuditRecord struct {
	OrgID, ProjectID, UserID, APIKeyID int64
	Action, Target                     string
	Meta                               map[string]any
}

func nullID(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

// WriteAudit appends to the audit log.
func (d *DB) WriteAudit(ctx context.Context, a AuditRecord) error {
	meta, _ := json.Marshal(a.Meta)
	if a.Meta == nil {
		meta = []byte("{}")
	}
	_, err := d.Pool.Exec(ctx, `INSERT INTO audit_log (org_id, project_id, user_id, api_key_id, action, target, meta) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		a.OrgID, nullID(a.ProjectID), nullID(a.UserID), nullID(a.APIKeyID), a.Action, a.Target, meta)
	return err
}

// ListAudit returns the org's newest audit entries.
func (d *DB) ListAudit(ctx context.Context, orgID int64, limit int) ([]model.AuditEntry, error) {
	rows, err := d.Pool.Query(ctx, `SELECT a.id, a.at, a.action, a.target, coalesce(a.project_id, 0), coalesce(a.user_id, 0), coalesce(u.email, ''),
	    coalesce(a.api_key_id, 0), coalesce(k.name, ''), a.meta
	  FROM audit_log a LEFT JOIN users u ON u.id = a.user_id LEFT JOIN api_keys k ON k.id = a.api_key_id
	  WHERE a.org_id = $1 ORDER BY a.at DESC, a.id DESC LIMIT $2`, orgID, clampLimit(limit, 100, 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AuditEntry{}
	for rows.Next() {
		var e model.AuditEntry
		var id, pid, uid, kid int64
		var at time.Time
		var meta []byte
		if err := rows.Scan(&id, &at, &e.Action, &e.Target, &pid, &uid, &e.UserEmail, &kid, &e.APIKeyName, &meta); err != nil {
			return nil, err
		}
		e.ID, e.At, e.ProjectID, e.UserID, e.APIKeyID, e.Meta = idStr(id), ms(at), idStr(pid), idStr(uid), idStr(kid), json.RawMessage(meta)
		out = append(out, e)
	}
	return out, rows.Err()
}
