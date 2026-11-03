package store

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// DeployRow is one validated deploy marker to write.
type DeployRow struct {
	Service, Env, Version, URL, Message, Author, ExternalID string
	Ts                                                      time.Time
}

// DeployRows validates a POST /deploys body and expands it to one row per service. Reposting the same
// (service, env, version) is idempotent: external_id = "<service>@<env>@<version>".
func DeployRows(in model.DeployInput, now time.Time) ([]DeployRow, error) {
	var svcs []string
	seen := map[string]bool{}
	for _, s := range append([]string{in.Service}, in.Services...) {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			svcs = append(svcs, s)
		}
	}
	if len(svcs) == 0 {
		return nil, Invalid("service or services is required")
	}
	if len(svcs) > 100 {
		return nil, Invalid("at most 100 services per deploy")
	}
	version := strings.TrimSpace(in.Version)
	if version == "" {
		version = strings.TrimSpace(in.CommitSHA)
	}
	if version == "" {
		return nil, Invalid("version or commitSha is required")
	}
	if len(version) > 200 {
		return nil, Invalid("version is too long")
	}
	env := strings.TrimSpace(in.Environment)
	if env == "" {
		env = "production"
	}
	ts := now
	switch {
	case in.Time != "":
		t, err := time.Parse(time.RFC3339, in.Time)
		if err != nil {
			return nil, Invalid("time must be RFC 3339")
		}
		ts = t
	case in.Ts > 0:
		ts = time.UnixMilli(in.Ts)
	}
	if ts.After(now.Add(time.Hour)) {
		return nil, Invalid("time is in the future")
	}
	if in.URL != "" {
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, Invalid("url must be an http(s) URL")
		}
	}
	msg := strings.TrimSpace(in.Message)
	if sha := strings.TrimSpace(in.CommitSHA); sha != "" && sha != version && !strings.Contains(msg, sha) {
		if msg == "" {
			msg = sha
		} else {
			msg = msg + " (" + sha + ")"
		}
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	out := make([]DeployRow, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, DeployRow{Service: s, Env: env, Version: version, URL: in.URL, Message: msg, Author: strings.TrimSpace(in.Author),
			ExternalID: s + "@" + env + "@" + version, Ts: ts.UTC()})
	}
	return out, nil
}

// CreateDeploys writes CI deploy markers (deploy_markers, source "ci") and returns them.
func (d *DB) CreateDeploys(ctx context.Context, now time.Time, in model.DeployInput) ([]model.DeployMarker, error) {
	rows, err := DeployRows(in, now)
	if err != nil {
		return nil, err
	}
	out := make([]model.DeployMarker, 0, len(rows))
	for _, r := range rows {
		if _, err := d.pg.Exec(ctx, `INSERT INTO deploy_markers (project_id, service, env, version, source, url, commit_message, author, state, ts, external_id)
		  VALUES ({{project}}, $1, $2, $3, 'ci', nullif($4, ''), nullif($5, ''), nullif($6, ''), 'ready', $7, $8)
		  ON CONFLICT (project_id, source, external_id) DO UPDATE SET url = excluded.url, commit_message = excluded.commit_message,
		    author = excluded.author, ts = excluded.ts`,
			r.Service, r.Env, r.Version, r.URL, r.Message, r.Author, r.Ts, r.ExternalID); err != nil {
			return nil, err
		}
		out = append(out, model.DeployMarker{Service: r.Service, Env: r.Env, Version: r.Version, Ts: ms(r.Ts), Source: "ci", URL: r.URL,
			CommitMessage: r.Message, Author: r.Author, State: "ready"})
	}
	return out, nil
}
