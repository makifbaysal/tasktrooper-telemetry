// Package store is the alert engine's Postgres access: monitors, KPIs, incidents and the notification outbox.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// Store wraps a pgx pool.
type Store struct{ DB *pgxpool.Pool }

// Shards selects monitors/KPIs by id modulo N (see README § Sharding). N ≤ 1 means everything.
type Shards struct {
	N    int
	Held []int
}

// All is the single-shard selection.
var All = Shards{N: 1, Held: []int{0}}

func (s Shards) filter(col string, argN int) (string, []any) {
	if s.N <= 1 {
		return "true", nil
	}
	held := make([]int32, len(s.Held))
	for i, h := range s.Held {
		held[i] = int32(h)
	}
	return fmt.Sprintf("(%s %% $%d) = ANY($%d)", col, argN, argN+1), []any{int64(s.N), held}
}

// Owns reports whether id belongs to a held shard.
func (s Shards) Owns(id int64) bool {
	if s.N <= 1 {
		return true
	}
	for _, h := range s.Held {
		if id%int64(s.N) == int64(h) {
			return true
		}
	}
	return false
}

// Fingerprint changes whenever an active monitor or KPI is added, removed or edited.
func (st *Store) Fingerprint(ctx context.Context) (string, error) {
	var mc, kc int64
	var mu, ku *time.Time
	var mmax, kmax *int64
	var kcond string
	err := st.DB.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM monitors WHERE status = 'active'), (SELECT max(updated_at) FROM monitors), (SELECT max(id) FROM monitors),
		(SELECT count(*) FROM kpis), (SELECT max(created_at) FROM kpis), (SELECT coalesce(max(id), 0) + coalesce(sum(threshold)::bigint, 0) + count(*) FROM kpi_alert_rules),
		(SELECT md5(coalesce(string_agg(id || ':' || condition::text, ',' ORDER BY id), '')) FROM kpi_alert_rules)`).
		Scan(&mc, &mu, &mmax, &kc, &ku, &kmax, &kcond)
	if err != nil {
		return "", err
	}
	f := func(t *time.Time) int64 {
		if t == nil {
			return 0
		}
		return t.UnixNano()
	}
	g := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return fmt.Sprintf("%d/%d/%d/%d/%d/%d/%s", mc, f(mu), g(mmax), kc, f(ku), g(kmax), kcond), nil
}

// LoadMonitors returns active monitors in the held shards. Rows whose JSON does not parse are returned
// with Kind "invalid" so they surface as no_data instead of being skipped silently.
func (st *Store) LoadMonitors(ctx context.Context, sh Shards) ([]*model.Monitor, error) {
	cond, args := sh.filter("m.id", 1)
	return st.loadMonitors(ctx, cond, args)
}

// LoadStreamMonitors returns every active stream rule (condition.stream = true, stream kinds) across all shards:
// the stream consumer evaluates them all and hands hits on other shards' monitors to their owner.
func (st *Store) LoadStreamMonitors(ctx context.Context) ([]*model.Monitor, error) {
	return st.loadMonitors(ctx, `m.kind IN ('log_match', 'log_count', 'error_new') AND m.condition->>'stream' = 'true'`, nil)
}

func (st *Store) loadMonitors(ctx context.Context, cond string, args []any) ([]*model.Monitor, error) {
	// source / code_ref via to_jsonb: works before and after the column migration (alert-as-code).
	rows, err := st.DB.Query(ctx, `SELECT m.id, p.org_id, m.project_id, m.name, m.kind, m.target, m.query, m.condition,
		m.severity, m.channels, m.state, m.state_since, m.last_value, m.updated_at, m.muted_until, coalesce(bc.kind, ''),
		coalesce(to_jsonb(m)->>'source', ''), coalesce(to_jsonb(m)->>'code_ref', '')
		FROM monitors m JOIN projects p ON p.id = m.project_id
		LEFT JOIN broker_clusters bc ON bc.project_id = m.project_id AND bc.slug = m.query->>'clusterId'
		WHERE m.status = 'active' AND `+cond+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Monitor
	for rows.Next() {
		var m model.Monitor
		var org, project int64
		var target, query, condition []byte
		var state string
		if err := rows.Scan(&m.ID, &org, &project, &m.Name, &m.Kind, &target, &query, &condition,
			&m.Severity, &m.Channels, &state, &m.StateSince, &m.LastValue, &m.UpdatedAt, &m.MutedUntil, &m.ClusterKind, &m.Source, &m.CodeRef); err != nil {
			return nil, err
		}
		m.OrgID, m.ProjectID, m.State = uint32(org), uint32(project), model.State(state)
		if err := decode(target, &m.Target, query, &m.Query, condition, &m.Condition); err != nil {
			m.Kind = "invalid:" + m.Kind
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func decode(pairs ...any) error {
	var errs []error
	for i := 0; i+1 < len(pairs); i += 2 {
		b := pairs[i].([]byte)
		if len(b) == 0 {
			continue
		}
		errs = append(errs, json.Unmarshal(b, pairs[i+1]))
	}
	return errors.Join(errs...)
}

// SaveEval persists the evaluation result (state columns change only on transitions).
func (st *Store) SaveEval(ctx context.Context, id int64, state model.State, since time.Time, value *float64, at time.Time) error {
	_, err := st.DB.Exec(ctx, `UPDATE monitors SET state = $2, state_since = $3, last_value = $4, last_eval_at = $5 WHERE id = $1`,
		id, string(state), since, value, at)
	return err
}

// LoadKpis returns KPIs (with rules) in the held shards.
func (st *Store) LoadKpis(ctx context.Context, sh Shards) ([]*model.Kpi, error) {
	cond, args := sh.filter("k.id", 1)
	rows, err := st.DB.Query(ctx, `SELECT k.id, p.org_id, k.project_id, k.name, k.target, k.sli, k.good_when, k.objective, k."window"
		FROM kpis k JOIN projects p ON p.id = k.project_id WHERE `+cond+` ORDER BY k.id`, args...)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*model.Kpi{}
	var out []*model.Kpi
	for rows.Next() {
		var k model.Kpi
		var org, project int64
		var target, good []byte
		var obj float32
		if err := rows.Scan(&k.ID, &org, &project, &k.Name, &target, &k.SLI, &good, &obj, &k.Window); err != nil {
			rows.Close()
			return nil, err
		}
		k.OrgID, k.ProjectID, k.Objective = uint32(org), uint32(project), float64(obj)
		_ = decode(target, &k.Target, good, &k.GoodWhen)
		byID[k.ID] = &k
		out = append(out, &k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(out))
	for _, k := range out {
		ids = append(ids, k.ID)
	}
	rr, err := st.DB.Query(ctx, `SELECT id, kpi_id, kind, coalesce(long_window, ''), coalesce(short_window, ''), threshold, severity, channels,
		state, state_since, condition FROM kpi_alert_rules WHERE kpi_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer rr.Close()
	for rr.Next() {
		var r model.KpiRule
		var kpiID int64
		var lw, sw string
		var th float32
		var state string
		var cond []byte
		if err := rr.Scan(&r.ID, &kpiID, &r.Kind, &lw, &sw, &th, &r.Severity, &r.Channels, &state, &r.StateSince, &cond); err != nil {
			return nil, err
		}
		r.State = model.State(state)
		r.Threshold = float64(th)
		r.LongWindow, _ = model.ParseDuration(lw)
		r.ShortWindow, _ = model.ParseDuration(sw)
		if len(cond) > 0 {
			var c model.Condition
			if json.Unmarshal(cond, &c) == nil {
				r.Renotify = c.Renotify
			}
		}
		if k := byID[kpiID]; k != nil {
			k.Rules = append(k.Rules, r)
		}
	}
	return out, rr.Err()
}

