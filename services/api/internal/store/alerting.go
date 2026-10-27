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
	"github.com/tasktrooper/telemetry/services/api/internal/rules"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// MonitorKinds are the UI's MonitorKind values (types.ts).
var MonitorKinds = map[string]bool{
	"metric_threshold": true, "latency": true, "throughput_change": true, "anomaly": true, "log_match": true,
	"log_pattern_new": true, "error_new": true, "consumer_lag": true, "job_missed": true, "rum_vital": true, "kpi_burn": true,
	// engine kinds (services/alerting/README.md)
	"error_rate": true, "throughput": true, "apdex": true, "log_count": true, "log_pattern": true, "dlq": true,
	"consumer_failures": true, "crash_rate": true,
}

var severitiesOK = map[string]bool{"critical": true, "warning": true, "info": true}

// MonitorFilter narrows monitor listing.
type MonitorFilter struct {
	Service   string
	Endpoint  string // "METHOD /route" or process
	Kind      string
	ClusterID string
	Group     string
}

const monitorCols = `m.id, m.name, m.kind, m.target, m.query, m.condition, m.severity, m.channels, m.source, m.status, m.state,
  m.state_since, m.last_value, coalesce(k.name, ''), m.muted_until, coalesce(m.code_ref, '')`

const monitorFrom = ` FROM monitors m LEFT JOIN kpis k ON k.id::text = m.target->>'kpiId' AND k.project_id = m.project_id`

func scanMonitor(row pgx.Row) (model.Monitor, error) {
	var m model.Monitor
	var id int64
	var target, query, cond []byte
	var source, state, kpiName string
	var since time.Time
	var muted *time.Time
	if err := row.Scan(&id, &m.Name, &m.Kind, &target, &query, &cond, &m.Severity, &m.Channels, &source, &m.Status, &state, &since, &m.LastValue, &kpiName, &muted, &m.CodeRef); err != nil {
		return m, err
	}
	m.ID = strconv.FormatInt(id, 10)
	_ = json.Unmarshal(target, &m.TargetSpec)
	_ = json.Unmarshal(cond, &m.Rule)
	m.Query = json.RawMessage(query)
	if m.Channels == nil {
		m.Channels = []string{}
	}
	m.Source = source
	if source == "suggested" { // pre-005 rows
		m.Source = "suggestion"
	}
	mutedNow := muted != nil && muted.After(time.Now())
	if mutedNow {
		m.MutedUntil = ms(*muted)
	}
	switch {
	case m.Status == "muted" || mutedNow:
		m.State = "muted"
	case state == "no_data":
		m.State = "ok"
	default:
		m.State = state
	}
	m.LastChange = ms(since)
	var meta struct {
		Metric string `json:"metric"`
	}
	_ = json.Unmarshal(query, &meta)
	if m.Kind == "consumer_lag" {
		var r model.QueueRule
		if json.Unmarshal(query, &r) == nil && r.Group != "" {
			m.Queue = &r
		}
	}
	m.Target = describeTarget(m.TargetSpec, m.Queue, kpiName)
	if m.Queue != nil {
		m.Condition = rules.DescribeQueueRule(*m.Queue)
	} else {
		m.Condition = rules.DescribeCondition(m.Kind, meta.Metric, m.Rule)
	}
	return m, nil
}

func describeTarget(t model.MonitorTarget, q *model.QueueRule, kpiName string) string {
	switch {
	case t.KpiID != "":
		if kpiName == "" {
			kpiName = t.KpiID
		}
		return "KPI · " + kpiName
	case t.Service != "" && t.Endpoint != "":
		return t.Service + " · " + t.Endpoint
	case t.Service != "" && t.Process != "":
		return t.Service + " · " + t.Process
	case t.Service != "" && t.View != "":
		return t.Service + " · " + t.View
	case t.Service != "" && q == nil:
		return t.Service
	case q != nil:
		if t.Service != "" {
			return t.Service + " · " + q.Group
		}
		return q.ClusterID + " · " + q.Group
	}
	return ""
}

