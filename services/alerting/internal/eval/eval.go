// Package eval evaluates monitors against the ClickHouse rollups. Every query filters by org_id + project_id.
// Windows cover complete minutes: [floor(now) − window, floor(now)); broker gauges include the current minute.
package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tasktrooper/telemetry/services/alerting/internal/lag"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// ErrUnsupported is returned for kinds the engine does not evaluate; the monitor goes to no_data.
var ErrUnsupported = errors.New("eval: unsupported monitor kind")

// Result of one evaluation.
type Result struct {
	Value    float64
	NoData   bool
	Breached bool
	HeldFor  time.Duration
	Detail   map[string]any
}

// Querier is the subset of the ClickHouse connection the evaluators use.
type Querier interface {
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) driver.Row
}

// Evaluator evaluates monitors.
type Evaluator struct{ CH Querier }

// Kinds lists every kind (and UI alias) the evaluator handles.
var Kinds = []string{
	"latency", "error_rate", "throughput", "throughput_change", "apdex", "metric_threshold",
	"log_count", "log_match", "log_pattern", "log_pattern_new", "consumer_lag", "dlq", "consumer_failures",
	"rum_vital", "crash_rate", "anomaly", "job_missed", "error_new",
}

// QueueSystems maps broker_clusters.kind to messaging.system.
var QueueSystems = map[string]string{"kafka": "kafka", "rabbitmq": "rabbitmq", "sqs": "aws_sqs", "pubsub": "gcp_pubsub"}

// HasOldestAge reports whether a messaging system reports the oldest-message age gauge.
func HasOldestAge(system string) bool {
	switch system {
	case "rabbitmq", "aws_sqs", "gcp_pubsub":
		return true
	}
	return false
}

// Condition returns the condition the state machine and comparison use. consumer_lag rules carry
// threshold/forMin in the query (QueueRule); they win over monitors.condition.
func Condition(m *model.Monitor) model.Condition {
	c := m.Condition
	if m.Kind == "consumer_lag" {
		if m.Query.Threshold != nil {
			c.Threshold = *m.Query.Threshold
		}
		if m.Query.ForMin != nil {
			c.For = time.Duration(*m.Query.ForMin * float64(time.Minute))
		}
		c.Op = ">"
	}
	switch m.Kind {
	case "job_missed":
		c.Op, c.Threshold = ">", JobWindow(m).Minutes()
	case "anomaly":
		c.Op = ">"
		if c.Threshold <= 0 {
			c.Threshold = 3
		}
	case "composite":
		c.Op = ">"
	}
	return c
}

// Eval evaluates m at now.
func (e *Evaluator) Eval(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	switch m.Kind {
	case "latency":
		return e.latency(ctx, m, now)
	case "error_rate":
		return e.errorRate(ctx, m, now)
	case "throughput", "throughput_change":
		return e.throughput(ctx, m, now)
	case "apdex":
		return e.apdex(ctx, m, now)
	case "metric_threshold":
		switch strings.ToLower(m.Query.Metric) {
		case "error_rate", "er", "errors", "":
			return e.errorRate(ctx, m, now)
		case "rpm", "throughput", "requests":
			return e.throughput(ctx, m, now)
		case "apdex":
			return e.apdex(ctx, m, now)
		default: // p50, p95, latency, ...
			mm := *m
			if mm.Query.Percentile == "" {
				mm.Query.Percentile = mm.Query.Metric
			}
			return e.latency(ctx, &mm, now)
		}
	case "log_count", "log_match":
		return e.logCount(ctx, m, now)
	case "log_pattern", "log_pattern_new":
		return e.logPattern(ctx, m, now)
	case "consumer_lag":
		return e.consumerLag(ctx, m, now)
	case "dlq", "consumer_failures":
		return e.consumer(ctx, m, now)
	case "rum_vital":
		return e.rumVital(ctx, m, now)
	case "crash_rate":
		return e.crashRate(ctx, m, now)
	case "anomaly":
		return e.anomaly(ctx, m, now)
	case "job_missed":
		return e.jobMissed(ctx, m, now)
	case "error_new":
		return e.errorNew(ctx, m, now)
	}
	return Result{NoData: true}, fmt.Errorf("%w: %q", ErrUnsupported, m.Kind)
}