// Incident is what the engine opens.
type Incident struct {
	ProjectID         int64
	MonitorID, KpiID  *int64
	KpiRuleID         *int64
	Title, Severity   string
	Service, Endpoint string
	Value             float64
	RootCause         any // marshalled into incidents.root_cause; nil = NULL
	Event             map[string]any
	Channels          []string
	// Suppressed (muted | maintenance) records the incident but enqueues no notifications.
	Suppressed string
}

// MonitorState is written in the same transaction as the incident change.
type MonitorState struct {
	ID    int64
	State model.State
	Since time.Time
	Value *float64
	At    time.Time
}

// PayloadFunc builds the notification payload once the incident id is known.
type PayloadFunc func(incidentID int64, startedAt time.Time) map[string]any

// Open creates the open incident as firing, or promotes the monitor's pending incident to firing, logs a fired
// event and enqueues firing notifications (none when inc.Suppressed). created is false when a firing/acknowledged
// incident already existed (no notifications then).
func (st *Store) Open(ctx context.Context, inc Incident, ms *MonitorState, payload PayloadFunc) (id int64, created bool, err error) {
	err = pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		created = false
		if err := saveState(ctx, tx, ms); err != nil {
			return err
		}
		var rc []byte
		if inc.RootCause != nil {
			if rc, err = json.Marshal(inc.RootCause); err != nil {
				return err
			}
		}
		var started time.Time
		switch {
		case inc.MonitorID != nil:
			err = tx.QueryRow(ctx, `INSERT INTO incidents (project_id, monitor_id, kpi_id, title, severity, service, endpoint, state, peak_value, root_cause)
				VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), 'firing', $8, $9)
				ON CONFLICT (monitor_id) WHERE state <> 'resolved' AND monitor_id IS NOT NULL DO NOTHING
				RETURNING id, started_at`,
				inc.ProjectID, inc.MonitorID, inc.KpiID, inc.Title, inc.Severity, inc.Service, inc.Endpoint, inc.Value, rc).Scan(&id, &started)
			if errors.Is(err, pgx.ErrNoRows) {
				// an open incident exists: promote it when pending, otherwise nothing to do
				err = tx.QueryRow(ctx, `UPDATE incidents SET state = 'firing', severity = $2, peak_value = $3, root_cause = coalesce($4, root_cause),
					title = $5 WHERE monitor_id = $1 AND state = 'pending' RETURNING id, started_at`,
					*inc.MonitorID, inc.Severity, inc.Value, rc, inc.Title).Scan(&id, &started)
				if errors.Is(err, pgx.ErrNoRows) {
					return tx.QueryRow(ctx, `SELECT id FROM incidents WHERE monitor_id = $1 AND state <> 'resolved'`, *inc.MonitorID).Scan(&id)
				}
			}
			if err != nil {
				return err
			}
		case inc.KpiRuleID != nil:
			err = tx.QueryRow(ctx, `INSERT INTO incidents (project_id, kpi_id, kpi_rule_id, title, severity, service, endpoint, state, peak_value, root_cause)
				VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), 'firing', $8, $9)
				ON CONFLICT (kpi_rule_id) WHERE state <> 'resolved' AND kpi_rule_id IS NOT NULL DO NOTHING
				RETURNING id, started_at`,
				inc.ProjectID, inc.KpiID, inc.KpiRuleID, inc.Title, inc.Severity, inc.Service, inc.Endpoint, inc.Value, rc).Scan(&id, &started)
			if errors.Is(err, pgx.ErrNoRows) {
				return tx.QueryRow(ctx, `SELECT id FROM incidents WHERE kpi_rule_id = $1 AND state <> 'resolved'`, *inc.KpiRuleID).Scan(&id)
			}
			if err != nil {
				return err
			}
		default:
			return errors.New("store: incident needs a monitor or KPI rule")
		}
		created = true
		ev := inc.Event
		if inc.Suppressed != "" {
			ev = withSuppressed(ev, inc.Suppressed)
		}
		if err := event(ctx, tx, id, "fired", ev); err != nil {
			return err
		}
		if inc.Suppressed != "" {
			return event(ctx, tx, id, "suppressed", map[string]any{"reason": inc.Suppressed, "notification": "firing", "channels": inc.Channels})
		}
		return enqueue(ctx, tx, id, "firing", inc.Channels, payload(id, started))
	})
	return id, created, err
}

