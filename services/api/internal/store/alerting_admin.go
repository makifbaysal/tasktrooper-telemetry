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
)

// ---------------------------------------------------------------- maintenance windows

const mwCols = `id, name, starts_at, ends_at, scope`

func scanMW(row pgx.Row, now time.Time) (model.MaintenanceWindow, error) {
	var w model.MaintenanceWindow
	var id int64
	var from, to time.Time
	var scope []byte
	if err := row.Scan(&id, &w.Name, &from, &to, &scope); err != nil {
		return w, err
	}
	w.ID, w.StartsAt, w.EndsAt = strconv.FormatInt(id, 10), ms(from), ms(to)
	_ = json.Unmarshal(scope, &w.Scope)
	w.Active = !now.Before(from) && now.Before(to)
	return w, nil
}

// MaintenanceFilter narrows maintenance windows: active now / upcoming (not ended) / all.
type MaintenanceFilter struct {
	Only string // active | upcoming | ""
}

// ListMaintenanceWindows returns windows newest first.
func (d *DB) ListMaintenanceWindows(ctx context.Context, now time.Time, f MaintenanceFilter) ([]model.MaintenanceWindow, error) {
	where, args := "", []any{}
	switch f.Only {
	case "":
	case "active":
		where, args = " AND starts_at <= $1 AND ends_at > $1", []any{now}
	case "upcoming":
		where, args = " AND ends_at > $1", []any{now}
	default:
		return nil, Invalid("only must be active|upcoming")
	}
	rows, err := d.pg.Query(ctx, `SELECT `+mwCols+` FROM maintenance_windows WHERE {{tenant}}`+where+` ORDER BY starts_at DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.MaintenanceWindow{}
	for rows.Next() {
		w, err := scanMW(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetMaintenanceWindow returns one window.
func (d *DB) GetMaintenanceWindow(ctx context.Context, now time.Time, id string) (*model.MaintenanceWindow, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	w, err := scanMW(d.pg.QueryRow(ctx, `SELECT `+mwCols+` FROM maintenance_windows WHERE {{tenant}} AND id = $1`, n), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &w, err
}

func validateMW(in *model.MaintenanceWindowInput) ([]byte, error) {
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "":
		return nil, Invalid("name is required")
	case in.StartsAt <= 0 || in.EndsAt <= 0:
		return nil, Invalid("startsAt and endsAt (unix ms) are required")
	case in.EndsAt <= in.StartsAt:
		return nil, Invalid("endsAt must be after startsAt")
	}
	for _, id := range in.Scope.MonitorIDs {
		if _, err := parseID(id); err != nil {
			return nil, Invalid("scope.monitorIds must be monitor ids")
		}
	}
	b, _ := json.Marshal(in.Scope)
	return b, nil
}

// CreateMaintenanceWindow inserts a window.
func (d *DB) CreateMaintenanceWindow(ctx context.Context, now time.Time, in model.MaintenanceWindowInput) (*model.MaintenanceWindow, error) {
	scope, err := validateMW(&in)
	if err != nil {
		return nil, err
	}
	var id int64
	if err := d.pg.QueryRow(ctx, `INSERT INTO maintenance_windows (project_id, name, starts_at, ends_at, scope) VALUES ({{project}}, $1, $2, $3, $4) RETURNING id`,
		in.Name, time.UnixMilli(in.StartsAt), time.UnixMilli(in.EndsAt), scope).Scan(&id); err != nil {
		return nil, err
	}
	return d.GetMaintenanceWindow(ctx, now, strconv.FormatInt(id, 10))
}

// UpdateMaintenanceWindow replaces a window.
func (d *DB) UpdateMaintenanceWindow(ctx context.Context, now time.Time, id string, in model.MaintenanceWindowInput) (*model.MaintenanceWindow, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	scope, err := validateMW(&in)
	if err != nil {
		return nil, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE maintenance_windows SET name = $2, starts_at = $3, ends_at = $4, scope = $5 WHERE {{tenant}} AND id = $1`,
		n, in.Name, time.UnixMilli(in.StartsAt), time.UnixMilli(in.EndsAt), scope)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.GetMaintenanceWindow(ctx, now, id)
}

