package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// EndpointRef identifies an endpoint (HTTP route) or a consumer process.
type EndpointRef struct {
	Method  string
	Route   string // process name when Process
	Process bool
}

// Key is "METHOD /route" (or the process name).
func (e EndpointRef) Key() string {
	if e.Process {
		return e.Route
	}
	return e.Method + " " + e.Route
}

// RedFilter narrows RED queries; empty fields match everything.
type RedFilter struct {
	Service string
	Ref     *EndpointRef
}

// ---------------------------------------------------------------- generic aggregations

type redRow struct {
	key                 []string
	count, errors, dsum float64
	s2, s3, s4, s5      float64
	q                   []float64 // p50, p75, p95, p99
	bucket              int
}

// red aggregates the RED rollup grouped by cols (+ bucket when bucketed).
func (d *DB) red(ctx context.Context, w timerange.Window, env string, cols []string, where string, args []any, bucketed bool) ([]redRow, error) {
	sel := strings.Join(cols, ", ")
	group := sel
	if bucketed {
		b := w.BucketExpr("minute") + " AS b"
		if sel == "" {
			sel, group = b, "b"
		} else {
			sel, group = sel+", "+b, group+", b"
		}
	}
	if sel != "" {
		sel += ", "
	}
	ec, ea := envCond(env, "env")
	sql := fmt.Sprintf(`SELECT %s toFloat64(sum(count)), toFloat64(sum(errors)), toFloat64(sum(duration_sum)),
	  toFloat64(sum(s2xx)), toFloat64(sum(s3xx)), toFloat64(sum(s4xx)), toFloat64(sum(s5xx)),
	  arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.75, 0.95, 0.99)(duration))
	FROM %s WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)%s%s`, sel, redTable(w), ec, where)
	if group != "" {
		sql += " GROUP BY " + group
	}
	all := append([]any{secs(w.From), secs(w.To)}, ea...)
	all = append(all, args...)
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []redRow
	for rows.Next() {
		r := redRow{key: make([]string, len(cols))}
		dest := make([]any, 0, len(cols)+9)
		for i := range cols {
			dest = append(dest, &r.key[i])
		}
		var b int64
		if bucketed {
			dest = append(dest, &b)
		}
		dest = append(dest, &r.count, &r.errors, &r.dsum, &r.s2, &r.s3, &r.s4, &r.s5, &r.q)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.bucket = int(b)
		out = append(out, r)
	}
	return out, rows.Err()
}

type consRow struct {
	key                                   []string
	processed, failed, retried, dlq, dsum float64
	q                                     []float64 // p50, p95, p99
	bucket                                int
}

func (d *DB) consumer(ctx context.Context, w timerange.Window, env string, cols []string, where string, args []any, bucketed bool) ([]consRow, error) {
	sel := strings.Join(cols, ", ")
	group := sel
	if bucketed {
		b := w.BucketExpr("minute") + " AS b"
		if sel == "" {
			sel, group = b, "b"
		} else {
			sel, group = sel+", "+b, group+", b"
		}
	}
	if sel != "" {
		sel += ", "
	}
	ec, ea := envCond(env, "env")
	sql := fmt.Sprintf(`SELECT %s toFloat64(sum(processed)), toFloat64(sum(failed)), toFloat64(sum(retried)), toFloat64(sum(dlq)),
	  toFloat64(sum(duration_sum)), arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(duration))
	FROM consumer_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)%s%s`, sel, ec, where)
	if group != "" {
		sql += " GROUP BY " + group
	}
	all := append([]any{secs(w.From), secs(w.To)}, ea...)
	all = append(all, args...)
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []consRow
	for rows.Next() {
		r := consRow{key: make([]string, len(cols))}
		dest := make([]any, 0, len(cols)+7)
		for i := range cols {
			dest = append(dest, &r.key[i])
		}
		var b int64
		if bucketed {
			dest = append(dest, &b)
		}
		dest = append(dest, &r.processed, &r.failed, &r.retried, &r.dlq, &r.dsum, &r.q)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.bucket = int(b)
		out = append(out, r)
	}
	return out, rows.Err()
}

type rumRow struct {
	key                          []string
	views, errors, crashes, tsum float64
	sessions, users              float64
	q                            []float64 // load p50, p75, p95
	bucket                       int
}