func withSuppressed(ev map[string]any, reason string) map[string]any {
	out := make(map[string]any, len(ev)+2)
	for k, v := range ev {
		out[k] = v
	}
	out["suppressed"], out["reason"] = true, reason
	return out
}

func saveState(ctx context.Context, tx pgx.Tx, ms *MonitorState) error {
	if ms == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE monitors SET state = $2, state_since = $3, last_value = $4, last_eval_at = $5 WHERE id = $1`,
		ms.ID, string(ms.State), ms.Since, ms.Value, ms.At)
	return err
}

// OpenPending records a pending incident (condition met, forMin not elapsed): no notifications. Idempotent.
func (st *Store) OpenPending(ctx context.Context, inc Incident, ms *MonitorState) (id int64, err error) {
	err = pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		if err := saveState(ctx, tx, ms); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO incidents (project_id, monitor_id, title, severity, service, endpoint, state, peak_value)
			VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), 'pending', $7)
			ON CONFLICT (monitor_id) WHERE state <> 'resolved' AND monitor_id IS NOT NULL DO NOTHING RETURNING id`,
			inc.ProjectID, inc.MonitorID, inc.Title, inc.Severity, inc.Service, inc.Endpoint, inc.Value).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already open
		}
		if err != nil {
			return err
		}
		return event(ctx, tx, id, "pending", inc.Event)
	})
	return id, err
}