// DeleteMaintenanceWindow removes a window.
func (d *DB) DeleteMaintenanceWindow(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `DELETE FROM maintenance_windows WHERE {{tenant}} AND id = $1`, n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- notification channels

// channelSecretKeys are the config fields stored in the secrets table, per channel type.
var channelSecretKeys = map[string][]string{
	"slack": {"webhookUrl"}, "teams": {"webhookUrl"}, "pagerduty": {"routingKey"}, "opsgenie": {"apiKey"}, "webhook": {"secret"}, "email": nil,
}

// channelRequired are the config fields a channel must have.
var channelRequired = map[string][]string{
	"slack": {"webhookUrl"}, "teams": {"webhookUrl"}, "pagerduty": {"routingKey"}, "opsgenie": {"apiKey"}, "webhook": {"url"}, "email": {"to"},
}

// IsRef reports whether v is a credentials reference (kept as is, never sealed).
func IsRef(v string) bool {
	return strings.HasPrefix(v, "secret:") || strings.HasPrefix(v, "env:") || strings.HasPrefix(v, "file:")
}

func scanChannel(row pgx.Row) (model.NotificationChannel, error) {
	var c model.NotificationChannel
	var id int64
	var cfg []byte
	if err := row.Scan(&id, &c.Name, &c.Type, &cfg); err != nil {
		return c, err
	}
	c.ID = strconv.FormatInt(id, 10)
	_ = json.Unmarshal(cfg, &c.Config)
	if c.Config == nil {
		c.Config = map[string]any{}
	}
	c.Secrets = []string{}
	for _, k := range channelSecretKeys[c.Type] {
		v, _ := c.Config[k].(string)
		if strings.HasPrefix(v, "secret:") {
			c.Secrets = append(c.Secrets, k)
		} else if v != "" && !IsRef(v) {
			c.Config[k] = "••••" // legacy plaintext row: never echo it
		}
	}
	return c, nil
}

// ListNotificationChannels returns the project's channels.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]model.NotificationChannel, error) {
	rows, err := d.pg.Query(ctx, `SELECT id, name, type, config FROM notification_channels WHERE {{tenant}} ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.NotificationChannel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetNotificationChannel returns one channel.
func (d *DB) GetNotificationChannel(ctx context.Context, id string) (*model.NotificationChannel, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	c, err := scanChannel(d.pg.QueryRow(ctx, `SELECT id, name, type, config FROM notification_channels WHERE {{tenant}} AND id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// channelConfig validates in and seals plaintext secret fields; prev is the stored config on update.
// Returns the config to store and the secret refs created in this call (to roll back on failure).
func (d *DB) channelConfig(ctx context.Context, in *model.NotificationChannelInput, prev map[string]any) (map[string]any, []string, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, nil, Invalid("name is required")
	}
	keys, ok := channelSecretKeys[in.Type]
	if !ok {
		return nil, nil, Invalid("type must be slack|pagerduty|email|webhook|opsgenie|teams")
	}
	cfg := map[string]any{}
	for k, v := range in.Config {
		cfg[k] = v
	}
	var created []string
	for _, k := range keys {
		v, isStr := cfg[k].(string)
		old, _ := prev[k].(string)
		switch {
		case cfg[k] == nil || (isStr && (v == "" || v == "••••")):
			if old != "" { // omitted on update: keep the stored reference
				cfg[k] = old
			} else {
				delete(cfg, k)
			}
		case !isStr:
			return nil, created, Invalid("config." + k + " must be a string")
		case IsRef(v):
		default:
			if rotated, err := d.rotateSecret(ctx, old, []byte(v)); err != nil {
				return nil, created, err
			} else if rotated {
				cfg[k] = old
				continue
			}
			ref, err := d.sealSecret(ctx, "channel:"+in.Name+":"+k, []byte(v))
			if err != nil {
				return nil, created, err
			}
			created = append(created, ref)
			cfg[k] = ref
		}
	}
	for _, k := range channelRequired[in.Type] {
		if v, ok := cfg[k]; !ok || v == "" {
			return nil, created, Invalid(in.Type + " channels need config." + k)
		}
	}
	return cfg, created, nil
}

// CreateNotificationChannel inserts a channel.
func (d *DB) CreateNotificationChannel(ctx context.Context, in model.NotificationChannelInput) (*model.NotificationChannel, error) {
	cfg, created, err := d.channelConfig(ctx, &in, nil)
	rollback := func() {
		for _, r := range created {
			_ = d.deleteSecret(ctx, r)
		}
	}
	if err != nil {
		rollback()
		return nil, err
	}
	b, _ := json.Marshal(cfg)
	var id int64
	err = d.pg.QueryRow(ctx, `INSERT INTO notification_channels (project_id, name, type, config) VALUES ({{project}}, $1, $2, $3) RETURNING id`,
		in.Name, in.Type, b).Scan(&id)
	if err != nil {
		rollback()
		if isUnique(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return d.GetNotificationChannel(ctx, strconv.FormatInt(id, 10))
}

// UpdateNotificationChannel replaces a channel; secret fields left empty keep their stored value, new values rotate it.
func (d *DB) UpdateNotificationChannel(ctx context.Context, id string, in model.NotificationChannelInput) (*model.NotificationChannel, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	var prevType string
	var prevCfg []byte
	err = d.pg.QueryRow(ctx, `SELECT type, config FROM notification_channels WHERE {{tenant}} AND id = $1`, n).Scan(&prevType, &prevCfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	prev := map[string]any{}
	_ = json.Unmarshal(prevCfg, &prev)
	if in.Type == "" {
		in.Type = prevType
	}
	if in.Type != prevType {
		prev = map[string]any{} // different secrets; old ones are dropped below
	}
	cfg, created, err := d.channelConfig(ctx, &in, prev)
	if err != nil {
		for _, r := range created {
			_ = d.deleteSecret(ctx, r)
		}
		return nil, err
	}
	b, _ := json.Marshal(cfg)
	_, err = d.pg.Exec(ctx, `UPDATE notification_channels SET name = $2, type = $3, config = $4 WHERE {{tenant}} AND id = $1`, n, in.Name, in.Type, b)
	if isUnique(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if in.Type != prevType { // drop secrets of the old type
		old := map[string]any{}
		_ = json.Unmarshal(prevCfg, &old)
		for _, k := range channelSecretKeys[prevType] {
			if v, _ := old[k].(string); strings.HasPrefix(v, "secret:") {
				_ = d.deleteSecret(ctx, v)
			}
		}
	}
	return d.GetNotificationChannel(ctx, id)
}

// DeleteNotificationChannel removes a channel and its sealed secrets.
func (d *DB) DeleteNotificationChannel(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	var typ string
	var cfg []byte
	err = d.pg.QueryRow(ctx, `DELETE FROM notification_channels WHERE {{tenant}} AND id = $1 RETURNING type, config`, n).Scan(&typ, &cfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	m := map[string]any{}
	_ = json.Unmarshal(cfg, &m)
	for _, k := range channelSecretKeys[typ] {
		if v, _ := m[k].(string); strings.HasPrefix(v, "secret:") {
			if err := d.deleteSecret(ctx, v); err != nil {
				return err
			}
		}
	}
	return nil
}
