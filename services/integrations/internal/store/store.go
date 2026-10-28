// Package store is trooper-cloud's Postgres access: integrations, secrets, discovered databases /
// broker_clusters and deploy markers (contracts §10, deploy/postgres/004_integrations.sql).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

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

// Integrations returns every integration whose status is not 'disabled'.
func (s *Store) Integrations(ctx context.Context) ([]model.Integration, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, project_id, provider, name, config, coalesce(credentials_ref, ''), status, last_sync_at
		FROM integrations WHERE status <> 'disabled' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.Integration, error) {
		var i model.Integration
		var cfg []byte
		err := r.Scan(&i.ID, &i.ProjectID, &i.Provider, &i.Name, &cfg, &i.CredentialsRef, &i.Status, &i.LastSyncAt)
		i.Config = cfg
		return i, err
	})
}

// LoadSecret implements secrets.Store.
func (s *Store) LoadSecret(ctx context.Context, projectID, id int64) (nonce, ciphertext []byte, err error) {
	err = s.DB.QueryRow(ctx, `SELECT nonce, ciphertext FROM secrets WHERE id = $1 AND project_id = $2`, id, projectID).Scan(&nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, errors.New("secret not found")
	}
	return nonce, ciphertext, err
}

// SetStatus records the outcome of a sync. resources < 0 keeps the stored count; ok updates last_sync_at.
func (s *Store) SetStatus(ctx context.Context, id int64, status, errMsg string, resources int) error {
	var e any
	if errMsg != "" {
		if len(errMsg) > 1000 {
			errMsg = errMsg[:1000]
		}
		e = errMsg
	}
	_, err := s.DB.Exec(ctx, `UPDATE integrations SET status = $2, error = $3,
		last_sync_at = CASE WHEN $2 = 'ok' THEN now() ELSE last_sync_at END,
		resources = CASE WHEN $4::int >= 0 THEN $4::int ELSE resources END
		WHERE id = $1 AND status <> 'disabled'`, id, status, e, resources)
	return err
}