// Peak raises (or lowers, for '<' conditions) the open incident's peak_value.
func (st *Store) Peak(ctx context.Context, monitorID int64, v float64, lowerIsWorse bool) error {
	fn := "greatest"
	if lowerIsWorse {
		fn = "least"
	}
	_, err := st.DB.Exec(ctx, `UPDATE incidents SET peak_value = `+fn+`(coalesce(peak_value, $2), $2)
		WHERE monitor_id = $1 AND state <> 'resolved'`, monitorID, v)
	return err
}

// PeakIncident is Peak by incident id (KPI incidents).
func (st *Store) PeakIncident(ctx context.Context, id int64, v float64, lowerIsWorse bool) error {
	fn := "greatest"
	if lowerIsWorse {
		fn = "least"
	}
	_, err := st.DB.Exec(ctx, `UPDATE incidents SET peak_value = `+fn+`(coalesce(peak_value, $2), $2) WHERE id = $1`, id, v)
	return err
}

// OpenKpiIncident returns the open incident id of a KPI rule (incidents.kpi_rule_id; rows written before that
// column existed are matched by kpi + deterministic title), or 0.
func (st *Store) OpenKpiIncident(ctx context.Context, ruleID, kpiID int64, title string) (int64, error) {
	var id int64
	err := st.DB.QueryRow(ctx, `SELECT id FROM incidents WHERE state <> 'resolved' AND monitor_id IS NULL
		AND (kpi_rule_id = $1 OR (kpi_rule_id IS NULL AND kpi_id = $2 AND title = $3)) ORDER BY (kpi_rule_id = $1) DESC NULLS LAST, id LIMIT 1`,
		ruleID, kpiID, title).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Resolved describes a resolved incident for the notification payload.
type Resolved struct {
	ID         int64
	StartedAt  time.Time
	ResolvedAt time.Time
	Peak       *float64
	Summary    string
	Quiet      bool     // resolved from pending, never notified
	Confidence *float64 // root_cause.confidence
}

// Resolve closes the open incident selected by monitor id or incident id, logs and enqueues resolved notifications.
// Returns ok=false when nothing was open.
//
// A pending incident is resolved quietly (quiet = true, no notifications). Resolved notifications go only to
// channels that were sent (or queued) the firing one, and none while suppressed (muted / maintenance).
func (st *Store) Resolve(ctx context.Context, monitorID, incidentID int64, ms *MonitorState, channels []string, ev map[string]any,
	suppressed string, payload func(Resolved) map[string]any) (r Resolved, ok bool, err error) {
	err = pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		ok = false
		if err := saveState(ctx, tx, ms); err != nil {
			return err
		}
		sel := "monitor_id = $1"
		key := monitorID
		if incidentID != 0 {
			sel, key = "id = $1", incidentID
		}
		var summary *string
		err := tx.QueryRow(ctx, `UPDATE incidents SET state = 'resolved', resolved_at = now(), quiet = (state = 'pending')
			WHERE `+sel+` AND state <> 'resolved' RETURNING id, started_at, resolved_at, peak_value, root_cause->>'summary', quiet, (root_cause->>'confidence')::float8`, key).
			Scan(&r.ID, &r.StartedAt, &r.ResolvedAt, &r.Peak, &summary, &r.Quiet, &r.Confidence)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		if summary != nil {
			r.Summary = *summary
		}
		if r.Quiet {
			return event(ctx, tx, r.ID, "resolved", withQuiet(ev))
		}
		if suppressed != "" {
			ev = withSuppressed(ev, suppressed)
		}
		if err := event(ctx, tx, r.ID, "resolved", ev); err != nil {
			return err
		}
		if suppressed != "" {
			return event(ctx, tx, r.ID, "suppressed", map[string]any{"reason": suppressed, "notification": "resolved", "channels": channels})
		}
		notified, err := firingChannels(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		var to []string
		for _, ch := range channels {
			if notified[strings.TrimSpace(ch)] {
				to = append(to, ch)
			}
		}
		return enqueue(ctx, tx, r.ID, "resolved", to, payload(r))
	})
	return r, ok, err
}