// ───────────────────────── helpers ─────────────────────────

type where struct {
	parts []string
	args  []any
}

func tenantWhere(m *model.Monitor) *where {
	return &where{parts: []string{"org_id = ?", "project_id = ?"}, args: []any{m.OrgID, m.ProjectID}}
}

func (w *where) eq(col, v string) *where {
	if v != "" {
		w.parts = append(w.parts, col+" = ?")
		w.args = append(w.args, v)
	}
	return w
}

func (w *where) add(cond string, args ...any) *where {
	w.parts = append(w.parts, cond)
	w.args = append(w.args, args...)
	return w
}

func (w *where) between(col string, from, to time.Time) *where {
	return w.add(col+" >= toDateTime(?) AND "+col+" < toDateTime(?)", from.Unix(), to.Unix())
}

func (w *where) sql() string { return strings.Join(w.parts, " AND ") }

// window returns [floor(now)−w, floor(now)).
func window(now time.Time, w time.Duration) (time.Time, time.Time) {
	to := now.Truncate(time.Minute)
	if w < time.Minute {
		w = time.Minute
	}
	return to.Add(-w), to
}

func compare(m *model.Monitor, v float64) Result {
	c := Condition(m)
	return Result{Value: v, Breached: c.Breached(v), Detail: map[string]any{"op": c.Op, "threshold": c.Threshold}}
}

func pct(m *model.Monitor) float64 { return ParsePercentile(m.Query.Percentile, 0.95) }

// ───────────────────────── red_1m ─────────────────────────

type redAgg struct {
	count, errors, s4xx, s5xx, durSum float64
	quantiles                         []float64
}

func (e *Evaluator) red(ctx context.Context, m *model.Monitor, from, to time.Time, levels string) (redAgg, error) {
	svc, route, method, env := m.Scope()
	w := tenantWhere(m).eq("service", svc).eq("route", route).eq("method", method).eq("env", env).between("minute", from, to)
	q := fmt.Sprintf(`SELECT sum(count), sum(errors), sum(s4xx), sum(s5xx), sum(duration_sum),
		quantilesTDigestWeightedMerge(%s)(duration) FROM telemetry.red_1m WHERE %s`, levels, w.sql())
	var a redAgg
	err := e.CH.QueryRow(ctx, q, w.args...).Scan(&a.count, &a.errors, &a.s4xx, &a.s5xx, &a.durSum, &a.quantiles)
	return a, err
}

func lowTraffic(m *model.Monitor, count float64) bool {
	return m.Condition.MinRequests > 0 && count < m.Condition.MinRequests
}

func (e *Evaluator) latency(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	p := pct(m)
	a, err := e.red(ctx, m, from, to, fmt.Sprintf("%g", p))
	if err != nil {
		return Result{}, err
	}
	if a.count == 0 || len(a.quantiles) == 0 || math.IsNaN(a.quantiles[0]) {
		return Result{NoData: true}, nil
	}
	r := compare(m, a.quantiles[0])
	r.Detail["requests"], r.Detail["percentile"] = a.count, p
	if lowTraffic(m, a.count) {
		r.Breached = false
	}
	return r, nil
}

func (e *Evaluator) errorRate(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	a, err := e.red(ctx, m, from, to, "0.5")
	if err != nil {
		return Result{}, err
	}
	if a.count == 0 {
		return Result{NoData: true}, nil
	}
	bad := a.errors
	switch {
	case m.Query.StatusMin >= 500:
		bad = a.s5xx
	case m.Query.StatusMin >= 400:
		bad = a.s4xx + a.s5xx
	}
	r := compare(m, bad/a.count)
	r.Detail["requests"], r.Detail["errors"] = a.count, bad
	if lowTraffic(m, a.count) {
		r.Breached = false
	}
	return r, nil
}