func metaJSON(m map[string]any) []byte {
	if m == nil {
		m = map[string]any{}
	}
	b, _ := json.Marshal(m)
	return b
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SyncDatabases upserts discovered databases for an integration and marks the integration's rows that
// were not discovered as meta.missing=true. Rows with integration_id NULL (added by hand) or owned by
// another integration are never touched. Returns the slugs written.
func (s *Store) SyncDatabases(ctx context.Context, in model.Integration, dbs []model.Database) ([]string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	slugs := make([]string, 0, len(dbs))
	for _, d := range dbs {
		slug := model.Slug(in.Provider, d.Name)
		env := d.Env
		if env == "" {
			env = "production"
		}
		tag, err := tx.Exec(ctx, `INSERT INTO databases (project_id, slug, name, engine, host, port, collector, cloud_resource_id, env,
				integration_id, source, version, zone, instance_class, disk_gb, meta)
			VALUES ($1, $2, $3, $4, $5, $6, 'cloud', $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (project_id, slug) DO UPDATE SET name = EXCLUDED.name, engine = EXCLUDED.engine, host = EXCLUDED.host,
				port = EXCLUDED.port, cloud_resource_id = EXCLUDED.cloud_resource_id, source = EXCLUDED.source,
				version = EXCLUDED.version, zone = EXCLUDED.zone, instance_class = EXCLUDED.instance_class,
				disk_gb = EXCLUDED.disk_gb, meta = EXCLUDED.meta
			WHERE databases.integration_id = EXCLUDED.integration_id`,
			in.ProjectID, slug, d.Name, d.Engine, d.Host, d.Port, nz(d.CloudResourceID), env, in.ID,
			nz(d.Source), nz(d.Version), nz(d.Zone), nz(d.InstanceClass), d.DiskGB, metaJSON(d.Meta))
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() > 0 {
			slugs = append(slugs, slug)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE databases SET meta = meta || '{"missing": true}'
		WHERE project_id = $1 AND integration_id = $2 AND NOT (slug = ANY($3)) AND NOT coalesce((meta->>'missing')::boolean, false)`,
		in.ProjectID, in.ID, allSlugs(in.Provider, dbs)); err != nil {
		return nil, err
	}
	return slugs, tx.Commit(ctx)
}

func allSlugs(provider string, dbs []model.Database) []string {
	out := make([]string, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, model.Slug(provider, d.Name))
	}
	return out
}

// SyncBrokers is SyncDatabases for broker_clusters.
func (s *Store) SyncBrokers(ctx context.Context, in model.Integration, brokers []model.Broker) ([]string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	slugs := make([]string, 0, len(brokers))
	all := make([]string, 0, len(brokers))
	for _, b := range brokers {
		slug := model.Slug(in.Provider, b.Name)
		all = append(all, slug)
		tag, err := tx.Exec(ctx, `INSERT INTO broker_clusters (project_id, slug, name, kind, endpoint, collector, integration_id, source, version, meta)
			VALUES ($1, $2, $3, $4, $5, 'cloud', $6, $7, $8, $9)
			ON CONFLICT (project_id, slug) DO UPDATE SET name = EXCLUDED.name, kind = EXCLUDED.kind, endpoint = EXCLUDED.endpoint,
				source = EXCLUDED.source, version = EXCLUDED.version, meta = EXCLUDED.meta
			WHERE broker_clusters.integration_id = EXCLUDED.integration_id`,
			in.ProjectID, slug, b.Name, b.Kind, b.Endpoint, in.ID, nz(b.Source), nz(b.Version), metaJSON(b.Meta))
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() > 0 {
			slugs = append(slugs, slug)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE broker_clusters SET meta = meta || '{"missing": true}'
		WHERE project_id = $1 AND integration_id = $2 AND NOT (slug = ANY($3)) AND NOT coalesce((meta->>'missing')::boolean, false)`,
		in.ProjectID, in.ID, all); err != nil {
		return nil, err
	}
	return slugs, tx.Commit(ctx)
}

// SyncFunctions upserts discovered serverless functions (deploy/postgres/011_serverless.sql) and sets
// missing=true on the integration's rows that were not discovered (cleared when they reappear). Rows owned by
// another integration or added by hand (integration_id NULL) are never touched. Returns the slugs written.
func (s *Store) SyncFunctions(ctx context.Context, in model.Integration, fns []model.Function) ([]string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	slugs := make([]string, 0, len(fns))
	all := make([]string, 0, len(fns))
	for _, f := range fns {
		all = append(all, f.Slug)
		triggers := f.Triggers
		if triggers == nil {
			triggers = []string{}
		}
		service := f.Service
		if service == "" {
			service = f.Name
		}
		tag, err := tx.Exec(ctx, `INSERT INTO serverless_functions (project_id, integration_id, provider, slug, name, app, region, runtime,
				memory_mb, timeout_s, arch, triggers, service, url, meta, last_seen_at, missing)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now(), false)
			ON CONFLICT (project_id, slug) DO UPDATE SET name = EXCLUDED.name, app = EXCLUDED.app, region = EXCLUDED.region,
				runtime = EXCLUDED.runtime, memory_mb = EXCLUDED.memory_mb, timeout_s = EXCLUDED.timeout_s, arch = EXCLUDED.arch,
				triggers = EXCLUDED.triggers, service = EXCLUDED.service, url = EXCLUDED.url, meta = EXCLUDED.meta,
				last_seen_at = now(), missing = false
			WHERE serverless_functions.integration_id = EXCLUDED.integration_id`,
			in.ProjectID, in.ID, in.Provider, f.Slug, f.Name, nz(f.App), f.Region, f.Runtime, f.MemoryMB, f.TimeoutS, f.Arch,
			triggers, service, nz(f.URL), metaJSON(f.Meta))
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() > 0 {
			slugs = append(slugs, f.Slug)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE serverless_functions SET missing = true
		WHERE project_id = $1 AND integration_id = $2 AND NOT (slug = ANY($3)) AND NOT missing`,
		in.ProjectID, in.ID, all); err != nil {
		return nil, err
	}
	return slugs, tx.Commit(ctx)
}

// PatchDatabaseMeta merges patch into databases.meta for the integration's row with slug.
func (s *Store) PatchDatabaseMeta(ctx context.Context, in model.Integration, patches map[string]map[string]any) error {
	for slug, p := range patches {
		if _, err := s.DB.Exec(ctx, `UPDATE databases SET meta = meta || $4 WHERE project_id = $1 AND integration_id = $2 AND slug = $3`,
			in.ProjectID, in.ID, slug, metaJSON(p)); err != nil {
			return err
		}
	}
	return nil
}

// UpsertDeploys writes deploy markers (unique on project, source, external_id).
func (s *Store) UpsertDeploys(ctx context.Context, in model.Integration, ds []model.DeployMarker) error {
	if len(ds) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, d := range ds {
		ts := d.TS
		if ts.IsZero() {
			ts = time.Now()
		}
		b.Queue(`INSERT INTO deploy_markers (project_id, service, env, version, source, integration_id, url, commit_message, author, state, ts, external_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (project_id, source, external_id) DO UPDATE SET service = EXCLUDED.service, env = EXCLUDED.env,
				version = EXCLUDED.version, url = EXCLUDED.url, commit_message = EXCLUDED.commit_message, author = EXCLUDED.author,
				state = EXCLUDED.state, ts = EXCLUDED.ts, integration_id = EXCLUDED.integration_id`,
			in.ProjectID, d.Service, d.Env, d.Version, d.Source, in.ID, nz(d.URL), nz(d.CommitMessage), nz(d.Author), d.State, ts, d.ExternalID)
	}
	return s.DB.SendBatch(ctx, b).Close()
}