// ListMonitors returns monitors matching f.
func (d *DB) ListMonitors(ctx context.Context, f MonitorFilter) ([]model.Monitor, error) {
	where := ""
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + strings.ReplaceAll(cond, "$?", "$"+strconv.Itoa(len(args)))
	}
	if f.Service != "" {
		add("m.target->>'service' = $?", f.Service)
	}
	if f.Endpoint != "" {
		add("(m.target->>'endpoint' = $? OR m.target->>'process' = $?)", f.Endpoint)
	}
	if f.Kind != "" {
		add("m.kind = $?", f.Kind)
	}
	if f.ClusterID != "" {
		add("m.query->>'clusterId' = $?", f.ClusterID)
	}
	if f.Group != "" {
		add("m.query->>'group' = $?", f.Group)
	}
	rows, err := d.pg.Query(ctx, `SELECT `+monitorCols+monitorFrom+` WHERE {{tenant:m}}`+where+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Monitor{}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func parseID(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return 0, ErrNotFound
	}
	return n, nil
}

// GetMonitor returns one monitor.
func (d *DB) GetMonitor(ctx context.Context, id string) (*model.Monitor, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	m, err := scanMonitor(d.pg.QueryRow(ctx, `SELECT `+monitorCols+monitorFrom+` WHERE {{tenant:m}} AND m.id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}

// normalizeMonitor validates in and returns the jsonb columns.
func normalizeMonitor(in *model.MonitorInput) (target, query, cond []byte, source string, err error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, nil, nil, "", Invalid("name is required")
	}
	if !MonitorKinds[in.Kind] {
		return nil, nil, nil, "", Invalid("unknown kind " + strconv.Quote(in.Kind))
	}
	if !severitiesOK[in.Severity] {
		return nil, nil, nil, "", Invalid("severity must be critical|warning|info")
	}
	if in.Status == "" {
		in.Status = "active"
	}
	if in.Status != "active" && in.Status != "muted" && in.Status != "draft" {
		return nil, nil, nil, "", Invalid("status must be active|muted|draft")
	}
	switch in.Source {
	case "": // create: manual; update: keep the stored source
	case "manual", "template":
		source = in.Source
	case "suggestion", "suggested":
		source = "suggestion"
	case "code":
		return nil, nil, nil, "", Invalid("source code is reserved for monitors managed in a repository (.trooper/monitors/*.yaml)")
	default:
		return nil, nil, nil, "", Invalid("source must be manual|template|suggestion")
	}
	if in.Kind == "consumer_lag" {
		if in.Queue == nil {
			return nil, nil, nil, "", Invalid("consumer_lag monitors need a queue rule")
		}
		r := in.Queue
		if r.ClusterID == "" || r.Group == "" || !rules.ValidLagMetric(r.Metric) {
			return nil, nil, nil, "", Invalid("queue needs clusterId, group and metric lag|time_behind|partition_lag|growing|not_drained|rebalances")
		}
		query, _ = json.Marshal(r)
		in.Condition = model.MonitorCondition{Op: ">", Threshold: rules.Threshold(*r), ForMin: r.ForMin}
	} else {
		query = in.Query
		if len(query) == 0 || string(query) == "null" {
			query = []byte("{}")
		}
		if !json.Valid(query) || query[0] != '{' {
			return nil, nil, nil, "", Invalid("query must be a JSON object")
		}
	}
	if in.Condition.Op == "" {
		in.Condition.Op = ">"
	}
	switch in.Condition.Op {
	case ">", "<", ">=", "<=":
	default:
		return nil, nil, nil, "", Invalid("condition.op must be > < >= <=")
	}
	if in.Channels == nil {
		in.Channels = []string{}
	}
	target, _ = json.Marshal(in.Target)
	cond, _ = json.Marshal(in.Condition)
	return target, query, cond, source, nil
}

// CreateMonitor inserts a monitor.
func (d *DB) CreateMonitor(ctx context.Context, in model.MonitorInput) (*model.Monitor, error) {
	target, query, cond, source, err := normalizeMonitor(&in)
	if err != nil {
		return nil, err
	}
	if source == "" {
		source = "manual"
	}
	var id int64
	if err := d.pg.QueryRow(ctx, `INSERT INTO monitors (project_id, name, kind, target, query, condition, severity, channels, source, status)
	  VALUES ({{project}}, $1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		in.Name, in.Kind, target, query, cond, in.Severity, in.Channels, source, in.Status).Scan(&id); err != nil {
		return nil, err
	}
	return d.GetMonitor(ctx, strconv.FormatInt(id, 10))
}

// UpdateMonitor replaces a monitor's definition.
func (d *DB) UpdateMonitor(ctx context.Context, id string, in model.MonitorInput) (*model.Monitor, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	target, query, cond, source, err := normalizeMonitor(&in)
	if err != nil {
		return nil, err
	}
	if err := d.ensureEditable(ctx, n); err != nil {
		return nil, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE monitors SET name = $2, kind = $3, target = $4, query = $5, condition = $6, severity = $7,
	  channels = $8, source = coalesce(nullif($9, ''), source), status = $10, updated_at = now() WHERE {{tenant}} AND id = $1 AND source <> 'code'`,
		n, in.Name, in.Kind, target, query, cond, in.Severity, in.Channels, source, in.Status)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.GetMonitor(ctx, id)
}

// ErrManagedInRepo is the 409 for changing a source = 'code' monitor.
const managedInRepo = "managed in repository: change the monitor's .trooper/monitors file instead"

// ensureEditable fails with 409 for alert-as-code monitors and 404 for unknown ones.
func (d *DB) ensureEditable(ctx context.Context, id int64) error {
	var source, ref string
	err := d.pg.QueryRow(ctx, `SELECT source, coalesce(code_ref, '') FROM monitors WHERE {{tenant}} AND id = $1`, id).Scan(&source, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if source == "code" {
		if ref != "" {
			return Conflict("managed in repository: change " + ref + " instead")
		}
		return Conflict(managedInRepo)
	}
	return nil
}

// DeleteMonitor removes a monitor (its incidents keep monitor_id = NULL).
func (d *DB) DeleteMonitor(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	if err := d.ensureEditable(ctx, n); err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `DELETE FROM monitors WHERE {{tenant}} AND id = $1 AND source <> 'code'`, n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PatchMonitor changes name / severity / channels / status / condition and mutedUntil. Setting mutedUntil bumps
// updated_at (the 003 trigger does not watch it) so the alert engine reloads the monitor.
func (d *DB) PatchMonitor(ctx context.Context, id string, p model.MonitorPatch) (*model.Monitor, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	// Alert-as-code monitors are read-only; muting (mutedUntil alone) stays possible, e.g. Slack "Mute 1h".
	if p.Name != nil || p.Severity != nil || p.Channels != nil || p.Status != nil || p.Condition != nil {
		if err := d.ensureEditable(ctx, n); err != nil {
			return nil, err
		}
	}
	sets := []string{"updated_at = now()"}
	args := []any{n}
	set := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if p.Name != nil {
		if strings.TrimSpace(*p.Name) == "" {
			return nil, Invalid("name must not be empty")
		}
		set("name", strings.TrimSpace(*p.Name))
	}
	if p.Severity != nil {
		if !severitiesOK[*p.Severity] {
			return nil, Invalid("severity must be critical|warning|info")
		}
		set("severity", *p.Severity)
	}
	if p.Channels != nil {
		set("channels", p.Channels)
	}
	if p.Status != nil {
		if *p.Status != "active" && *p.Status != "muted" && *p.Status != "draft" {
			return nil, Invalid("status must be active|muted|draft")
		}
		set("status", *p.Status)
	}
	if p.Condition != nil {
		switch p.Condition.Op {
		case "":
			p.Condition.Op = ">"
		case ">", "<", ">=", "<=":
		default:
			return nil, Invalid("condition.op must be > < >= <=")
		}
		b, _ := json.Marshal(p.Condition)
		set("condition", b)
	}
	if p.MutedUntilSet {
		var v *time.Time
		if p.MutedUntil != nil && *p.MutedUntil > 0 {
			t := time.UnixMilli(*p.MutedUntil)
			v = &t
		}
		set("muted_until", v)
	}
	tag, err := d.pg.Exec(ctx, `UPDATE monitors SET `+strings.Join(sets, ", ")+` WHERE {{tenant}} AND id = $1`, args...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.GetMonitor(ctx, id)
}

// LagInputs reads the broker values a queue rule is computed from, bucketed over w.
func (d *DB) LagInputs(ctx context.Context, w timerange.Window, r model.QueueRule) (rules.LagInputs, error) {
	in := rules.LagInputs{
		Lag: make([]float64, w.Points), MaxPartition: make([]float64, w.Points), Rebalances: make([]float64, w.Points),
		ProduceRate: make([]float64, w.Points), TimeBehind: make([]float64, w.Points), OldestAge: make([]float64, w.Points),
	}
	topic, targs := "", []any{}
	if r.Topic != "" {
		topic, targs = " AND topic = ?", []any{r.Topic}
	}
	// per minute: total lag, topic produce rate, biggest partition; per bucket: the worst minute
	rows, err := d.ch.Query(ctx, `SELECT `+w.BucketExpr("m")+` AS b, max(tl), avg(pr), max(mp), max(tl / if(pr > 0, pr, 1)), max(oa) FROM (
	    SELECT m, sum(l) AS tl, sum(p) AS pr, max(l) AS mp, max(a) AS oa FROM (
	      SELECT minute AS m, topic, partition, maxIf(lag, isFinite(lag)) AS l, maxIf(produce_rate, isFinite(produce_rate)) AS p,
	        maxIf(oldest_age_s, isFinite(oldest_age_s)) AS a FROM broker_lag_1m
	      WHERE {{tenant}} AND cluster_id = ? AND consumer_group = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)`+topic+`
	      GROUP BY m, topic, partition)
	    GROUP BY m)
	  GROUP BY b`, append([]any{r.ClusterID, r.Group, secs(w.From), secs(w.To)}, targs...)...)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var b int64
		var lag, pr, mp, tb, oa float64
		if err := rows.Scan(&b, &lag, &pr, &mp, &tb, &oa); err != nil {
			rows.Close()
			return in, err
		}
		if b >= 0 && int(b) < w.Points {
			in.Lag[b], in.ProduceRate[b], in.MaxPartition[b], in.TimeBehind[b], in.OldestAge[b] = fin(lag), fin(pr), fin(mp), fin(tb), fin(oa)
		}
	}
	rows.Close()
	grows, err := d.ch.Query(ctx, `SELECT `+w.BucketExpr("minute")+` AS b, maxIf(rebalances, isFinite(rebalances)) FROM broker_groups_1m
	  WHERE {{tenant}} AND cluster_id = ? AND consumer_group = ? AND minute >= toDateTime(?) AND minute < toDateTime(?) GROUP BY b`,
		r.ClusterID, r.Group, secs(w.From), secs(w.To))
	if err != nil {
		return in, err
	}
	defer grows.Close()
	for grows.Next() {
		var b int64
		var v float64
		if err := grows.Scan(&b, &v); err != nil {
			return in, err
		}
		if b >= 0 && int(b) < w.Points {
			in.Rebalances[b] = fin(v)
		}
	}
	return in, grows.Err()
}

// ---------------------------------------------------------------- incidents

// IncidentFilter narrows incident listing.
type IncidentFilter struct {
	State string // open (not resolved) | pending | firing | acknowledged | resolved | "" (all)
	// IncludeQuiet lists incidents resolved from pending without notifying (hidden by default).
	IncludeQuiet bool
	Service      string
	Endpoint     string
	MonitorID    string
	KpiID        string
	Since        time.Time
	Limit        int
}

const incidentCols = `i.id, coalesce(i.monitor_id::text, ''), coalesce(i.kpi_id::text, ''), i.title, i.severity, i.state, i.started_at,
  coalesce(i.service, ''), coalesce(i.endpoint, ''), i.acked_at, i.resolved_at, i.peak_value, i.root_cause,
  (SELECT e.data->>'display' FROM incident_events e WHERE e.incident_id = i.id AND e.data ? 'display' ORDER BY e.ts DESC LIMIT 1), i.quiet`

func scanIncident(row pgx.Row) (model.Incident, error) {
	var in model.Incident
	var id int64
	var started time.Time
	var acked, resolved *time.Time
	var root []byte
	var display *string
	if err := row.Scan(&id, &in.MonitorID, &in.KpiID, &in.Title, &in.Severity, &in.State, &started, &in.Service, &in.Endpoint,
		&acked, &resolved, &in.PeakValue, &root, &display, &in.Quiet); err != nil {
		return in, err
	}
	in.ID, in.StartedAt = strconv.FormatInt(id, 10), ms(started)
	if acked != nil {
		in.AckedAt = ms(*acked)
	}
	if resolved != nil {
		in.ResolvedAt = ms(*resolved)
	}
	if len(root) > 0 {
		in.RootCause = json.RawMessage(root)
	}
	switch {
	case display != nil:
		in.Value = *display
	case in.PeakValue != nil:
		in.Value = strconv.FormatFloat(*in.PeakValue, 'g', 4, 64)
	}
	return in, nil
}

// ListIncidents returns incidents newest first.
func (d *DB) ListIncidents(ctx context.Context, f IncidentFilter) ([]model.Incident, error) {
	where := ""
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + strings.ReplaceAll(cond, "$?", "$"+strconv.Itoa(len(args)))
	}
	switch f.State {
	case "":
	case "open":
		where += " AND i.state <> 'resolved'"
	case "pending", "firing", "acknowledged", "resolved":
		add("i.state = $?", f.State)
	default:
		return nil, Invalid("state must be open|pending|firing|acknowledged|resolved")
	}
	if !f.IncludeQuiet {
		where += " AND NOT i.quiet"
	}
	if f.Service != "" {
		add("i.service = $?", f.Service)
	}
	if f.Endpoint != "" {
		add("i.endpoint = $?", f.Endpoint)
	}
	if f.MonitorID != "" {
		add("i.monitor_id::text = $?", f.MonitorID)
	}
	if f.KpiID != "" {
		add("i.kpi_id::text = $?", f.KpiID)
	}
	if !f.Since.IsZero() {
		add("(i.state <> 'resolved' OR i.started_at >= $?)", f.Since)
	}
	args = append(args, clampLimit(f.Limit, 200, 1000))
	rows, err := d.pg.Query(ctx, `SELECT `+incidentCols+` FROM incidents i WHERE {{tenant:i}}`+where+
		` ORDER BY i.started_at DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Incident{}
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// GetIncident returns one incident.
func (d *DB) GetIncident(ctx context.Context, id string) (*model.Incident, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	in, err := scanIncident(d.pg.QueryRow(ctx, `SELECT `+incidentCols+` FROM incidents i WHERE {{tenant:i}} AND i.id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &in, err
}

// ErrConflict is returned for an invalid state transition.
var ErrConflict = errors.New("conflict")

// TransitionIncident acknowledges (firing → acknowledged) or resolves (any open → resolved) an incident.
func (d *DB) TransitionIncident(ctx context.Context, id, action string) (*model.Incident, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	var sql, kind string
	a := ActorOf(ctx)
	args := []any{n}
	switch action {
	case "ack":
		sql, kind = `UPDATE incidents SET state = 'acknowledged', acked_at = now(), acked_by = $2 WHERE {{tenant}} AND id = $1 AND state = 'firing'`, "acked"
		args = append(args, nullID(a.UserID))
	case "resolve":
		sql, kind = `UPDATE incidents SET state = 'resolved', resolved_at = now() WHERE {{tenant}} AND id = $1 AND state <> 'resolved'`, "resolved"
	default:
		return nil, Invalid("unknown action")
	}
	tag, err := d.pg.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := d.GetIncident(ctx, id); err != nil {
			return nil, err
		}
		return nil, ErrConflict
	}
	data, _ := json.Marshal(map[string]any{"by": a.via(), "userId": idStr(a.UserID), "email": a.Email})
	if _, err := d.pg.Exec(ctx, `INSERT INTO incident_events (incident_id, kind, data)
	  SELECT id, $2, $3::jsonb FROM incidents WHERE {{tenant}} AND id = $1`, n, kind, data); err != nil {
		return nil, err
	}
	return d.GetIncident(ctx, id)
}

// IncidentEvents returns the timeline of an incident.
func (d *DB) IncidentEvents(ctx context.Context, id string) ([]model.IncidentEvent, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	if _, err := d.GetIncident(ctx, id); err != nil {
		return nil, err
	}
	rows, err := d.pg.Query(ctx, `SELECT e.id, e.ts, e.kind, e.data FROM incident_events e JOIN incidents i ON i.id = e.incident_id
	  WHERE {{tenant:i}} AND i.id = $1 ORDER BY e.ts, e.id`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.IncidentEvent{}
	for rows.Next() {
		var e model.IncidentEvent
		var eid int64
		var ts time.Time
		var data []byte
		if err := rows.Scan(&eid, &ts, &e.Kind, &data); err != nil {
			return nil, err
		}
		e.ID, e.Ts, e.Data = strconv.FormatInt(eid, 10), ms(ts), json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}
