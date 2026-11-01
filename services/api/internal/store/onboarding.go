package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// Onboarding without seed data (ADR-028, contracts §11): POST /orgs creates an org, its first project and an ingest
// key in one transaction; `api bootstrap` does the same idempotently for operators / CI / the self project.
// These statements create the tenant, so (like the other identity statements) they run on the raw pool, keyed
// by the authenticated user and the ids created in the same transaction.

const (
	// DefaultMaxOrgsPerUser caps how many orgs one user may own through POST /orgs (abuse guard).
	DefaultMaxOrgsPerUser = 20
	// DefaultPublicIngestURL is the ingest gateway as reachable by SDKs (TT_PUBLIC_INGEST_URL).
	DefaultPublicIngestURL = "http://localhost:4318"
	// BootstrapKeyName names the ingest key `api bootstrap` manages.
	BootstrapKeyName = "bootstrap"

	maxOrgName     = 100
	maxProjectName = 100
	maxSlug        = 63
)

// CleanName trims s and checks it is 1..maxLen characters without control characters.
func CleanName(field, s string, maxLen int) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", Invalid(field + " is required")
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen {
		return "", Invalid(fmt.Sprintf("%s must be at most %d characters", field, maxLen))
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", Invalid(field + " must not contain control characters")
		}
	}
	return s, nil
}

// ProjectSlug returns the slug for a project: the given one (validated) or one derived from the name
// ("project" when the name has no ASCII letters or digits).
func ProjectSlug(name, slug string) (string, error) {
	if slug != "" {
		if slug != LowerSlug(slug) || len(slug) > maxSlug {
			return "", Invalid(fmt.Sprintf("slug must be lowercase letters, digits and dashes (at most %d)", maxSlug))
		}
		return slug, nil
	}
	s := LowerSlug(name)
	if len(s) > maxSlug {
		s = strings.TrimRight(s[:maxSlug], "-")
	}
	if s == "" {
		s = "project"
	}
	return s, nil
}

// mintKey returns a new api key of kind, its sha256 and display prefix (same format as POST /api-keys).
func mintKey(kind string) (key string, hash []byte, prefix string) {
	key = "tt_" + kind + "_" + strings.NewReplacer("-", "", "_", "").Replace(randomToken(30))
	h := sha256.Sum256([]byte(key))
	return key, h[:], key[:12]
}

