package notify

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSlackStore implements SlackStore on Postgres: the org's Slack installation is the newest
// integrations(provider='slack') row of any project of the org (contracts §17); its credentials_ref
// (secret:<id>, sealed with the installing project as AAD) holds the bot token. Message refs live in
// notification_messages (009_slack_github.sql).
type PGSlackStore struct {
	DB      *pgxpool.Pool
	Secrets *SecretResolver
}

// BotToken implements SlackStore.
func (s *PGSlackStore) BotToken(ctx context.Context, projectID int64) (string, error) {
	var instProject int64
	var ref *string
	err := s.DB.QueryRow(ctx, `SELECT i.project_id, i.credentials_ref FROM integrations i JOIN projects p ON p.id = i.project_id
		WHERE i.provider = 'slack' AND i.status <> 'disabled' AND p.org_id = (SELECT org_id FROM projects WHERE id = $1)
		ORDER BY i.id DESC LIMIT 1`, projectID).Scan(&instProject, &ref)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ref == nil || *ref == "")) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(*ref, "secret:"):
		if s.Secrets == nil {
			return "", permanent("slack bot token %s: no secrets key configured", *ref)
		}
		return s.Secrets.resolve(ctx, instProject, *ref)
	case strings.HasPrefix(*ref, "env:"):
		return os.Getenv(strings.TrimPrefix(*ref, "env:")), nil
	}
	return "", permanent("slack installation: unsupported credentials_ref %q (want secret:<id> or env:NAME)", redactRef(*ref))
}

func redactRef(s string) string {
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i+1] + "…"
	}
	return "…"
}

// MessageRef implements SlackStore.
func (s *PGSlackStore) MessageRef(ctx context.Context, incidentID int64, target string) (ts, channel string, ok bool, err error) {
	err = s.DB.QueryRow(ctx, `SELECT external_id, coalesce(external_channel, '') FROM notification_messages
		WHERE incident_id = $1 AND channel = $2 AND provider = 'slack' ORDER BY id LIMIT 1`, incidentID, target).Scan(&ts, &channel)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return ts, channel, err == nil, err
}

// SaveMessageRef implements SlackStore (first message wins).
func (s *PGSlackStore) SaveMessageRef(ctx context.Context, incidentID int64, target, ts, channel string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO notification_messages (incident_id, channel, provider, external_id, external_channel)
		SELECT $1, $2, 'slack', $3, $4 WHERE NOT EXISTS (
			SELECT 1 FROM notification_messages WHERE incident_id = $1 AND channel = $2 AND provider = 'slack')`, incidentID, target, ts, channel)
	return err
}
