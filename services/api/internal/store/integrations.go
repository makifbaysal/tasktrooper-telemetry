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
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/tq"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
)

// Providers are the integrations.provider values (ADR-012).
var Providers = map[string]bool{"aws": true, "gcp": true, "vercel": true, "neon": true}

// secretConfigKeys are never returned in integration config (they belong in the secrets table).
var secretConfigKeys = map[string]bool{
	"secret": true, "password": true, "privatekey": true, "secretaccesskey": true, "apikey": true, "accesstoken": true,
	"token": true, "drainsecret": true, "serviceaccountjson": true, "sessiontoken": true,
}

// ---------------------------------------------------------------- secrets

func (d *DB) key() ([]byte, error) {
	if d.SecretsKey != nil {
		return d.SecretsKey, nil
	}
	k, err := secrets.Key()
	if err != nil {
		return nil, err
	}
	d.SecretsKey = k
	return k, nil
}

// sealSecret stores plaintext encrypted (AAD = project id) and returns "secret:<id>".
func (d *DB) sealSecret(ctx context.Context, name string, plaintext []byte) (string, error) {
	t, err := tqTenant(ctx)
	if err != nil {
		return "", err
	}
	k, err := d.key()
	if err != nil {
		return "", err
	}
	nonce, ct, err := secrets.Seal(k, int64(t), plaintext)
	if err != nil {
		return "", err
	}
	var id int64
	if err := d.pg.QueryRow(ctx, `INSERT INTO secrets (project_id, name, nonce, ciphertext) VALUES ({{project}}, $1, $2, $3) RETURNING id`,
		name, nonce, ct).Scan(&id); err != nil {
		return "", err
	}
	return "secret:" + strconv.FormatInt(id, 10), nil
}