func withQuiet(ev map[string]any) map[string]any {
	out := make(map[string]any, len(ev)+1)
	for k, v := range ev {
		out[k] = v
	}
	out["quiet"] = true
	return out
}

func firingChannels(ctx context.Context, tx pgx.Tx, incidentID int64) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT channel FROM notifications WHERE incident_id = $1 AND kind = 'firing'`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[c] = true
	}
	return out, rows.Err()
}

func event(ctx context.Context, tx pgx.Tx, incidentID int64, kind string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO incident_events (incident_id, kind, data) VALUES ($1, $2, $3)`, incidentID, kind, b)
	return err
}

// DedupeKey is unique per (incident, kind, channel).
func DedupeKey(incidentID int64, kind, channel string) string {
	return fmt.Sprintf("incident:%d:%s:%s", incidentID, kind, strings.TrimSpace(channel))
}

func enqueue(ctx context.Context, tx pgx.Tx, incidentID int64, kind string, channels []string, payload map[string]any) error {
	return enqueueKind(ctx, tx, incidentID, kind, kind, channels, payload)
}

// enqueueKind writes outbox rows of notification kind with dedupe key incident:<id>:<dedupe>:<channel>.
func enqueueKind(ctx context.Context, tx pgx.Tx, incidentID int64, kind, dedupe string, channels []string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, ch := range channels {
		ch = strings.TrimSpace(ch)
		if ch == "" || seen[ch] {
			continue
		}
		seen[ch] = true
		if _, err := tx.Exec(ctx, `INSERT INTO notifications (incident_id, channel, kind, payload, dedupe_key)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (dedupe_key) DO NOTHING`, incidentID, ch, kind, b, DedupeKey(incidentID, dedupe, ch)); err != nil {
			return err
		}
	}
	return nil
}

// ───────────────────────── mute, maintenance, reminders, KPI rule state, composite ─────────────────────────

// Maintenance is an active maintenance_windows row. Empty Services and MonitorIDs = the whole project.
type Maintenance struct {
	ID         int64
	ProjectID  int64
	Name       string
	Services   []string
	MonitorIDs []int64
}