func (d *DB) rumViews(ctx context.Context, w timerange.Window, env string, cols []string, where string, args []any, bucketed bool) ([]rumRow, error) {
	sel := strings.Join(cols, ", ")
	group := sel
	if bucketed {
		b := w.BucketExpr("minute") + " AS b"
		if sel == "" {
			sel, group = b, "b"
		} else {
			sel, group = sel+", "+b, group+", b"
		}
	}
	if sel != "" {
		sel += ", "
	}
	ec, ea := envCond(env, "env")
	sql := fmt.Sprintf(`SELECT %s toFloat64(sum(views)), toFloat64(sum(errors)), toFloat64(sum(crashes)), toFloat64(sum(time_spent_sum)),
	  toFloat64(uniqMerge(sessions)), toFloat64(uniqMerge(users)),
	  arrayMap(x -> toFloat64(x), quantilesTDigestMerge(0.5, 0.75, 0.95)(load))
	FROM rum_views_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)%s%s`, sel, ec, where)
	if group != "" {
		sql += " GROUP BY " + group
	}
	all := append([]any{secs(w.From), secs(w.To)}, ea...)
	all = append(all, args...)
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rumRow
	for rows.Next() {
		r := rumRow{key: make([]string, len(cols))}
		dest := make([]any, 0, len(cols)+8)
		for i := range cols {
			dest = append(dest, &r.key[i])
		}
		var b int64
		if bucketed {
			dest = append(dest, &b)
		}
		dest = append(dest, &r.views, &r.errors, &r.crashes, &r.tsum, &r.sessions, &r.users, &r.q)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.bucket = int(b)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- services

type pgService struct{ typ, language string }

func (d *DB) pgServices(ctx context.Context) (map[string]pgService, error) {
	rows, err := d.pg.Query(ctx, `SELECT name, type, coalesce(language, '') FROM services WHERE {{tenant}}`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]pgService{}
	for rows.Next() {
		var n string
		var s pgService
		if err := rows.Scan(&n, &s.typ, &s.language); err != nil {
			return nil, err
		}
		out[n] = s
	}
	return out, rows.Err()
}

func uiType(pgType string) string {
	if pgType == "api" {
		return "http"
	}
	return pgType
}

func (d *DB) latestVersions(ctx context.Context, env string) (map[string]string, error) {
	ec, ea := envCond(env, "env")
	rows, err := d.ch.Query(ctx, `SELECT service, argMax(version, fs) FROM (
	    SELECT service, version, min(first_seen) AS fs FROM deploys WHERE {{tenant}}`+ec+` GROUP BY service, version)
	  GROUP BY service`, ea...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var s, v string
		if err := rows.Scan(&s, &v); err != nil {
			return nil, err
		}
		out[s] = v
	}
	return out, rows.Err()
}

func (d *DB) instances(ctx context.Context, q Q) (map[string]int, error) {
	rows, err := d.ch.Query(ctx, `SELECT service, toInt64(uniq(resource_attrs['service.instance.id']))
	  FROM spans WHERE {{tenant}} AND ts >= toDateTime64(?, 6) AND is_entry = 1 GROUP BY service`, q.Now.Add(-5*60e9).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = int(n)
	}
	return out, rows.Err()
}

// groupLags returns the current broker lag per consumer group (summed over clusters, topics and partitions).
func (d *DB) groupLags(ctx context.Context, q Q) (map[string]float64, error) {
	rows, err := d.ch.Query(ctx, `SELECT consumer_group, toFloat64(sum(l)) FROM (
	    SELECT cluster_id, consumer_group, topic, partition, argMax(lag, m) AS l FROM (
	      SELECT cluster_id, consumer_group, topic, partition, minute AS m, maxIf(lag, isFinite(lag)) AS lag FROM broker_lag_1m
	      WHERE {{tenant}} AND minute >= toDateTime(?) GROUP BY cluster_id, consumer_group, topic, partition, m)
	    GROUP BY cluster_id, consumer_group, topic, partition)
	  GROUP BY consumer_group`, q.Now.Add(-10*60e9).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var g string
		var v float64
		if err := rows.Scan(&g, &v); err != nil {
			return nil, err
		}
		out[g] = fin(v)
	}
	return out, rows.Err()
}

// openIncidents returns the worst open incident severity per service.
func (d *DB) openIncidents(ctx context.Context) (map[string]string, error) {
	rows, err := d.pg.Query(ctx, `SELECT coalesce(service, ''), severity, state FROM incidents WHERE {{tenant}} AND state <> 'resolved'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var s, sev, st string
		if err := rows.Scan(&s, &sev, &st); err != nil {
			return nil, err
		}
		if sev == "critical" && st == "firing" {
			out[s] = "critical"
		} else if out[s] == "" {
			out[s] = "warning"
		}
	}
	return out, rows.Err()
}

// Health derives a service/endpoint status from current vs previous error rate and p95 plus open incidents.
func Health(errRate, prevErr, p95, prevP95 float64, incident string) model.Health {
	switch {
	case incident == "critical",
		errRate >= 0.05,
		errRate >= 0.02 && errRate > 2*prevErr,
		prevP95 > 0 && p95 > 3*prevP95 && p95 > 100:
		return model.Critical
	case incident != "",
		errRate >= 0.01,
		errRate >= 0.005 && errRate > 1.5*prevErr,
		prevP95 > 0 && p95 > 1.5*prevP95 && p95 > 50:
		return model.Warning
	}
	return model.OK
}

// ListServices returns every backend service, consumer/worker and client app seen in the window or registered in Postgres.
func (d *DB) ListServices(ctx context.Context, q Q) ([]model.Service, error) {
	return d.services(ctx, q, "")
}

// GetService returns one service.
func (d *DB) GetService(ctx context.Context, q Q, id string) (*model.Service, error) {
	all, err := d.services(ctx, q, id)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, ErrNotFound
}

func (d *DB) services(ctx context.Context, q Q, only string) ([]model.Service, error) {
	var where string
	var args []any
	if only != "" {
		where, args = " AND service = ?", []any{only}
	}
	prev := q.W.Prev()
	cur, err := d.red(ctx, q.W, q.Env, []string{"service"}, where, args, false)
	if err != nil {
		return nil, err
	}
	old, err := d.red(ctx, prev, q.Env, []string{"service"}, where, args, false)
	if err != nil {
		return nil, err
	}
	spark, err := d.red(ctx, q.W, q.Env, []string{"service"}, where, args, true)
	if err != nil {
		return nil, err
	}
	ccur, err := d.consumer(ctx, q.W, q.Env, []string{"service"}, where, args, false)
	if err != nil {
		return nil, err
	}
	cold, err := d.consumer(ctx, prev, q.Env, []string{"service"}, where, args, false)
	if err != nil {
		return nil, err
	}
	cspark, err := d.consumer(ctx, q.W, q.Env, []string{"service"}, where, args, true)
	if err != nil {
		return nil, err
	}
	appWhere := strings.Replace(where, "service", "app", 1)
	rcur, err := d.rumViews(ctx, q.W, q.Env, []string{"app"}, appWhere, args, false)
	if err != nil {
		return nil, err
	}
	rold, err := d.rumViews(ctx, prev, q.Env, []string{"app"}, appWhere, args, false)
	if err != nil {
		return nil, err
	}
	rspark, err := d.rumViews(ctx, q.W, q.Env, []string{"app"}, appWhere, args, true)
	if err != nil {
		return nil, err
	}
	pgs, err := d.pgServices(ctx)
	if err != nil {
		return nil, err
	}
	versions, err := d.latestVersions(ctx, q.Env)
	if err != nil {
		return nil, err
	}
	inst, err := d.instances(ctx, q)
	if err != nil {
		return nil, err
	}
	lags, err := d.groupLags(ctx, q)
	if err != nil {
		return nil, err
	}
	groups, err := d.serviceGroups(ctx, q)
	if err != nil {
		return nil, err
	}
	incidents, err := d.openIncidents(ctx)
	if err != nil {
		return nil, err
	}
	platforms, err := d.appPlatforms(ctx, q)
	if err != nil {
		return nil, err
	}

	mins := q.W.Minutes()
	stepMin := q.W.StepMinutes()
	by := map[string]*model.Service{}
	get := func(name string) *model.Service {
		s, ok := by[name]
		if !ok {
			s = &model.Service{ID: name, Name: name, Spark: make([]float64, q.W.Points)}
			by[name] = s
		}
		return s
	}
	for _, r := range cur {
		s := get(r.key[0])
		s.Type, s.Rpm, s.ErrorRate, s.P95 = "http", r.count/mins, div(r.errors, r.count), qt(r.q, 2)
	}
	for _, r := range old {
		if s, ok := by[r.key[0]]; ok {
			s.PrevRpm, s.PrevErrorRate, s.PrevP95 = r.count/mins, div(r.errors, r.count), qt(r.q, 2)
		}
	}
	for _, r := range spark {
		if s, ok := by[r.key[0]]; ok && r.bucket >= 0 && r.bucket < q.W.Points {
			s.Spark[r.bucket] = r.count / stepMin
		}
	}
	consumers := map[string]bool{}
	for _, r := range ccur {
		consumers[r.key[0]] = true
		s := get(r.key[0])
		if s.Type == "" { // HTTP traffic wins for mixed services
			s.Type, s.Rpm, s.ErrorRate, s.P95 = "consumer", r.processed/mins, div(r.failed, r.processed), qt(r.q, 1)
		}
	}
	for _, r := range cold {
		if s, ok := by[r.key[0]]; ok && s.Type == "consumer" {
			s.PrevRpm, s.PrevErrorRate, s.PrevP95 = r.processed/mins, div(r.failed, r.processed), qt(r.q, 1)
		}
	}
	for _, r := range cspark {
		if s, ok := by[r.key[0]]; ok && s.Type == "consumer" && r.bucket >= 0 && r.bucket < q.W.Points {
			s.Spark[r.bucket] = r.processed / stepMin
		}
	}
	for _, r := range rcur {
		s := get(r.key[0])
		if s.Type == "" {
			s.Type = "web"
			if p := platforms[r.key[0]]; p != "" && p != "web" {
				s.Type = "mobile"
			}
			s.Rpm, s.P95 = r.views/mins, qt(r.q, 2)
			s.ErrorRate = div(r.errors, r.views)
		}
	}
	for _, r := range rold {
		if s, ok := by[r.key[0]]; ok && (s.Type == "web" || s.Type == "mobile") {
			s.PrevRpm, s.PrevErrorRate, s.PrevP95 = r.views/mins, div(r.errors, r.views), qt(r.q, 2)
		}
	}
	for _, r := range rspark {
		if s, ok := by[r.key[0]]; ok && (s.Type == "web" || s.Type == "mobile") && r.bucket >= 0 && r.bucket < q.W.Points {
			s.Spark[r.bucket] = r.views / stepMin
		}
	}
	for name, p := range pgs {
		if only != "" && name != only {
			continue
		}
		s := get(name)
		s.Language = p.language
		if t := uiType(p.typ); t != "" && !(t == "worker" && s.Type == "http") {
			if t == "worker" && consumers[name] && s.Type == "consumer" {
				s.Type = "worker"
			} else if s.Type == "" || t != "worker" {
				s.Type = t
			}
		}
	}
	out := make([]model.Service, 0, len(by))
	for name, s := range by {
		if s.Type == "" {
			s.Type = "http"
		}
		s.Version = versions[name]
		s.Instances = inst[name]
		if s.Type == "consumer" || s.Type == "worker" {
			total := 0.0
			for _, g := range groups[name] {
				total += lags[g]
			}
			s.Lag = &total
		}
		s.Status = Health(s.ErrorRate, s.PrevErrorRate, s.P95, s.PrevP95, incidents[name])
		s.Rpm, s.ErrorRate, s.P95 = fin(s.Rpm), fin(s.ErrorRate), fin(s.P95)
		s.PrevRpm, s.PrevErrorRate, s.PrevP95 = fin(s.PrevRpm), fin(s.PrevErrorRate), fin(s.PrevP95)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rpm != out[j].Rpm {
			return out[i].Rpm > out[j].Rpm
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// serviceGroups maps service → consumer groups it reads with (from consumer_1m and consumer_group_owners).
func (d *DB) serviceGroups(ctx context.Context, q Q) (map[string][]string, error) {
	out := map[string][]string{}
	seen := map[string]bool{}
	add := func(s, g string) {
		if g == "" || seen[s+"\x00"+g] {
			return
		}
		seen[s+"\x00"+g] = true
		out[s] = append(out[s], g)
	}
	rows, err := d.ch.Query(ctx, `SELECT DISTINCT service, consumer_group FROM consumer_1m WHERE {{tenant}} AND minute >= toDateTime(?)`, q.Now.Add(-24*3600e9).Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s, g string
		if err := rows.Scan(&s, &g); err != nil {
			rows.Close()
			return nil, err
		}
		add(s, g)
	}
	rows.Close()
	prow, err := d.pg.Query(ctx, `SELECT s.name, o.consumer_group FROM consumer_group_owners o
	  JOIN broker_clusters c ON c.id = o.cluster_id JOIN services s ON s.id = o.service_id WHERE {{tenant:c}}`)
	if err != nil {
		return nil, err
	}
	defer prow.Close()
	for prow.Next() {
		var s, g string
		if err := prow.Scan(&s, &g); err != nil {
			return nil, err
		}
		add(s, g)
	}
	return out, prow.Err()
}

func (d *DB) appPlatforms(ctx context.Context, q Q) (map[string]string, error) {
	rows, err := d.ch.Query(ctx, `SELECT app, any(platform) FROM rum_events WHERE {{tenant}} AND type = 'session_start' AND ts >= toDateTime64(?, 3) GROUP BY app`, q.W.From.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, p string
		if err := rows.Scan(&a, &p); err != nil {
			return nil, err
		}
		out[a] = p
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- RED series

// RedSeries returns rpm, errorRate, p50/p95/p99 and status-class (or retry/DLQ for processes) series.
// Service "" means the whole project.
func (d *DB) RedSeries(ctx context.Context, q Q, f RedFilter) (model.SeriesSet, error) {
	stepMin := q.W.StepMinutes()
	if f.Ref != nil && f.Ref.Process {
		b := timerange.NewBuilder(q.W, "rpm", "errorRate", "p50", "p95", "p99", "retried", "dlq")
		rows, err := d.consumer(ctx, q.W, q.Env, nil, " AND service = ? AND process = ?", []any{f.Service, f.Ref.Route}, true)
		if err != nil {
			return model.SeriesSet{}, err
		}
		for _, r := range rows {
			b.Set("rpm", r.bucket, r.processed/stepMin)
			b.Set("errorRate", r.bucket, div(r.failed, r.processed))
			b.Set("p50", r.bucket, qt(r.q, 0))
			b.Set("p95", r.bucket, qt(r.q, 1))
			b.Set("p99", r.bucket, qt(r.q, 2))
			b.Set("retried", r.bucket, r.retried/stepMin)
			b.Set("dlq", r.bucket, r.dlq/stepMin)
		}
		return labels(b, map[string]string{"rpm": "Messages / min", "errorRate": "Error rate", "retried": "Retries / min", "dlq": "DLQ / min"}), nil
	}
	b := timerange.NewBuilder(q.W, "rpm", "errorRate", "p50", "p95", "p99", "s2xx", "s3xx", "s4xx", "s5xx")
	var where string
	var args []any
	if f.Service != "" {
		where, args = " AND service = ?", []any{f.Service}
	}
	if f.Ref != nil {
		where += " AND route = ? AND method = ?"
		args = append(args, f.Ref.Route, f.Ref.Method)
	}
	rows, err := d.red(ctx, q.W, q.Env, nil, where, args, true)
	if err != nil {
		return model.SeriesSet{}, err
	}
	for _, r := range rows {
		b.Set("rpm", r.bucket, r.count/stepMin)
		b.Set("errorRate", r.bucket, div(r.errors, r.count))
		b.Set("p50", r.bucket, qt(r.q, 0))
		b.Set("p95", r.bucket, qt(r.q, 2))
		b.Set("p99", r.bucket, qt(r.q, 3))
		b.Set("s2xx", r.bucket, r.s2/stepMin)
		b.Set("s3xx", r.bucket, r.s3/stepMin)
		b.Set("s4xx", r.bucket, r.s4/stepMin)
		b.Set("s5xx", r.bucket, r.s5/stepMin)
	}
	// consumers/workers without HTTP traffic: fall back to their processed messages
	if f.Service != "" && f.Ref == nil && len(rows) == 0 {
		crow, err := d.consumer(ctx, q.W, q.Env, nil, " AND service = ?", []any{f.Service}, true)
		if err != nil {
			return model.SeriesSet{}, err
		}
		for _, r := range crow {
			b.Set("rpm", r.bucket, r.processed/stepMin)
			b.Set("errorRate", r.bucket, div(r.failed, r.processed))
			b.Set("p50", r.bucket, qt(r.q, 0))
			b.Set("p95", r.bucket, qt(r.q, 1))
			b.Set("p99", r.bucket, qt(r.q, 2))
			b.Set("s2xx", r.bucket, (r.processed-r.failed)/stepMin)
			b.Set("s5xx", r.bucket, r.failed/stepMin)
		}
		// web / mobile client apps: views per minute, views with an error, load-time percentiles (rum_views_1m)
		if len(crow) == 0 {
			vrows, err := d.rumViews(ctx, q.W, q.Env, nil, " AND app = ?", []any{f.Service}, true)
			if err != nil {
				return model.SeriesSet{}, err
			}
			for _, r := range vrows {
				b.Set("rpm", r.bucket, r.views/stepMin)
				b.Set("errorRate", r.bucket, div(r.errors, r.views))
				b.Set("p50", r.bucket, qt(r.q, 0))
				b.Set("p95", r.bucket, qt(r.q, 2))
				b.Set("p99", r.bucket, qt(r.q, 2))
				b.Set("s2xx", r.bucket, (r.views-math.Min(r.views, r.errors))/stepMin)
				b.Set("s5xx", r.bucket, math.Min(r.views, r.errors)/stepMin)
			}
			if len(vrows) > 0 {
				return labels(b, map[string]string{"rpm": "Views / min", "errorRate": "Views with an error", "p50": "Load p50", "p95": "Load p95",
					"p99": "Load p95", "s2xx": "Clean views", "s5xx": "Views with errors"}), nil
			}
		}
	}
	return labels(b, map[string]string{"rpm": "Requests / min", "errorRate": "Error rate", "s2xx": "2xx", "s3xx": "3xx", "s4xx": "4xx", "s5xx": "5xx"}), nil
}

func labels(b *timerange.Builder, l map[string]string) model.SeriesSet {
	for id, label := range l {
		b.Label(id, label)
	}
	return b.Build()
}

// ---------------------------------------------------------------- endpoints

// ListEndpoints returns HTTP endpoints (red) or consumer processes (consumer_1m) of a service.
func (d *DB) ListEndpoints(ctx context.Context, q Q, service string) ([]model.Endpoint, error) {
	mins, stepMin := q.W.Minutes(), q.W.StepMinutes()
	monitored, err := d.monitoredTargets(ctx, service)
	if err != nil {
		return nil, err
	}
	crit, err := d.criticality(ctx, service)
	if err != nil {
		return nil, err
	}
	var out []model.Endpoint
	rows, err := d.red(ctx, q.W, q.Env, []string{"method", "route"}, " AND service = ?", []any{service}, false)
	if err != nil {
		return nil, err
	}
	spark, err := d.red(ctx, q.W, q.Env, []string{"method", "route"}, " AND service = ?", []any{service}, true)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for _, r := range rows {
		m, rt := r.key[0], r.key[1]
		key := m + " " + rt
		idx[key] = len(out)
		out = append(out, model.Endpoint{
			ID: EndpointID(m, rt), ServiceID: service, Method: m, Route: rt,
			Rpm: r.count / mins, ErrorRate: div(r.errors, r.count), P50: qt(r.q, 0), P95: qt(r.q, 2), P99: qt(r.q, 3),
			Monitored: monitored[key], Criticality: critOr(crit[key]),
			Status: &model.StatusCounts{S2xx: r.s2, S3xx: r.s3, S4xx: r.s4, S5xx: r.s5},
			Spark:  make([]float64, q.W.Points),
		})
	}
	for _, r := range spark {
		if i, ok := idx[r.key[0]+" "+r.key[1]]; ok && r.bucket >= 0 && r.bucket < q.W.Points {
			out[i].Spark[r.bucket] = r.count / stepMin
		}
	}
	procs, err := d.processes(ctx, q, service, monitored, crit)
	if err != nil {
		return nil, err
	}
	out = append(out, procs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rpm > out[j].Rpm })
	return out, nil
}

func critOr(c string) string {
	if c == "" {
		return "low"
	}
	return c
}

func (d *DB) processes(ctx context.Context, q Q, service string, monitored map[string]bool, crit map[string]string) ([]model.Endpoint, error) {
	mins, stepMin := q.W.Minutes(), q.W.StepMinutes()
	rows, err := d.consumer(ctx, q.W, q.Env, []string{"process", "topic", "consumer_group", "messaging_system"}, " AND service = ?", []any{service}, false)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	spark, err := d.consumer(ctx, q.W, q.Env, []string{"process"}, " AND service = ?", []any{service}, true)
	if err != nil {
		return nil, err
	}
	lags, err := d.groupLags(ctx, q)
	if err != nil {
		return nil, err
	}
	info, err := d.groupInfo(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []model.Endpoint
	idx := map[string]int{}
	for _, r := range rows {
		proc, topic, group, system := r.key[0], r.key[1], r.key[2], r.key[3]
		if i, ok := idx[proc]; ok { // a process reading several topics: keep the busiest topic, sum counts
			e := &out[i]
			e.Rpm += r.processed / mins
			continue
		}
		gi := info[group]
		// messaging.system from the SDK span wins; else the broker cluster's kind; else kafka (pre-002 rows)
		source := BrokerKind(system)
		if source == "" {
			source = gi.kind
		}
		if source == "" {
			source = "kafka"
		}
		method := strings.ToUpper(source)
		idx[proc] = len(out)
		out = append(out, model.Endpoint{
			ID: proc, ServiceID: service, Method: method, Route: topic,
			Rpm: r.processed / mins, ErrorRate: div(r.failed, r.processed), P50: qt(r.q, 0), P95: qt(r.q, 1), P99: qt(r.q, 2),
			Monitored: monitored[proc], Criticality: critOr(crit[proc]),
			Sub: &model.Subscription{
				Process: proc, Source: source, Topic: topic, Group: group, Partitions: gi.partitions,
				Lag: lags[group], RetriesPerMin: r.retried / mins, DlqPerMin: r.dlq / mins,
			},
			Spark: make([]float64, q.W.Points),
		})
	}
	for _, r := range spark {
		if i, ok := idx[r.key[0]]; ok && r.bucket >= 0 && r.bucket < q.W.Points {
			out[i].Spark[r.bucket] = r.processed / stepMin
		}
	}
	return out, nil
}

type groupMeta struct {
	kind       string
	partitions int
}

// groupInfo returns broker kind and partition count per consumer group, from broker data + Postgres clusters.
func (d *DB) groupInfo(ctx context.Context, q Q) (map[string]groupMeta, error) {
	kinds := map[string]string{}
	prow, err := d.pg.Query(ctx, `SELECT slug, kind FROM broker_clusters WHERE {{tenant}}`)
	if err != nil {
		return nil, err
	}
	for prow.Next() {
		var s, k string
		if err := prow.Scan(&s, &k); err != nil {
			prow.Close()
			return nil, err
		}
		kinds[s] = k
	}
	prow.Close()
	rows, err := d.ch.Query(ctx, `SELECT consumer_group, any(cluster_id), toInt64(uniqExact(topic, partition))
	  FROM broker_lag_1m WHERE {{tenant}} AND minute >= toDateTime(?) GROUP BY consumer_group`, q.Now.Add(-15*60e9).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]groupMeta{}
	for rows.Next() {
		var g, c string
		var n int64
		if err := rows.Scan(&g, &c, &n); err != nil {
			return nil, err
		}
		out[g] = groupMeta{kind: kinds[c], partitions: int(n)}
	}
	return out, rows.Err()
}

// monitoredTargets returns endpoint keys / process names of a service that have an active monitor.
func (d *DB) monitoredTargets(ctx context.Context, service string) (map[string]bool, error) {
	rows, err := d.pg.Query(ctx, `SELECT coalesce(target->>'endpoint', ''), coalesce(target->>'process', '')
	  FROM monitors WHERE {{tenant}} AND status = 'active' AND target->>'service' = $1`, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var e, p string
		if err := rows.Scan(&e, &p); err != nil {
			return nil, err
		}
		if e != "" {
			out[e] = true
		}
		if p != "" {
			out[p] = true
		}
	}
	return out, rows.Err()
}

// criticality reads code-inventory criticality (meta.criticality) per endpoint key / process of a service.
func (d *DB) criticality(ctx context.Context, service string) (map[string]string, error) {
	rows, err := d.pg.Query(ctx, `SELECT i.key, coalesce(i.meta->>'criticality', '') FROM inventory_items i
	  JOIN repositories r ON r.id = i.repository_id
	  WHERE {{tenant:r}} AND i.kind IN ('endpoint', 'consumer', 'cron') AND coalesce(i.meta->>'service', '') = $1`, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, c string
		if err := rows.Scan(&k, &c); err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, rows.Err()
}

// ResolveEndpoint maps a UI endpoint id to its method/route (or process).
func (d *DB) ResolveEndpoint(ctx context.Context, q Q, service, id string) (*model.Endpoint, error) {
	eps, err := d.ListEndpoints(ctx, q, service)
	if err != nil {
		return nil, err
	}
	for i := range eps {
		if eps[i].ID == id {
			return &eps[i], nil
		}
	}
	// endpoint without traffic in the window: look further back once
	if q.W.Range != timerange.R7d {
		q2 := q
		q2.W = timerange.R7d.Window(q.Now)
		eps, err = d.ListEndpoints(ctx, q2, service)
		if err != nil {
			return nil, err
		}
		for i := range eps {
			if eps[i].ID == id {
				e := eps[i]
				e.Rpm, e.ErrorRate, e.P50, e.P95, e.P99, e.Spark, e.Status = 0, 0, 0, 0, 0, make([]float64, q.W.Points), &model.StatusCounts{}
				return &e, nil
			}
		}
	}
	return nil, ErrNotFound
}

// RefOf converts an endpoint to its query reference.
func RefOf(e *model.Endpoint) EndpointRef {
	if e.Sub != nil {
		return EndpointRef{Route: e.Sub.Process, Process: true}
	}
	return EndpointRef{Method: e.Method, Route: e.Route}
}

// ---------------------------------------------------------------- callers

var callerVia = map[string]string{
	"service": "Trace context", "web": "Browser SDK · traceparent", "mobile": "Mobile SDK · traceparent",
	"webhook": "User-Agent", "probe": "User-Agent", "unknown": "No trace context, API key or known user-agent",
}

func callerOf(name, kind string) model.Caller {
	via := callerVia[kind]
	if kind == "partner" {
		via = "API key · " + name
	}
	if kind == "" {
		kind = "unknown"
	}
	return model.Caller{ID: name, Name: name, Kind: kind, Via: via}
}

// Callers returns incoming traffic per caller for a service (ref nil) or one endpoint.
// Processes read from topics: their only "caller" is the topic, with its producers as via.
func (d *DB) Callers(ctx context.Context, q Q, service string, ref *EndpointRef) ([]model.CallerTraffic, error) {
	mins := q.W.Minutes()
	if ref != nil && ref.Process {
		return d.sourceCallers(ctx, q, service, ref.Route)
	}
	where, args := " AND service = ?", []any{service}
	if ref != nil {
		where += " AND route = ? AND method = ?"
		args = append(args, ref.Route, ref.Method)
	}
	ec, ea := envCond(q.Env, "env")
	sql := `SELECT caller, any(caller_kind), method, route, toFloat64(sum(count)), toFloat64(sum(errors)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95)(duration))
	  FROM callers_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)` + ec + where + `
	  GROUP BY caller, method, route`
	all := append(append([]any{secs(q.W.From), secs(q.W.To)}, ea...), args...)
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		c                   model.Caller
		count, errs, p95w   float64
		topCount, topErrs   float64
		topMethod, topRoute string
		topQ                []float64
	}
	by := map[string]*acc{}
	total := 0.0
	for rows.Next() {
		var caller, kind, method, route string
		var c, e float64
		var qs []float64
		if err := rows.Scan(&caller, &kind, &method, &route, &c, &e, &qs); err != nil {
			return nil, err
		}
		a, ok := by[caller]
		if !ok {
			a = &acc{c: callerOf(caller, kind)}
			by[caller] = a
		}
		a.count += c
		a.errs += e
		a.p95w += qt(qs, 1) * c
		if c > a.topCount {
			a.topCount, a.topErrs, a.topMethod, a.topRoute, a.topQ = c, e, method, route, qs
		}
		total += c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]model.CallerTraffic, 0, len(by))
	for _, a := range by {
		ct := model.CallerTraffic{Caller: a.c, Rpm: a.count / mins, Share: div(a.count, total), ErrorRate: div(a.errs, a.count), P95: div(a.p95w, a.count)}
		if ref == nil && a.topRoute != "" {
			// the caller's traffic to its busiest endpoint (callers_1m keeps p50/p95 only)
			ct.TopEndpoint = &model.Endpoint{ID: EndpointID(a.topMethod, a.topRoute), ServiceID: service, Method: a.topMethod, Route: a.topRoute,
				Criticality: "low", Rpm: a.topCount / mins, ErrorRate: div(a.topErrs, a.topCount), P50: qt(a.topQ, 0), P95: qt(a.topQ, 1), P99: qt(a.topQ, 1)}
		}
		out = append(out, ct)
	}
	if ref == nil && len(out) == 0 { // consumer services: the topics / queues they read are their callers
		return d.sourceCallers(ctx, q, service, "")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rpm > out[j].Rpm })
	return out, nil
}

// sourceCallers lists the topics / queues a consumer process (or, with process "", every process of the service) reads.
func (d *DB) sourceCallers(ctx context.Context, q Q, service, process string) ([]model.CallerTraffic, error) {
	mins := q.W.Minutes()
	where, args := " AND service = ?", []any{service}
	if process != "" {
		where, args = where+" AND process = ?", append(args, process)
	}
	rows, err := d.consumer(ctx, q.W, q.Env, []string{"topic", "consumer_group", "messaging_system"}, where, args, false)
	if err != nil {
		return nil, err
	}
	info, err := d.groupInfo(ctx, q)
	if err != nil {
		return nil, err
	}
	total := 0.0
	for _, r := range rows {
		total += r.processed
	}
	out := []model.CallerTraffic{}
	for _, r := range rows {
		topic, group := r.key[0], r.key[1]
		kind := BrokerKind(r.key[2])
		if kind == "" {
			kind = info[group].kind
		}
		ck, sys := "topic", "Kafka"
		switch kind {
		case "rabbitmq":
			ck, sys = "queue", "RabbitMQ"
		case "sqs":
			ck, sys = "queue", "Amazon SQS"
		case "pubsub":
			ck, sys = "topic", "Google Pub/Sub"
		}
		producers, err := d.producers(ctx, q, topic)
		if err != nil {
			return nil, err
		}
		via := sys + " · produced by " + strings.Join(producers, ", ")
		if len(producers) == 0 {
			via = sys + " · produced by unknown producer"
		}
		out = append(out, model.CallerTraffic{
			Caller: model.Caller{ID: "src-" + topic, Name: topic, Kind: ck, Via: via},
			Rpm:    r.processed / mins, Share: div(r.processed, total), ErrorRate: div(r.failed, r.processed), P95: qt(r.q, 1),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rpm > out[j].Rpm })
	return out, nil
}

// producers lists services that publish to a topic/queue (edges with dep "<system> · <topic>").
func (d *DB) producers(ctx context.Context, q Q, topic string) ([]string, error) {
	rows, err := d.ch.Query(ctx, `SELECT DISTINCT parent_service FROM edges_1m WHERE {{tenant}} AND dep_kind = 'queue'
	  AND minute >= toDateTime(?) AND splitByString(' · ', dep)[2] = ? ORDER BY parent_service`, q.Now.Add(-24*3600e9).Unix(), topic)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CallerSeries returns incoming rpm per caller over time.
func (d *DB) CallerSeries(ctx context.Context, q Q, service string, ref *EndpointRef) (model.SeriesSet, error) {
	b := timerange.NewBuilder(q.W)
	stepMin := q.W.StepMinutes()
	if ref != nil && ref.Process {
		rows, err := d.consumer(ctx, q.W, q.Env, []string{"topic"}, " AND service = ? AND process = ?", []any{service, ref.Route}, true)
		if err != nil {
			return model.SeriesSet{}, err
		}
		for _, r := range rows {
			b.Add("src-"+r.key[0], r.bucket, r.processed/stepMin)
			b.Label("src-"+r.key[0], r.key[0])
		}
		return b.Build(), nil
	}
	where, args := " AND service = ?", []any{service}
	if ref != nil {
		where += " AND route = ? AND method = ?"
		args = append(args, ref.Route, ref.Method)
	}
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT caller, `+q.W.BucketExpr("minute")+` AS b, toFloat64(sum(count))
	  FROM callers_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+where+` GROUP BY caller, b ORDER BY caller, b`,
		append(append([]any{secs(q.W.From), secs(q.W.To)}, ea...), args...)...)
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var bk int64
		var v float64
		if err := rows.Scan(&c, &bk, &v); err != nil {
			return model.SeriesSet{}, err
		}
		b.Set(c, int(bk), v/stepMin)
	}
	if err := rows.Err(); err != nil {
		return model.SeriesSet{}, err
	}
	set := b.Build()
	if ref == nil && len(set.Series) == 0 { // consumer services: messages per source topic
		crows, err := d.consumer(ctx, q.W, q.Env, []string{"topic"}, " AND service = ?", []any{service}, true)
		if err != nil {
			return model.SeriesSet{}, err
		}
		for _, r := range crows {
			b.Add("src-"+r.key[0], r.bucket, r.processed/stepMin)
			b.Label("src-"+r.key[0], r.key[0])
		}
		set = b.Build()
	}
	return set, nil
}

// ---------------------------------------------------------------- dependencies & time breakdown

type edgeRow struct {
	route, method, dep, kind string
	calls, errors, dsum      float64
	q                        []float64 // p50, p95
	bucket                   int
}

func (d *DB) edges(ctx context.Context, w timerange.Window, env, service string, ref *EndpointRef, groupRoute, bucketed bool, extra string, extraArgs []any) ([]edgeRow, error) {
	cols := "'' AS r, '' AS m"
	group := "dep, dep_kind"
	if groupRoute {
		cols = "parent_route AS r, parent_method AS m"
		group += ", r, m"
	}
	if bucketed {
		cols += ", " + w.BucketExpr("minute") + " AS b"
		group += ", b"
	} else {
		cols += ", toInt64(0) AS b"
	}
	where, args := " AND parent_service = ?", []any{service}
	if ref != nil {
		where += " AND parent_route = ?"
		args = append(args, ref.Route)
		if !ref.Process {
			where += " AND parent_method = ?"
			args = append(args, ref.Method)
		}
	}
	ec, ea := envCond(env, "env")
	sql := `SELECT ` + cols + `, dep, dep_kind, toFloat64(sum(calls)), toFloat64(sum(errors)), toFloat64(sum(duration_sum)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95)(duration))
	  FROM edges_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)` + ec + where + extra + ` GROUP BY ` + group
	all := append(append(append([]any{secs(w.From), secs(w.To)}, ea...), args...), extraArgs...)
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []edgeRow
	for rows.Next() {
		var e edgeRow
		var b int64
		if err := rows.Scan(&e.route, &e.method, &b, &e.dep, &e.kind, &e.calls, &e.errors, &e.dsum, &e.q); err != nil {
			return nil, err
		}
		e.bucket = int(b)
		out = append(out, e)
	}
	return out, rows.Err()
}

func depFrom(service string, e edgeRow, mins float64) model.Dependency {
	return model.Dependency{
		ID: LowerSlug(e.dep), ServiceID: service, Name: e.dep, Kind: e.kind,
		AvgMs: div(e.dsum, e.calls), P95: qt(e.q, 1), ErrorRate: div(e.errors, e.calls), CallsPerMin: e.calls / mins,
	}
}

// Dependencies returns the outgoing dependencies of a service (edges_1m).
func (d *DB) Dependencies(ctx context.Context, q Q, service string) ([]model.Dependency, error) {
	rows, err := d.edges(ctx, q.W, q.Env, service, nil, false, false, "", nil)
	if err != nil {
		return nil, err
	}
	out := make([]model.Dependency, 0, len(rows))
	for _, e := range rows {
		out = append(out, depFrom(service, e, q.W.Minutes()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CallsPerMin > out[j].CallsPerMin })
	return out, nil
}

// entryCount returns the number of requests (or processed messages) of a service / endpoint in w.
func (d *DB) entryCount(ctx context.Context, w timerange.Window, env, service string, ref *EndpointRef) (count, dsum float64, err error) {
	if ref != nil && ref.Process {
		rows, err := d.consumer(ctx, w, env, nil, " AND service = ? AND process = ?", []any{service, ref.Route}, false)
		if err != nil || len(rows) == 0 {
			return 0, 0, err
		}
		return rows[0].processed, rows[0].dsum, nil
	}
	where, args := " AND service = ?", []any{service}
	if ref != nil {
		where += " AND route = ? AND method = ?"
		args = append(args, ref.Route, ref.Method)
	}
	rows, err := d.red(ctx, w, env, nil, where, args, false)
	if err != nil {
		return 0, 0, err
	}
	if len(rows) > 0 && rows[0].count > 0 {
		return rows[0].count, rows[0].dsum, nil
	}
	if ref == nil { // consumer-only service
		crow, err := d.consumer(ctx, w, env, nil, " AND service = ?", []any{service}, false)
		if err != nil || len(crow) == 0 {
			return 0, 0, err
		}
		return crow[0].processed, crow[0].dsum, nil
	}
	return 0, 0, nil
}

// EndpointCalls returns per-request fan-out of an endpoint to its dependencies.
func (d *DB) EndpointCalls(ctx context.Context, q Q, service string, ref EndpointRef) ([]model.EndpointCall, error) {
	n, _, err := d.entryCount(ctx, q.W, q.Env, service, &ref)
	if err != nil {
		return nil, err
	}
	rows, err := d.edges(ctx, q.W, q.Env, service, &ref, false, false, "", nil)
	if err != nil {
		return nil, err
	}
	deps, err := d.Dependencies(ctx, q, service)
	if err != nil {
		return nil, err
	}
	byName := map[string]model.Dependency{}
	for _, dp := range deps {
		byName[dp.Name] = dp
	}
	out := make([]model.EndpointCall, 0, len(rows))
	for _, e := range rows {
		dep, ok := byName[e.dep]
		if !ok {
			dep = depFrom(service, e, q.W.Minutes())
		}
		out = append(out, model.EndpointCall{Dep: dep, CallsPerReq: div(e.calls, n), MsPerReq: div(e.dsum, n)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MsPerReq > out[j].MsPerReq })
	return out, nil
}

var categories = []string{"app", "db", "cache", "http", "queue"}

// Breakdown returns where request time goes (ms per request by category) for a service or endpoint.
func (d *DB) Breakdown(ctx context.Context, q Q, service string, ref *EndpointRef) (model.TimeBreakdown, error) {
	n, dsum, err := d.entryCount(ctx, q.W, q.Env, service, ref)
	if err != nil {
		return model.TimeBreakdown{}, err
	}
	rows, err := d.edges(ctx, q.W, q.Env, service, ref, false, false, "", nil)
	if err != nil {
		return model.TimeBreakdown{}, err
	}
	parts := map[string]float64{}
	for _, c := range categories {
		parts[c] = 0
	}
	avg := div(dsum, n)
	depMs := 0.0
	for _, e := range rows {
		v := div(e.dsum, n)
		if _, ok := parts[e.kind]; ok {
			parts[e.kind] += v
			depMs += v
		}
	}
	// parallel calls can make Σ dependency time exceed the request; then own code is 0 and avg follows the sum
	parts["app"] = max(0, avg-depMs)
	return model.TimeBreakdown{AvgMs: parts["app"] + depMs, Parts: parts}, nil
}

// BreakdownSeries returns ms per request by category over time.
func (d *DB) BreakdownSeries(ctx context.Context, q Q, service string, ref *EndpointRef) (model.SeriesSet, error) {
	b := timerange.NewBuilder(q.W, categories...)
	counts := make([]float64, q.W.Points)
	avg := make([]float64, q.W.Points)
	if ref != nil && ref.Process {
		rows, err := d.consumer(ctx, q.W, q.Env, nil, " AND service = ? AND process = ?", []any{service, ref.Route}, true)
		if err != nil {
			return model.SeriesSet{}, err
		}
		for _, r := range rows {
			if r.bucket >= 0 && r.bucket < q.W.Points {
				counts[r.bucket], avg[r.bucket] = r.processed, div(r.dsum, r.processed)
			}
		}
	} else {
		where, args := " AND service = ?", []any{service}
		if ref != nil {
			where += " AND route = ? AND method = ?"
			args = append(args, ref.Route, ref.Method)
		}
		rows, err := d.red(ctx, q.W, q.Env, nil, where, args, true)
		if err != nil {
			return model.SeriesSet{}, err
		}
		for _, r := range rows {
			if r.bucket >= 0 && r.bucket < q.W.Points {
				counts[r.bucket], avg[r.bucket] = r.count, div(r.dsum, r.count)
			}
		}
	}
	rows, err := d.edges(ctx, q.W, q.Env, service, ref, false, true, "", nil)
	if err != nil {
		return model.SeriesSet{}, err
	}
	dep := make([]float64, q.W.Points)
	for _, e := range rows {
		if e.bucket < 0 || e.bucket >= q.W.Points || counts[e.bucket] == 0 {
			continue
		}
		v := e.dsum / counts[e.bucket]
		b.Add(e.kind, e.bucket, v)
		dep[e.bucket] += v
	}
	for i := range avg {
		b.Set("app", i, max(0, avg[i]-dep[i]))
	}
	return labels(b, map[string]string{"app": "App code", "db": "Database", "cache": "Cache", "http": "External HTTP", "queue": "Queue publish"}), nil
}

// DependencySeries returns calls/min, error rate, avg and p95 of one dependency of a service (by id or name).
func (d *DB) DependencySeries(ctx context.Context, q Q, service, depID string) (model.SeriesSet, error) {
	deps, err := d.Dependencies(ctx, q, service)
	if err != nil {
		return model.SeriesSet{}, err
	}
	name := ""
	for _, dp := range deps {
		if dp.ID == depID || dp.Name == depID {
			name = dp.Name
		}
	}
	if name == "" {
		return model.SeriesSet{}, ErrNotFound
	}
	rows, err := d.edges(ctx, q.W, q.Env, service, nil, false, true, " AND dep = ?", []any{name})
	if err != nil {
		return model.SeriesSet{}, err
	}
	b := timerange.NewBuilder(q.W, "calls", "errorRate", "avg", "p95")
	for _, e := range rows {
		b.Add("calls", e.bucket, e.calls/q.W.StepMinutes())
		b.Set("errorRate", e.bucket, div(e.errors, e.calls))
		b.Set("avg", e.bucket, div(e.dsum, e.calls))
		b.Set("p95", e.bucket, qt(e.q, 1))
	}
	return labels(b, map[string]string{"calls": "Calls / min", "errorRate": "Error rate", "avg": "Avg ms", "p95": "p95 ms"}), nil
}
