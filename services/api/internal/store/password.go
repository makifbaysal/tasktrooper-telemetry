package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Password login (ADR-035, contracts §15). Like the other login lookups these run on the raw pool: they are how the
// user is found, every statement is keyed by email / user id.

// PasswordHash returns the user and bcrypt hash for email. Unknown email → ErrNotFound; a user without a password
// → their id and "".
func (d *DB) PasswordHash(ctx context.Context, email string) (int64, string, error) {
	var id int64
	var hash *string
	err := d.Pool.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE email = $1`, strings.ToLower(strings.TrimSpace(email))).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil || hash == nil {
		return id, "", err
	}
	return id, *hash, nil
}

// SetPasswordInput is `api set-password`.
type SetPasswordInput struct {
	Email string
	// Name is set on the user when not empty (a new user without one is named after the email's local part).
	Name string
	// Hash replaces the user's bcrypt hash; empty = keep the current one (it already matches the password).
	Hash string
	// Org is an org id or an exact org name; ProjectID picks the org of that project instead. Neither = no membership change.
	Org       string
	ProjectID int64
	// Role of the membership. Empty: a new membership gets viewer, an existing one is left as it is. Set: the existing
	// role is changed to it (the last owner of an org is never demoted).
	Role string
}

// SetPasswordResult says what changed.
type SetPasswordResult struct {
	UserID          int64
	UserCreated     bool
	PasswordChanged bool
	SessionsRevoked int64
	OrgID           int64
	Role            string
	MemberAdded     bool
	RoleChanged     bool
}

// SetPassword creates the user when missing, stores the hash (revoking the user's sessions when it changes) and
// ensures the membership. Idempotent: a second run with the same password and role changes nothing.
func (d *DB) SetPassword(ctx context.Context, in SetPasswordInput) (*SetPasswordResult, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, Invalid("a valid email is required")
	}
	if in.Role != "" && !ValidRole(in.Role) {
		return nil, Invalid("role must be owner|admin|member|viewer")
	}
	name := strings.TrimSpace(in.Name)
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	res := &SetPasswordResult{}

	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE email = $1 FOR UPDATE`, email).Scan(&res.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		n := name
		if n == "" {
			n, _, _ = strings.Cut(email, "@")
		}
		err = tx.QueryRow(ctx, `INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id`, email, n).Scan(&res.UserID)
		res.UserCreated = true
	} else if err == nil && name != "" {
		_, err = tx.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1 AND name IS DISTINCT FROM $2`, res.UserID, name)
	}
	if err != nil {
		return nil, fmt.Errorf("user: %w", err)
	}
	if in.Hash != "" {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, password_updated_at = now() WHERE id = $1`, res.UserID, in.Hash); err != nil {
			return nil, err
		}
		res.PasswordChanged = true
		// a new password signs out everyone who knew the old one (shared demo account rotation)
		tag, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, res.UserID)
		if err != nil {
			return nil, err
		}
		res.SessionsRevoked = tag.RowsAffected()
	}

	if res.OrgID, err = resolveOrg(ctx, tx, in.Org, in.ProjectID); err != nil || res.OrgID == 0 {
		if err != nil {
			return nil, err
		}
		return res, tx.Commit(ctx)
	}
	var cur string
	err = tx.QueryRow(ctx, `SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2 FOR UPDATE`, res.OrgID, res.UserID).Scan(&cur)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		res.Role = in.Role
		if res.Role == "" {
			res.Role = "viewer"
		}
		_, err = tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`, res.OrgID, res.UserID, res.Role)
		res.MemberAdded = true
	case err != nil:
	case in.Role == "" || in.Role == cur:
		res.Role = cur
	default:
		if cur == "owner" {
			var owners int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner'`, res.OrgID).Scan(&owners); err == nil && owners <= 1 {
				return nil, Conflict("refusing to demote the last owner of org " + strconv.FormatInt(res.OrgID, 10))
			}
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE memberships SET role = $3 WHERE org_id = $1 AND user_id = $2`, res.OrgID, res.UserID, in.Role)
			res.Role, res.RoleChanged = in.Role, true
		}
	}
	if err != nil {
		return nil, fmt.Errorf("membership: %w", err)
	}
	return res, tx.Commit(ctx)
}

// resolveOrg finds the org of projectID, else org (numeric id, then exact name; a name shared by several orgs is an
// error). Both empty → 0.
func resolveOrg(ctx context.Context, tx pgx.Tx, org string, projectID int64) (int64, error) {
	var id int64
	if projectID > 0 {
		err := tx.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, projectID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, Invalid("project " + strconv.FormatInt(projectID, 10) + " not found")
		}
		return id, err
	}
	org = strings.TrimSpace(org)
	if org == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(org, 10, 64); err == nil {
		err = tx.QueryRow(ctx, `SELECT id FROM orgs WHERE id = $1`, n).Scan(&id)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return id, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id FROM orgs WHERE name = $1 ORDER BY id LIMIT 2`, org)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch len(ids) {
	case 0:
		return 0, Invalid("org " + strconv.Quote(org) + " not found (orgs are created in the dashboard or by `api bootstrap`)")
	case 1:
		return ids[0], nil
	default:
		return 0, Invalid("several orgs are named " + strconv.Quote(org) + "; pass the org id")
	}
}