// rotateSecret re-seals an existing secret row in place (the reference stays the same).
func (d *DB) rotateSecret(ctx context.Context, ref string, plaintext []byte) (bool, error) {
	id, ok := secretID(ref)
	if !ok {
		return false, nil
	}
	t, err := tqTenant(ctx)
	if err != nil {
		return false, err
	}
	k, err := d.key()
	if err != nil {
		return false, err
	}
	nonce, ct, err := secrets.Seal(k, int64(t), plaintext)
	if err != nil {
		return false, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE secrets SET nonce = $2, ciphertext = $3, rotated_at = now() WHERE {{tenant}} AND id = $1`, id, nonce, ct)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (d *DB) deleteSecret(ctx context.Context, ref string) error {
	id, ok := secretID(ref)
	if !ok {
		return nil
	}
	_, err := d.pg.Exec(ctx, `DELETE FROM secrets WHERE {{tenant}} AND id = $1`, id)
	return err
}

// LoadSecret implements secrets.Store (tenant from ctx; projectID must match it).
func (d *DB) LoadSecret(ctx context.Context, projectID, id int64) (nonce, ciphertext []byte, err error) {
	err = d.pg.QueryRow(ctx, `SELECT nonce, ciphertext FROM secrets WHERE {{tenant}} AND id = $1 AND project_id = $2`, id, projectID).Scan(&nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// tqTenant returns the project id of the request tenant (AAD of sealed secrets).
func tqTenant(ctx context.Context) (uint32, error) {
	t, err := tq.MustTenant(ctx)
	if err != nil {
		return 0, err
	}
	return t.ProjectID, nil
}

func secretID(ref string) (int64, bool) {
	s, ok := strings.CutPrefix(ref, "secret:")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// ---------------------------------------------------------------- integrations

const integrationCols = `id, provider, name, status, coalesce(error, ''), last_sync_at, config, coalesce(credentials_ref, ''), resources, created_at`

type integrationRow struct {
	id    int64
	m     model.Integration
	creds string
	cfg   map[string]any
}

func scanIntegration(row pgx.Row) (integrationRow, error) {
	var r integrationRow
	var last *time.Time
	var created time.Time
	var cfg []byte
	if err := row.Scan(&r.id, &r.m.Provider, &r.m.Name, &r.m.Status, &r.m.Error, &last, &cfg, &r.creds, &r.m.ResourceCount, &created); err != nil {
		return r, err
	}
	r.m.ID = strconv.FormatInt(r.id, 10)
	if last != nil {
		r.m.LastSyncAt = ms(*last)
	}
	r.m.CreatedAt = ms(created)
	r.m.HasSecret = r.creds != ""
	_ = json.Unmarshal(cfg, &r.cfg)
	if r.cfg == nil {
		r.cfg = map[string]any{}
	}
	if v, ok := r.cfg["syncRequestedAt"].(float64); ok {
		r.m.SyncRequestedAt = int64(v)
	}
	r.m.Config = redactConfig(r.cfg)
	r.m.Resources = []model.DiscoveredResource{}
	return r, nil
}

func redactConfig(cfg map[string]any) json.RawMessage {
	out := map[string]any{}
	for k, v := range cfg {
		if secretConfigKeys[strings.ToLower(k)] {
			continue
		}
		out[k] = v
	}
	b, _ := json.Marshal(out)
	return b
}

// discovered lists the rows an integration created: databases, broker clusters and deploy projects.
func (d *DB) discovered(ctx context.Context, ids []int64) (map[int64][]model.DiscoveredResource, error) {
	out := map[int64][]model.DiscoveredResource{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pg.Query(ctx, `SELECT integration_id, 'database', slug, name, concat_ws(' · ', nullif(source, ''), nullif(instance_class, ''), nullif(version, '')),
	    coalesce((meta->>'missing')::boolean, false)
	  FROM databases WHERE {{tenant}} AND integration_id = ANY($1)
	  UNION ALL
	  SELECT integration_id, CASE kind WHEN 'sqs' THEN 'queue' WHEN 'pubsub' THEN 'subscription' ELSE 'cluster' END, slug, name,
	    concat_ws(' · ', nullif(source, ''), nullif(version, '')), coalesce((meta->>'missing')::boolean, false)
	  FROM broker_clusters WHERE {{tenant}} AND integration_id = ANY($1)
	  UNION ALL
	  SELECT integration_id, 'project', '', service, count(*)::text || ' deploys · last ' || to_char(max(ts) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI') || ' UTC', false
	  FROM deploy_markers WHERE {{tenant}} AND integration_id = ANY($1) GROUP BY integration_id, service
	  ORDER BY 1, 2, 4`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r model.DiscoveredResource
		var missing bool
		if err := rows.Scan(&id, &r.Kind, &r.RefID, &r.Name, &r.Detail, &missing); err != nil {
			return nil, err
		}
		if missing {
			r.Detail = strings.TrimPrefix(r.Detail+" · missing in the last discovery", " · ")
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

// ListIntegrations returns the project's cloud integrations with their discovered resources.
func (d *DB) ListIntegrations(ctx context.Context) ([]model.Integration, error) {
	return d.integrations(ctx, 0)
}

func (d *DB) integrations(ctx context.Context, only int64) ([]model.Integration, error) {
	where, args := "", []any{}
	if only > 0 {
		where, args = " AND id = $1", []any{only}
	}
	rows, err := d.pg.Query(ctx, `SELECT `+integrationCols+` FROM integrations WHERE {{tenant}}`+where+` ORDER BY provider, name`, args...)
	if err != nil {
		return nil, err
	}
	var list []integrationRow
	for rows.Next() {
		r, err := scanIntegration(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(list))
	for i, r := range list {
		ids[i] = r.id
	}
	res, err := d.discovered(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]model.Integration, 0, len(list))
	for _, r := range list {
		if x := res[r.id]; x != nil {
			r.m.Resources = x
		}
		// a Vercel project that has no deploy yet still shows up from the project map
		if r.m.Provider == "vercel" {
			if pm, ok := r.cfg["projectMap"].(map[string]any); ok {
				seen := map[string]bool{}
				for _, x := range r.m.Resources {
					seen[x.Name] = true
				}
				var names []string
				for p, svc := range pm {
					s, _ := svc.(string)
					if s == "" {
						s = p
					}
					if !seen[s] {
						names = append(names, s)
					}
				}
				sort.Strings(names)
				for _, n := range names {
					r.m.Resources = append(r.m.Resources, model.DiscoveredResource{Kind: "project", Name: n, Detail: "Vercel project · no deploys yet"})
				}
			}
		}
		out = append(out, r.m)
	}
	return out, nil
}

// GetIntegration returns one integration.
func (d *DB) GetIntegration(ctx context.Context, id string) (*model.Integration, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	list, err := d.integrations(ctx, n)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

func (d *DB) integrationRow(ctx context.Context, n int64) (integrationRow, error) {
	r, err := scanIntegration(d.pg.QueryRow(ctx, `SELECT `+integrationCols+` FROM integrations WHERE {{tenant}} AND id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// parseConfig validates an integration config object and moves an inline Vercel drain secret into the secrets table.
func (d *DB) parseConfig(ctx context.Context, provider, name string, raw json.RawMessage, base map[string]any) (map[string]any, error) {
	cfg := base
	if cfg == nil {
		cfg = map[string]any{}
	}
	if len(raw) > 0 && string(raw) != "null" {
		var in map[string]any
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, Invalid("config must be a JSON object")
		}
		for k, v := range in {
			if k == "syncRequestedAt" {
				continue
			}
			if k == "drainSecret" {
				continue
			}
			if secretConfigKeys[strings.ToLower(k)] {
				return nil, Invalid("config." + k + " looks like a secret: send it as `secret`, it is stored encrypted")
			}
			if v == nil {
				delete(cfg, k)
				continue
			}
			cfg[k] = v
		}
		if s, ok := in["drainSecret"].(string); ok && s != "" {
			if provider != "vercel" {
				return nil, Invalid("drainSecret is only used by vercel integrations")
			}
			if old, _ := cfg["drainSecretRef"].(string); old != "" {
				if ok, err := d.rotateSecret(ctx, old, []byte(s)); err != nil {
					return nil, err
				} else if ok {
					return cfg, nil
				}
			}
			ref, err := d.sealSecret(ctx, "integration:"+name+":drain", []byte(s))
			if err != nil {
				return nil, err
			}
			cfg["drainSecretRef"] = ref
		}
	}
	return cfg, nil
}

func validateProviderConfig(provider string, cfg map[string]any, hasSecret bool) error {
	switch provider {
	case "aws":
		if _, ok := cfg["roleArn"].(string); !ok && !hasSecret {
			return Invalid("aws needs config.roleArn (assume role) or an access key as secret")
		}
	case "gcp", "vercel", "neon":
		if !hasSecret {
			return Invalid(provider + " needs a secret (" + map[string]string{"gcp": "service-account JSON", "vercel": "access token", "neon": "API key"}[provider] + ")")
		}
	}
	return nil
}

// splitVercelSecret accepts the Vercel connect form's secret, a JSON string {"token","drainSecret"}: the token becomes
// the integration secret and the drain secret is merged into config as drainSecret, which parseConfig seals separately
// (config.drainSecretRef, resolved by ingest-gw). Any other secret is returned unchanged.
func splitVercelSecret(provider, secret string, config json.RawMessage) (string, json.RawMessage, error) {
	if provider != "vercel" || !strings.HasPrefix(strings.TrimSpace(secret), "{") {
		return secret, config, nil
	}
	var s struct {
		Token       string `json:"token"`
		DrainSecret string `json:"drainSecret"`
	}
	if err := json.Unmarshal([]byte(secret), &s); err != nil || s.Token == "" {
		return "", nil, Invalid(`vercel secret must be an access token or {"token","drainSecret"}`)
	}
	if s.DrainSecret == "" {
		return s.Token, config, nil
	}
	cfg := map[string]any{}
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return "", nil, Invalid("config must be a JSON object")
		}
	}
	cfg["drainSecret"] = s.DrainSecret
	b, _ := json.Marshal(cfg)
	return s.Token, b, nil
}