// Covers reports whether the window applies to a monitor (or KPI: monitorID 0) of service in project.
func (w Maintenance) Covers(projectID, monitorID int64, service string) bool {
	if w.ProjectID != projectID {
		return false
	}
	if len(w.Services) == 0 && len(w.MonitorIDs) == 0 {
		return true
	}
	for _, id := range w.MonitorIDs {
		if monitorID != 0 && id == monitorID {
			return true
		}
	}
	for _, s := range w.Services {
		if service != "" && strings.EqualFold(s, service) {
			return true
		}
	}
	return false
}

// Suppression is the per-tick snapshot of mutes and active maintenance windows.
type Suppression struct {
	Muted   map[int64]time.Time // monitor id → muted_until (> now)
	Windows []Maintenance
}

// Reason returns "muted", "maintenance:<id>" or "" for a monitor (monitorID 0 = KPI rule).
func (s Suppression) Reason(projectID, monitorID int64, service string) string {
	if monitorID != 0 {
		if _, ok := s.Muted[monitorID]; ok {
			return "muted"
		}
	}
	for _, w := range s.Windows {
		if w.Covers(projectID, monitorID, service) {
			return fmt.Sprintf("maintenance:%d", w.ID)
		}
	}
	return ""
}

// LoadSuppression reads monitors muted at now and maintenance windows active at now (all projects, or projectIDs).
func (st *Store) LoadSuppression(ctx context.Context, now time.Time, projectIDs []int64) (Suppression, error) {
	s := Suppression{Muted: map[int64]time.Time{}}
	filter := projectIDs
	if filter == nil {
		filter = []int64{}
	}
	rows, err := st.DB.Query(ctx, `SELECT id, muted_until FROM monitors
		WHERE muted_until > $1 AND status = 'active' AND (cardinality($2::bigint[]) = 0 OR project_id = ANY($2))`, now, filter)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var id int64
		var until time.Time
		if err := rows.Scan(&id, &until); err != nil {
			rows.Close()
			return s, err
		}
		s.Muted[id] = until
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}
	rows, err = st.DB.Query(ctx, `SELECT id, project_id, name, scope FROM maintenance_windows
		WHERE starts_at <= $1 AND ends_at > $1 AND (cardinality($2::bigint[]) = 0 OR project_id = ANY($2))`, now, filter)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var w Maintenance
		var scope []byte
		if err := rows.Scan(&w.ID, &w.ProjectID, &w.Name, &scope); err != nil {
			return s, err
		}
		var sc struct {
			Services   []string          `json:"services"`
			MonitorIDs []model.FlexInt64 `json:"monitorIds"`
		}
		_ = json.Unmarshal(scope, &sc)
		w.Services = sc.Services
		for _, id := range sc.MonitorIDs {
			w.MonitorIDs = append(w.MonitorIDs, int64(id))
		}
		s.Windows = append(s.Windows, w)
	}
	return s, rows.Err()
}

// ReminderDue is what Remind needs to know about the open incident.
type ReminderDue struct {
	IncidentID int64
	StartedAt  time.Time
	FiredAt    time.Time
	N          int // reminder number (1, 2, …)
	Peak       *float64
	Summary    string
	Confidence *float64 // root_cause.confidence (0..1), nil without RCA
}