func (e *Evaluator) apdex(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	a, err := e.red(ctx, m, from, to, GridSQL)
	if err != nil {
		return Result{}, err
	}
	if a.count == 0 {
		return Result{NoData: true}, nil
	}
	t := m.Query.ApdexTMs
	if t <= 0 {
		t = 500
	}
	v := Apdex(GridLevels, a.quantiles, t, a.errors/a.count)
	if math.IsNaN(v) {
		return Result{NoData: true}, nil
	}
	c := Condition(m)
	if !c.OpSet {
		c.Op = "<" // apdex alerts when low
	}
	r := Result{Value: v, Breached: c.Breached(v), Detail: map[string]any{"op": c.Op, "threshold": c.Threshold}}
	r.Detail["requests"], r.Detail["tMs"] = a.count, t
	if lowTraffic(m, a.count) {
		r.Breached = false
	}
	return r, nil
}

// throughput: direction rpm (requests/min, default), drop (fraction below baseline), spike (fraction above),
// change (|relative change|). Baseline = same window shifted by baselineOffset (default one week: seasonal).
func (e *Evaluator) throughput(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	cur, err := e.red(ctx, m, from, to, "0.5")
	if err != nil {
		return Result{}, err
	}
	mins := to.Sub(from).Minutes()
	rpm := cur.count / mins
	dir := strings.ToLower(m.Query.Direction)
	if dir == "" && m.Kind == "throughput_change" {
		dir = "change"
	}
	if dir == "" || dir == "rpm" || dir == "absolute" {
		r := compare(m, rpm)
		r.Detail["rpm"] = rpm
		return r, nil
	}
	off, _ := model.ParseDuration(m.Query.BaselineOffset)
	if off <= 0 {
		off = 7 * 24 * time.Hour
	}
	base, err := e.red(ctx, m, from.Add(-off), to.Add(-off), "0.5")
	if err != nil {
		return Result{}, err
	}
	if base.count == 0 {
		return Result{NoData: true}, nil
	}
	baseRpm := base.count / mins
	rel := (rpm - baseRpm) / baseRpm
	var v float64
	switch dir {
	case "drop":
		v = -rel
	case "spike":
		v = rel
	default:
		v = math.Abs(rel)
	}
	v += 0 // normalise −0
	r := compare(m, v)
	r.Detail["rpm"], r.Detail["baselineRpm"] = rpm, baseRpm
	if lowTraffic(m, base.count) {
		r.Breached = false
	}
	return r, nil
}

// ───────────────────────── logs ─────────────────────────

func severities(q model.Query) []string {
	if q.Severity != "" {
		return []string{strings.ToLower(q.Severity)}
	}
	order := []string{"trace", "debug", "info", "warn", "error", "fatal"}
	for i, s := range order {
		if s == strings.ToLower(q.MinSeverity) {
			return order[i:]
		}
	}
	return nil
}

func inList(col string, vals []string) (string, []any) {
	ph := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		ph[i], args[i] = "?", v
	}
	return col + " IN (" + strings.Join(ph, ", ") + ")", args
}

func (e *Evaluator) logCount(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	svc, _, _, env := m.Scope()
	sev := severities(m.Query)
	var n uint64
	if m.Query.Contains != "" {
		// free-text match needs raw logs (tokenbf index on body)
		w := tenantWhere(m).eq("service", svc).eq("env", env).
			add("ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6)", from.Unix(), to.Unix()).
			add("positionCaseInsensitive(body, ?) > 0", m.Query.Contains)
		if len(sev) > 0 {
			c, a := inList("severity", sev)
			w.add(c, a...)
		}
		if err := e.CH.QueryRow(ctx, "SELECT count() FROM telemetry.logs WHERE "+w.sql(), w.args...).Scan(&n); err != nil {
			return Result{}, err
		}
	} else {
		w := tenantWhere(m).eq("service", svc).between("minute", from, to)
		if len(sev) > 0 {
			c, a := inList("severity", sev)
			w.add(c, a...)
		}
		if m.Query.PatternID != 0 {
			w.add("pattern_id = ?", uint64(m.Query.PatternID))
		}
		if err := e.CH.QueryRow(ctx, "SELECT sum(count) FROM telemetry.log_patterns_1m WHERE "+w.sql(), w.args...).Scan(&n); err != nil {
			return Result{}, err
		}
	}
	return compare(m, float64(n)), nil
}