// CreateIntegration inserts an integration; the secret is sealed into the secrets table.
func (d *DB) CreateIntegration(ctx context.Context, in model.IntegrationInput) (*model.Integration, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !Providers[in.Provider] {
		return nil, Invalid("provider must be aws|gcp|vercel|neon")
	}
	if in.Name == "" {
		return nil, Invalid("name is required")
	}
	var err error
	if in.Secret, in.Config, err = splitVercelSecret(in.Provider, in.Secret, in.Config); err != nil {
		return nil, err
	}
	cfg, err := d.parseConfig(ctx, in.Provider, in.Name, in.Config, nil)
	if err != nil {
		return nil, err
	}
	if err := validateProviderConfig(in.Provider, cfg, in.Secret != ""); err != nil {
		return nil, err
	}
	ref := ""
	if in.Secret != "" {
		if ref, err = d.sealSecret(ctx, "integration:"+in.Name, []byte(in.Secret)); err != nil {
			return nil, err
		}
	}
	b, _ := json.Marshal(cfg)
	var id int64
	err = d.pg.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config, credentials_ref) VALUES ({{project}}, $1, $2, $3, nullif($4, ''))
	  RETURNING id`, in.Provider, in.Name, b, ref).Scan(&id)
	if err != nil {
		_ = d.deleteSecret(ctx, ref)
		if dr, _ := cfg["drainSecretRef"].(string); dr != "" {
			_ = d.deleteSecret(ctx, dr)
		}
		if isUnique(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return d.GetIntegration(ctx, strconv.FormatInt(id, 10))
}

// UpdateIntegration patches name / config / status and rotates the secret when one is given.
func (d *DB) UpdateIntegration(ctx context.Context, id string, p model.IntegrationPatch) (*model.Integration, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	cur, err := d.integrationRow(ctx, n)
	if err != nil {
		return nil, err
	}
	name := cur.m.Name
	if p.Name != nil {
		if name = strings.TrimSpace(*p.Name); name == "" {
			return nil, Invalid("name must not be empty")
		}
	}
	if p.Secret != nil {
		sec := *p.Secret
		if sec, p.Config, err = splitVercelSecret(cur.m.Provider, sec, p.Config); err != nil {
			return nil, err
		}
		p.Secret = &sec
	}
	cfg, err := d.parseConfig(ctx, cur.m.Provider, name, p.Config, cur.cfg)
	if err != nil {
		return nil, err
	}
	status, errText := cur.m.Status, cur.m.Error
	if p.Status != nil {
		switch *p.Status {
		case "disabled", "pending":
			status = *p.Status
		default:
			return nil, Invalid("status can be set to disabled or pending")
		}
	}
	ref := cur.creds
	if p.Secret != nil && *p.Secret != "" {
		rotated, err := d.rotateSecret(ctx, ref, []byte(*p.Secret))
		if err != nil {
			return nil, err
		}
		if !rotated {
			if ref, err = d.sealSecret(ctx, "integration:"+name, []byte(*p.Secret)); err != nil {
				return nil, err
			}
		}
		if status != "disabled" { // new credentials: let trooper-cloud retry
			status, errText = "pending", ""
		}
	}
	if len(p.Config) > 0 && status == "error" {
		status = "pending"
	}
	b, _ := json.Marshal(cfg)
	_, err = d.pg.Exec(ctx, `UPDATE integrations SET name = $2, config = $3, credentials_ref = nullif($4, ''), status = $5, error = nullif($6, '')
	  WHERE {{tenant}} AND id = $1`, n, name, b, ref, status, errText)
	if isUnique(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return d.GetIntegration(ctx, id)
}

// DeleteIntegration removes an integration and its secrets. Discovered databases / clusters / deploy markers keep
// their data with integration_id = NULL (FK ON DELETE SET NULL).
func (d *DB) DeleteIntegration(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	cur, err := d.integrationRow(ctx, n)
	if err != nil {
		return err
	}
	if _, err := d.pg.Exec(ctx, `DELETE FROM integrations WHERE {{tenant}} AND id = $1`, n); err != nil {
		return err
	}
	if err := d.deleteSecret(ctx, cur.creds); err != nil {
		return err
	}
	if dr, _ := cur.cfg["drainSecretRef"].(string); dr != "" {
		return d.deleteSecret(ctx, dr)
	}
	return nil
}

// RequestIntegrationSync stamps config.syncRequestedAt (unix ms); trooper-cloud polls it and syncs out of schedule.
func (d *DB) RequestIntegrationSync(ctx context.Context, id string, now time.Time) (*model.Integration, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE integrations SET config = jsonb_set(config, '{syncRequestedAt}', to_jsonb($2::bigint)),
	    status = CASE WHEN status = 'error' THEN 'pending' ELSE status END
	  WHERE {{tenant}} AND id = $1`, n, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.GetIntegration(ctx, id)
}