// Remind enqueues reminder number floor((now − fired)/every) for the monitor's open incident when it is still
// firing (not acknowledged). Dedupe key incident:<id>:reminder-<n>:<channel> makes it idempotent across ticks
// and restarts. sent=false when nothing was due. With suppressed != "" it only logs a suppressed event.
func (st *Store) Remind(ctx context.Context, sel IncidentSel, every time.Duration, now time.Time, channels []string, suppressed string,
	payload func(ReminderDue) map[string]any) (due ReminderDue, sent bool, err error) {
	if every <= 0 {
		return due, false, nil
	}
	err = pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		sent = false
		col, key := sel.where()
		var state string
		var summary *string
		err := tx.QueryRow(ctx, `SELECT i.id, i.state, i.started_at,
				coalesce((SELECT max(ts) FROM incident_events e WHERE e.incident_id = i.id AND e.kind = 'fired'), i.started_at),
				i.peak_value, i.root_cause->>'summary', (i.root_cause->>'confidence')::float8
			FROM incidents i WHERE `+col+` = $1 AND i.state <> 'resolved' FOR UPDATE`, key).Scan(&due.IncidentID, &state, &due.StartedAt, &due.FiredAt,
			&due.Peak, &summary, &due.Confidence)
		if summary != nil {
			due.Summary = *summary
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if state != "firing" { // acknowledged incidents (and pending ones) never remind
			return nil
		}
		due.N = int(now.Sub(due.FiredAt) / every)
		if due.N < 1 {
			return nil
		}
		kind := fmt.Sprintf("reminder-%d", due.N)
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM incident_events WHERE incident_id = $1 AND kind IN ('reminder', 'suppressed') AND data->>'n' = $2)`,
			due.IncidentID, fmt.Sprint(due.N)).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if suppressed != "" {
			return event(ctx, tx, due.IncidentID, "suppressed", map[string]any{"reason": suppressed, "notification": "reminder", "n": due.N})
		}
		sent = true
		if err := event(ctx, tx, due.IncidentID, "reminder", map[string]any{"n": due.N, "channels": channels}); err != nil {
			return err
		}
		return enqueueKind(ctx, tx, due.IncidentID, "reminder", kind, channels, payload(due))
	})
	return due, sent, err
}

// IncidentSel selects an open incident by monitor or KPI rule.
type IncidentSel struct{ MonitorID, KpiRuleID int64 }

func (s IncidentSel) where() (string, int64) {
	if s.KpiRuleID != 0 {
		return "i.kpi_rule_id", s.KpiRuleID
	}
	return "i.monitor_id", s.MonitorID
}

// SaveRuleState persists a KPI rule's state machine snapshot.
func (st *Store) SaveRuleState(ctx context.Context, ruleID int64, state model.State, since time.Time, value *float64, at time.Time) error {
	_, err := st.DB.Exec(ctx, `UPDATE kpi_alert_rules SET state = $2, state_since = $3, last_value = $4, last_eval_at = $5 WHERE id = $1`,
		ruleID, string(state), since, value, at)
	return err
}

// MonitorStates returns the current state of monitors (composite children) in a project.
func (st *Store) MonitorStates(ctx context.Context, projectID int64, ids []int64) (map[int64]model.State, error) {
	rows, err := st.DB.Query(ctx, `SELECT id, state FROM monitors WHERE project_id = $1 AND id = ANY($2) AND status = 'active'`, projectID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]model.State{}
	for rows.Next() {
		var id int64
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			return nil, err
		}
		out[id] = model.State(s)
	}
	return out, rows.Err()
}

// LoadSecret implements services/pkg/secrets.Store (table secrets, scoped to the project). A missing row is
// pgx.ErrNoRows; any other database error is wrapped as retryable for the notifier.
func (st *Store) LoadSecret(ctx context.Context, projectID, id int64) (nonce, ciphertext []byte, err error) {
	err = st.DB.QueryRow(ctx, `SELECT nonce, ciphertext FROM secrets WHERE id = $1 AND project_id = $2`, id, projectID).Scan(&nonce, &ciphertext)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		err = &RetryableError{Err: err}
	}
	return nonce, ciphertext, err
}

// RetryableError is a transient database failure.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Transient marks the error as retryable for errors.As-based checks.
func (e *RetryableError) Transient() bool { return true }

// ───────────────────────── stream-rule hand-off between shards ─────────────────────────

// StreamHitChannel is the NOTIFY channel announcing a new alert_stream_hits row (payload = monitor id).
const StreamHitChannel = "tt_stream_hit"

// StreamHit is one alert_stream_hits row: a stream match on a monitor owned by another shard.
type StreamHit struct {
	ID        int64
	MonitorID int64
	Kind      string // breach | eval
	Value     float64
	Detail    map[string]any
	CreatedAt time.Time
}

// HandOffStreamHit records a stream hit for the monitor's owner and notifies it (NOTIFY is delivered on commit).
func (st *Store) HandOffStreamHit(ctx context.Context, monitorID int64, kind string, value float64, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO alert_stream_hits (monitor_id, kind, value, detail) VALUES ($1, $2, $3, $4)`, monitorID, kind, value, b); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, StreamHitChannel, fmt.Sprint(monitorID))
		return err
	})
}