// DSN is the SDK DSN of an ingest key: <scheme>://<key>@<host>[/<base path>]/<project id>.
func DSN(base, key string, projectID int64) (string, error) {
	if base == "" {
		base = DefaultPublicIngestURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("public ingest url %q must be an absolute http(s) URL", base)
	}
	u.User = url.User(key)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strconv.FormatInt(projectID, 10)
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// KeyFromDSN returns the ingest key of a DSN, or s itself when it is a bare key.
func KeyFromDSN(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.User != nil {
		return u.User.Username()
	}
	return s
}

// CreateOrg creates an org owned by userID with its first project and one ingest key (POST /orgs). maxOrgs caps
// the orgs the user owns (<= 0: DefaultMaxOrgsPerUser).
func (d *DB) CreateOrg(ctx context.Context, userID int64, in model.OrgInput, maxOrgs int, ingestURL string) (*model.OrgCreated, error) {
	orgName, err := CleanName("name", in.Name, maxOrgName)
	if err != nil {
		return nil, err
	}
	projName, err := CleanName("project.name", in.Project.Name, maxProjectName)
	if err != nil {
		return nil, err
	}
	slug, err := ProjectSlug(projName, in.Project.Slug)
	if err != nil {
		return nil, err
	}
	if maxOrgs <= 0 {
		maxOrgs = DefaultMaxOrgsPerUser
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Lock the user row: concurrent POST /orgs of one user cannot both pass the limit.
	var owned int
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM memberships WHERE user_id = u.id AND role = 'owner')
	  FROM users u WHERE u.id = $1 FOR UPDATE`, userID).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if owned >= maxOrgs {
		return nil, Conflict(fmt.Sprintf("org limit reached: a user can own at most %d orgs", maxOrgs))
	}
	var orgID, projID, keyID int64
	if err := tx.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ($1) RETURNING id`, orgName).Scan(&orgID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, orgID, userID); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO projects (org_id, name, slug) VALUES ($1, $2, $3) RETURNING id`, orgID, projName, slug).Scan(&projID); err != nil {
		return nil, err
	}
	key, hash, prefix := mintKey("ingest")
	if err := tx.QueryRow(ctx, `INSERT INTO api_keys (project_id, name, kind, hash, prefix, created_by) VALUES ($1, 'default', 'ingest', $2, $3, $4) RETURNING id`,
		projID, hash, prefix, userID).Scan(&keyID); err != nil {
		return nil, err
	}
	dsn, err := DSN(ingestURL, key, projID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &model.OrgCreated{
		Org:       model.CreatedOrg{ID: idStr(orgID), Name: orgName, Role: "owner"},
		Project:   model.CreatedProject{ID: idStr(projID), Name: projName, Slug: slug},
		IngestKey: model.CreatedKey{ID: idStr(keyID), Prefix: prefix, Key: key},
		DSN:       dsn,
	}, nil
}

// BootstrapInput is `api bootstrap`.
type BootstrapInput struct {
	Org, Project, OwnerEmail string
	// KnownKey is the plaintext ingest key the caller already holds (from its out file); "" = none.
	KnownKey string
	// CanStore: the caller persists a newly minted key (an out file was given). Without it an existing
	// bootstrap key is kept even though its plaintext cannot be shown again.
	CanStore bool
}

// BootstrapResult says what `api bootstrap` found or created.
type BootstrapResult struct {
	UserID, OrgID, ProjectID, KeyID int64
	ProjectSlug, KeyPrefix          string
	// Key is the plaintext ingest key: KnownKey when reused, the new key when minted, "" when an existing key
	// was kept without its plaintext.
	Key                                                string
	UserCreated, OrgCreated, ProjectCreated, KeyMinted bool
	// RevokedHashes are the bootstrap keys revoked by a rotation (to drop from auth caches).
	RevokedHashes [][]byte
}

// Bootstrap idempotently finds or creates the owner (by email), the org of that name owned by them, the project
// (by slug or name) and its `bootstrap` ingest key. Key rules: KnownKey matching an active bootstrap key → reuse;
// no KnownKey match and CanStore → revoke active bootstrap keys and mint one; !CanStore with an active key → keep it.
func (d *DB) Bootstrap(ctx context.Context, in BootstrapInput) (*BootstrapResult, error) {
	orgName, err := CleanName("org", in.Org, maxOrgName)
	if err != nil {
		return nil, err
	}
	projName, err := CleanName("project", in.Project, maxProjectName)
	if err != nil {
		return nil, err
	}
	slug, err := ProjectSlug(projName, "")
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(in.OwnerEmail))
	if email == "" || !strings.Contains(email, "@") {
		return nil, Invalid("a valid owner email is required")
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Serializes concurrent bootstraps (e.g. two `make self` runs) so neither creates a duplicate org.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('tt-api-bootstrap'))`); err != nil {
		return nil, err
	}
	res := &BootstrapResult{ProjectSlug: slug}

	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&res.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		name, _, _ := strings.Cut(email, "@")
		err = tx.QueryRow(ctx, `INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id`, email, name).Scan(&res.UserID)
		res.UserCreated = true
	}
	if err != nil {
		return nil, fmt.Errorf("owner: %w", err)
	}

	err = tx.QueryRow(ctx, `SELECT o.id FROM orgs o JOIN memberships m ON m.org_id = o.id AND m.user_id = $1 AND m.role = 'owner'
	  WHERE o.name = $2 ORDER BY o.id LIMIT 1`, res.UserID, orgName).Scan(&res.OrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ($1) RETURNING id`, orgName).Scan(&res.OrgID); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, res.OrgID, res.UserID)
		}
		res.OrgCreated = true
	}
	if err != nil {
		return nil, fmt.Errorf("org: %w", err)
	}

	err = tx.QueryRow(ctx, `SELECT id, slug FROM projects WHERE org_id = $1 AND (slug = $2 OR name = $3) ORDER BY slug = $2 DESC, id LIMIT 1`,
		res.OrgID, slug, projName).Scan(&res.ProjectID, &res.ProjectSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO projects (org_id, name, slug) VALUES ($1, $2, $3) RETURNING id`, res.OrgID, projName, slug).Scan(&res.ProjectID)
		res.ProjectCreated = true
	}
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}

	type keyRow struct {
		id     int64
		hash   []byte
		prefix string
	}
	rows, err := tx.Query(ctx, `SELECT id, hash, prefix FROM api_keys WHERE project_id = $1 AND kind = 'ingest' AND name = $2
	  AND revoked_at IS NULL ORDER BY id DESC FOR UPDATE`, res.ProjectID, BootstrapKeyName)
	if err != nil {
		return nil, err
	}
	var active []keyRow
	for rows.Next() {
		var k keyRow
		if err := rows.Scan(&k.id, &k.hash, &k.prefix); err != nil {
			rows.Close()
			return nil, err
		}
		active = append(active, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if known := strings.TrimSpace(in.KnownKey); known != "" {
		h := sha256.Sum256([]byte(known))
		for _, k := range active {
			if string(k.hash) == string(h[:]) {
				res.KeyID, res.KeyPrefix, res.Key = k.id, k.prefix, known
				return res, tx.Commit(ctx)
			}
		}
	}
	if len(active) > 0 && !in.CanStore {
		res.KeyID, res.KeyPrefix = active[0].id, active[0].prefix
		return res, tx.Commit(ctx)
	}
	for _, k := range active {
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, k.id); err != nil {
			return nil, err
		}
		res.RevokedHashes = append(res.RevokedHashes, k.hash)
	}
	key, hash, prefix := mintKey("ingest")
	if err := tx.QueryRow(ctx, `INSERT INTO api_keys (project_id, name, kind, hash, prefix, created_by) VALUES ($1, $2, 'ingest', $3, $4, $5) RETURNING id`,
		res.ProjectID, BootstrapKeyName, hash, prefix, res.UserID).Scan(&res.KeyID); err != nil {
		return nil, err
	}
	res.Key, res.KeyPrefix, res.KeyMinted = key, prefix, true
	return res, tx.Commit(ctx)
}