// ---------------------------------------------------------------- deploys

// DeployFilter narrows deploy markers.
type DeployFilter struct {
	Service string
	Source  string // spans | vercel | ci | manual | aws | gcp | "" (all)
}

var deploySources = map[string]bool{"spans": true, "vercel": true, "ci": true, "manual": true, "aws": true, "gcp": true}

// ListDeploys merges versions first seen in spans (ClickHouse deploys, source "spans") with Postgres deploy_markers,
// newest first.
func (d *DB) ListDeploys(ctx context.Context, q Q, f DeployFilter) ([]model.DeployMarker, error) {
	if f.Source != "" && !deploySources[f.Source] {
		return nil, Invalid("source must be spans|vercel|ci|manual|aws|gcp")
	}
	out := []model.DeployMarker{}
	if f.Source == "" || f.Source == "spans" {
		where, args := "", []any{}
		if f.Service != "" {
			where, args = " AND service = ?", []any{f.Service}
		}
		ec, ea := envCond(q.Env, "env")
		rows, err := d.ch.Query(ctx, `SELECT service, env, version, min(first_seen) AS fs FROM deploys WHERE {{tenant}}`+ec+where+`
		  GROUP BY service, env, version HAVING fs >= toDateTime(?) AND fs < toDateTime(?) ORDER BY fs DESC LIMIT 500`,
			append(append(ea, args...), secs(q.W.From), secs(q.W.To))...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			dp := model.DeployMarker{Source: "spans"}
			var t time.Time
			if err := rows.Scan(&dp.Service, &dp.Env, &dp.Version, &t); err != nil {
				rows.Close()
				return nil, err
			}
			dp.Ts = ms(t)
			out = append(out, dp)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if f.Source != "spans" {
		where := ""
		args := []any{q.W.From, q.W.To}
		add := func(cond string, v any) {
			args = append(args, v)
			where += fmt.Sprintf(" AND %s = $%d", cond, len(args))
		}
		if f.Service != "" {
			add("service", f.Service)
		}
		if f.Source != "" {
			add("source", f.Source)
		}
		if q.Env != "" {
			add("env", q.Env)
		}
		rows, err := d.pg.Query(ctx, `SELECT service, env, version, ts, source, coalesce(url, ''), coalesce(commit_message, ''), coalesce(author, ''), state,
		    coalesce(integration_id::text, '')
		  FROM deploy_markers WHERE {{tenant}} AND ts >= $1 AND ts < $2`+where+` ORDER BY ts DESC LIMIT 500`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var dp model.DeployMarker
			var t time.Time
			if err := rows.Scan(&dp.Service, &dp.Env, &dp.Version, &t, &dp.Source, &dp.URL, &dp.CommitMessage, &dp.Author, &dp.State, &dp.IntegrationID); err != nil {
				return nil, err
			}
			dp.Ts = ms(t)
			out = append(out, dp)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	if len(out) > 500 {
		out = out[:500]
	}
	return out, nil
}

// SecretsKeyOrDefault returns the sealing key (TT_SECRETS_KEY or the dev key).
func (d *DB) SecretsKeyOrDefault() []byte {
	k, _ := d.key()
	return k
}