// logPattern: with patternId, the pattern's count in the window; without, the number of patterns first
// seen in the window that did not appear during lookback (default 7d) — "a new error pattern appeared".
func (e *Evaluator) logPattern(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	if m.Query.PatternID != 0 {
		return e.logCount(ctx, m, now)
	}
	from, to := window(now, m.Condition.Window)
	look, _ := model.ParseDuration(m.Query.Lookback)
	if look <= 0 {
		look = 7 * 24 * time.Hour
	}
	svc, _, _, _ := m.Scope()
	sev := severities(m.Query)
	if len(sev) == 0 {
		sev = []string{"error", "fatal"}
	}
	c, a := inList("severity", sev)
	w := tenantWhere(m).eq("service", svc).add(c, a...).between("minute", from.Add(-look), to)
	q := `SELECT count(), groupArray(5)(sample) FROM (
		SELECT pattern_id, min(minute) AS first, any(sample) AS sample FROM telemetry.log_patterns_1m
		WHERE ` + w.sql() + ` GROUP BY pattern_id HAVING first >= toDateTime(?))`
	var n uint64
	var samples []string
	if err := e.CH.QueryRow(ctx, q, append(w.args, from.Unix())...).Scan(&n, &samples); err != nil {
		return Result{}, err
	}
	r := compare(m, float64(n))
	r.Detail["samples"] = samples
	return r, nil
}

// ───────────────────────── consumers ─────────────────────────

// consumer evaluates consumer_1m: metric dlq (default for kind dlq) | failed | retried | failure_rate | retry_rate.
func (e *Evaluator) consumer(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	svc, _, _, env := m.Scope()
	process := m.Query.Process
	if process == "" {
		process = m.Target.Process
	}
	w := tenantWhere(m).eq("service", svc).eq("env", env).eq("process", process).eq("topic", m.Query.Topic).
		eq("consumer_group", m.Query.ConsumerGroup).eq("messaging_system", m.Query.MessagingSystem).between("minute", from, to)
	var processed, failed, retried, dlqN float64
	q := "SELECT sum(processed), sum(failed), sum(retried), sum(dlq) FROM telemetry.consumer_1m WHERE " + w.sql()
	if err := e.CH.QueryRow(ctx, q, w.args...).Scan(&processed, &failed, &retried, &dlqN); err != nil {
		return Result{}, err
	}
	metric := strings.ToLower(m.Query.Metric)
	if metric == "" {
		metric = "dlq"
		if m.Kind == "consumer_failures" {
			metric = "failure_rate"
		}
	}
	var v float64
	switch metric {
	case "failed":
		v = failed
	case "retried":
		v = retried
	case "failure_rate", "retry_rate":
		if processed == 0 {
			return Result{NoData: true}, nil
		}
		v = failed / processed
		if metric == "retry_rate" {
			v = retried / processed
		}
	default:
		v = dlqN
	}
	r := compare(m, v)
	r.Detail["processed"] = processed
	return r, nil
}

// lagLookback is how much history a lag metric needs to compute its last point.
func lagLookback(metric lag.Metric, threshold, forMin float64) time.Duration {
	switch metric {
	case lag.Growing:
		return time.Duration(forMin+15) * time.Minute
	case lag.NotDrained:
		h := math.Max(6, 2*threshold)
		return time.Duration(math.Min(h, 7*24) * float64(time.Hour))
	}
	return time.Duration(forMin+10) * time.Minute
}