// ClaimStreamHits deletes and returns up to limit fresh hits on monitors of the held shards (oldest first).
// Concurrent claimers never see the same row (SKIP LOCKED).
func (st *Store) ClaimStreamHits(ctx context.Context, sh Shards, maxAge time.Duration, limit int) ([]StreamHit, error) {
	cond, args := sh.filter("monitor_id", 3)
	rows, err := st.DB.Query(ctx, `DELETE FROM alert_stream_hits WHERE id IN (
			SELECT id FROM alert_stream_hits WHERE created_at > now() - make_interval(secs => $1) AND `+cond+`
			ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING id, monitor_id, kind, coalesce(value, 0), detail, created_at`, append([]any{maxAge.Seconds(), limit}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StreamHit
	for rows.Next() {
		var h StreamHit
		var detail []byte
		if err := rows.Scan(&h.ID, &h.MonitorID, &h.Kind, &h.Value, &detail, &h.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &h.Detail)
		out = append(out, h)
	}
	return out, rows.Err()
}

// PurgeStreamHits drops hits nobody claimed (owner down or monitor moved).
func (st *Store) PurgeStreamHits(ctx context.Context, maxAge time.Duration) (int64, error) {
	tag, err := st.DB.Exec(ctx, `DELETE FROM alert_stream_hits WHERE created_at <= now() - make_interval(secs => $1)`, maxAge.Seconds())
	return tag.RowsAffected(), err
}

// Deploy is the latest deploy_markers row of a service (notification deploy context).
type Deploy struct {
	Version, Source, URL, CommitMessage, Author, Env string
	TS                                               time.Time
}

// RecentDeploy returns the newest deploy of service in project within (now − window, now], or nil.
func (st *Store) RecentDeploy(ctx context.Context, projectID int64, service string, now time.Time, window time.Duration) (*Deploy, error) {
	if service == "" {
		return nil, nil
	}
	var d Deploy
	err := st.DB.QueryRow(ctx, `SELECT version, source, coalesce(url, ''), coalesce(commit_message, ''), coalesce(author, ''), env, ts
		FROM deploy_markers WHERE project_id = $1 AND service = $2 AND ts > $3 AND ts <= $4 ORDER BY ts DESC LIMIT 1`,
		projectID, service, now.Add(-window), now).Scan(&d.Version, &d.Source, &d.URL, &d.CommitMessage, &d.Author, &d.Env, &d.TS)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// EnqueueAcks writes an 'acknowledged' notification for every Slack App target (slack:<channel id>…) that received
// the firing notification of an incident acknowledged within the last day. Idempotent
// (dedupe incident:<id>:acknowledged:<channel>); the notifier answers in the firing message's thread.
func (st *Store) EnqueueAcks(ctx context.Context) (int64, error) {
	tag, err := st.DB.Exec(ctx, `INSERT INTO notifications (incident_id, channel, kind, payload, dedupe_key)
		SELECT n.incident_id, n.channel, 'acknowledged',
			n.payload || jsonb_build_object('state', 'acknowledged', 'ackedAt', (extract(epoch FROM i.acked_at) * 1000)::bigint,
				'ackedBy', coalesce(u.name, u.email, '')),
			'incident:' || n.incident_id || ':acknowledged:' || n.channel
		FROM notifications n
		JOIN incidents i ON i.id = n.incident_id
		LEFT JOIN users u ON u.id = i.acked_by
		WHERE n.kind = 'firing' AND i.state = 'acknowledged' AND i.acked_at > now() - interval '1 day'
			AND n.channel ~ '^slack:[A-Z0-9]{6,}(#|$)'
		ON CONFLICT (dedupe_key) DO NOTHING`)
	return tag.RowsAffected(), err
}