// staleAfter: a group whose last broker sample is older than this has no data (collector down).
const staleAfter = 5 * time.Minute

func (e *Evaluator) consumerLag(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	q := m.Query
	metric := lag.Metric(q.Metric)
	if !metric.Valid() {
		return Result{NoData: true}, fmt.Errorf("eval: consumer_lag metric %q", q.Metric)
	}
	group := q.Group
	if group == "" {
		group = q.ConsumerGroup
	}
	// messaging system: the cluster's kind (broker_clusters) wins; query.messagingSystem must agree with it
	system := QueueSystems[m.ClusterKind]
	if q.MessagingSystem != "" {
		if system != "" && system != q.MessagingSystem {
			return Result{NoData: true}, fmt.Errorf("%w: cluster %q is %s, monitor expects %s", ErrUnsupported, q.ClusterID, system, q.MessagingSystem)
		}
		system = q.MessagingSystem
	}
	if metric == lag.OldestAge && system != "" && !HasOldestAge(system) {
		return Result{NoData: true}, fmt.Errorf("%w: oldest_age needs a queue (sqs, pubsub, rabbitmq), cluster %q is %s", ErrUnsupported, q.ClusterID, system)
	}
	c := Condition(m)
	forMin := c.For.Minutes()
	to := now.Truncate(time.Minute).Add(time.Minute) // include the current (partial) minute: gauges
	from := to.Add(-lagLookback(metric, c.Threshold, forMin))

	type point struct{ lag, rate, maxPart, reb, age float64 }
	pts := map[int64]*point{}
	get := func(t time.Time) *point {
		k := t.Unix()
		if pts[k] == nil {
			pts[k] = &point{}
		}
		return pts[k]
	}

	w := tenantWhere(m).eq("cluster_id", q.ClusterID).eq("consumer_group", group).eq("topic", q.Topic).between("minute", from, to)
	// oldest_age_s: -Inf = not reported (contracts §5); max over queues ignores those
	rows, err := e.CH.Query(ctx, `SELECT minute, sum(l), sum(pr), max(l), max(a) FROM (
		SELECT minute, topic, partition, max(lag) AS l, max(produce_rate) AS pr,
			if(isFinite(max(oldest_age_s)), max(oldest_age_s), -1) AS a FROM telemetry.broker_lag_1m
		WHERE `+w.sql()+` GROUP BY minute, topic, partition) GROUP BY minute ORDER BY minute`, w.args...)
	if err != nil {
		return Result{}, err
	}
	var last time.Time
	for rows.Next() {
		var t time.Time
		var l, pr, mx, age float64
		if err := rows.Scan(&t, &l, &pr, &mx, &age); err != nil {
			rows.Close()
			return Result{}, err
		}
		if metric == lag.OldestAge && age < 0 {
			continue // no age reported this minute: forward-filled / stale below
		}
		p := get(t)
		p.lag, p.rate, p.maxPart, p.age = l, pr, mx, age
		if t.After(last) {
			last = t
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Result{}, err
	}

	if metric == lag.Rebalances {
		gw := tenantWhere(m).eq("cluster_id", q.ClusterID).eq("consumer_group", group).between("minute", from, to)
		rows, err := e.CH.Query(ctx, "SELECT minute, max(rebalances) FROM telemetry.broker_groups_1m WHERE "+gw.sql()+" GROUP BY minute ORDER BY minute", gw.args...)
		if err != nil {
			return Result{}, err
		}
		for rows.Next() {
			var t time.Time
			var r float64
			if err := rows.Scan(&t, &r); err != nil {
				rows.Close()
				return Result{}, err
			}
			get(t).reb = r
			if t.After(last) {
				last = t
			}
		}
		rows.Close()
	}
	if last.IsZero() || now.Sub(last) > staleAfter+time.Minute {
		return Result{NoData: true}, nil
	}

	// align to minutes, forward-filling gaps; drop leading minutes before the first sample
	var in lag.Inputs
	in.StepMin = 1
	var prev *point
	for t := from; t.Before(to); t = t.Add(time.Minute) {
		p := pts[t.Unix()]
		if p == nil {
			if prev == nil {
				continue
			}
			p = prev
		}
		in.Lag = append(in.Lag, p.lag)
		in.ProduceRate = append(in.ProduceRate, p.rate)
		in.MaxPartition = append(in.MaxPartition, p.maxPart)
		in.Rebalances = append(in.Rebalances, p.reb)
		in.OldestAge = append(in.OldestAge, p.age)
		prev = p
	}
	series, err := lag.Series(metric, in)
	if err != nil {
		return Result{}, err
	}
	th := lag.Threshold(metric, c.Threshold)
	ev := lag.Evaluate(series, th, forMin, 1)
	return Result{
		Value:    ev.Now,
		Breached: ev.Run > 0,
		HeldFor:  time.Duration(ev.Run) * time.Minute,
		Detail: map[string]any{"metric": string(metric), "threshold": th, "op": ">", "lag": in.Lag[len(in.Lag)-1], "episodes": ev.Episodes,
			"messagingSystem": system},
	}, nil
}

// ───────────────────────── RUM ─────────────────────────

func (e *Evaluator) rumVital(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	q := m.Query
	app := q.App
	if app == "" {
		app = m.Target.Service
	}
	view := q.View
	if view == "" {
		view = m.Target.View
	}
	vital := strings.ToUpper(q.Vital)
	if vital == "" {
		vital = "LCP"
	}
	p := ParsePercentile(q.Percentile, 0.75)
	w := tenantWhere(m).eq("app", app).eq("vital_name", vital).eq("view_name", view).eq("device_type", q.DeviceType).
		eq("env", q.Env).between("minute", from, to)
	var n uint64
	var qs []float64
	sql := fmt.Sprintf("SELECT sum(count), quantilesTDigestMerge(%g)(value) FROM telemetry.rum_vitals_1m WHERE %s", p, w.sql())
	if err := e.CH.QueryRow(ctx, sql, w.args...).Scan(&n, &qs); err != nil {
		return Result{}, err
	}
	if n == 0 || len(qs) == 0 || math.IsNaN(qs[0]) {
		return Result{NoData: true}, nil
	}
	r := compare(m, qs[0])
	r.Detail["samples"], r.Detail["vital"], r.Detail["percentile"] = n, vital, p
	if lowTraffic(m, float64(n)) {
		r.Breached = false
	}
	return r, nil
}

// crashRate = crashes / sessions (per: session, default) or crashes / views (per: view).
func (e *Evaluator) crashRate(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	app := m.Query.App
	if app == "" {
		app = m.Target.Service
	}
	view := m.Query.View
	if view == "" {
		view = m.Target.View
	}
	w := tenantWhere(m).eq("app", app).eq("view_name", view).eq("env", m.Query.Env).between("minute", from, to)
	var crashes, sessions, views uint64
	q := "SELECT sum(crashes), uniqMerge(sessions), sum(views) FROM telemetry.rum_views_1m WHERE " + w.sql()
	if err := e.CH.QueryRow(ctx, q, w.args...).Scan(&crashes, &sessions, &views); err != nil {
		return Result{}, err
	}
	den := float64(sessions)
	if m.Query.Per == "view" {
		den = float64(views)
	}
	if den == 0 {
		return Result{NoData: true}, nil
	}
	r := compare(m, float64(crashes)/den)
	r.Detail["crashes"], r.Detail["sessions"] = crashes, sessions
	if lowTraffic(m, den) {
		r.Breached = false
	}
	return r, nil
}

// Severities returns the log severities a query matches (nil = any): severity, or minSeverity and above.
func Severities(q model.Query) []string { return severities(q) }

// StreamKinds are the kinds that can run as stream rules (condition.stream = true).
func StreamKind(kind string) bool {
	switch kind {
	case "log_match", "log_count", "error_new":
		return true
	}
	return false
}
