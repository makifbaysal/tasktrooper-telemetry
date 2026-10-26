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
	"github.com/tasktrooper/telemetry/services/api/internal/rules"
	"github.com/tasktrooper/telemetry/services/api/internal/slo"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

type kpiRow struct {
	id        int64
	m         model.Kpi
	objective float64
}

const kpiCols = `id, name, coalesce(description, ''), coalesce(owner, ''), target, sli, good_when, objective, "window", source`

func scanKpi(row pgx.Row) (kpiRow, error) {
	var k kpiRow
	var target, good []byte
	var obj float32
	if err := row.Scan(&k.id, &k.m.Name, &k.m.Description, &k.m.Owner, &target, &k.m.Sli, &good, &obj, &k.m.Window, &k.m.Source); err != nil {
		return k, err
	}
	k.m.ID = strconv.FormatInt(k.id, 10)
	_ = json.Unmarshal(target, &k.m.Target)
	_ = json.Unmarshal(good, &k.m.Good)
	k.objective = float64(obj)
	k.m.Objective = k.objective
	k.m.GoodWhen = goodWhen(k.m.Sli, k.m.Target.Kind, k.m.Good)
	if k.m.Good.ThresholdMs > 0 {
		k.m.Threshold = &model.KpiThreshold{Metric: metricName(k.m.Target.Kind, k.m.Good), Value: k.m.Good.ThresholdMs}
	}
	k.m.Alerts = []model.KpiAlertRule{}
	return k, nil
}

func metricName(kind string, g model.KpiGood) string {
	if g.Metric != "" {
		return g.Metric
	}
	switch kind {
	case "page":
		return "LCP"
	case "app_start":
		return "Cold start"
	}
	return "Duration"
}

func goodWhen(sli, kind string, g model.KpiGood) string {
	if g.Description != "" {
		return g.Description
	}
	below := g.StatusBelow
	if below == 0 {
		below = 500
	}
	switch {
	case sli == "latency" && kind == "page":
		return fmt.Sprintf("Page view with %s ≤ %s and no JS error", metricName(kind, g), fmtMs(g.ThresholdMs))
	case sli == "latency" && kind == "app_start":
		return fmt.Sprintf("%s ≤ %s and no crash", metricName(kind, g), fmtMs(g.ThresholdMs))
	case sli == "latency":
		return fmt.Sprintf("Response ≤ %s and not %dxx", fmtMs(g.ThresholdMs), below/100)
	case kind == "page":
		return "Page view without a JS error"
	}
	return fmt.Sprintf("Response status below %d", below)
}

func fmtMs(v float64) string {
	if v >= 1000 {
		return strings.TrimSuffix(strconv.FormatFloat(v/1000, 'f', 1, 64), ".0") + " s"
	}
	return strconv.FormatFloat(v, 'f', -1, 64) + " ms"
}

func parseWindow(s string) time.Duration {
	if strings.HasSuffix(s, "d") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "d"))
		return time.Duration(n) * 24 * time.Hour
	}
	d, _ := time.ParseDuration(s)
	return d
}

// sliSource is how a KPI's good/total events are counted on the rollups or raw tables.
type sliSource struct {
	table, timeCol, good, total, where string
	args                               []any
	timeFn                             string // converts a unix-seconds placeholder to the column type
}

func (d *DB) kpiSource(ctx context.Context, k model.Kpi) (*sliSource, error) {
	// sli_1m wins when the alert-engine maintains it for this KPI
	var n uint64
	row := d.ch.QueryRow(ctx, `SELECT count() FROM sli_1m WHERE {{tenant}} AND kpi_id = ?`, mustUint(k.ID))
	if err := row.Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return &sliSource{table: "sli_1m", timeCol: "minute", good: "good", total: "total", where: " AND kpi_id = ?", args: []any{mustUint(k.ID)}, timeFn: "toDateTime"}, nil
	}
	g := k.Good
	below := g.StatusBelow
	if below == 0 {
		below = 500
	}
	t := k.Target
	switch t.Kind {
	case "endpoint":
		method, route, ok := strings.Cut(t.Ref, " ")
		if !ok {
			return nil, nil
		}
		if k.Sli == "latency" {
			return &sliSource{table: "spans", timeCol: "ts", timeFn: "toDateTime",
				good:  fmt.Sprintf("if(duration_ns <= %d AND status_code < %d, sample_weight, 0)", int64(g.ThresholdMs*1e6), below),
				total: "sample_weight",
				where: " AND is_entry = 1 AND kind = 'server' AND service = ? AND http_method = ? AND route = ?", args: []any{t.ServiceID, method, route}}, nil
		}
		bad := "s5xx"
		if below <= 400 {
			bad = "s4xx + s5xx"
		}
		return &sliSource{table: "red_1m", timeCol: "minute", timeFn: "toDateTime", good: "count - " + bad, total: "count",
			where: " AND service = ? AND method = ? AND route = ?", args: []any{t.ServiceID, method, route}}, nil
	case "page":
		if k.Sli == "latency" {
			return &sliSource{table: "rum_events", timeCol: "ts", timeFn: "toDateTime",
				good:  fmt.Sprintf("if(vital_value <= %g, 1, 0)", g.ThresholdMs),
				total: "1",
				where: " AND type = 'vital' AND app = ? AND view_name = ? AND upper(vital_name) = ?", args: []any{t.ServiceID, t.Ref, strings.ToUpper(metricName("page", g))}}, nil
		}
		return &sliSource{table: "rum_views_1m", timeCol: "minute", timeFn: "toDateTime", good: "views - least(views, errors)", total: "views",
			where: " AND app = ? AND view_name = ?", args: []any{t.ServiceID, t.Ref}}, nil
	case "app_start":
		return &sliSource{table: "rum_events", timeCol: "ts", timeFn: "toDateTime",
			good:  fmt.Sprintf("if(duration_ms <= %g, 1, 0)", g.ThresholdMs),
			total: "1",
			where: " AND type = 'app_start' AND app = ?", args: []any{t.ServiceID}}, nil
	}
	return nil, nil // journeys need sli_1m
}

func mustUint(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// windowCounts returns good/total for each look-back window ending now, in one query.
func (d *DB) windowCounts(ctx context.Context, src *sliSource, now time.Time, windows []time.Duration) ([]slo.Counts, error) {
	out := make([]slo.Counts, len(windows))
	if src == nil || len(windows) == 0 {
		return out, nil
	}
	longest := time.Duration(0)
	var sel []string
	var args []any
	for _, w := range windows {
		longest = max(longest, w)
		sel = append(sel, fmt.Sprintf("toFloat64(sumIf(%s, %s >= %s(?))), toFloat64(sumIf(%s, %s >= %s(?)))", src.good, src.timeCol, src.timeFn, src.total, src.timeCol, src.timeFn))
		args = append(args, now.Add(-w).Unix(), now.Add(-w).Unix())
	}
	args = append(args, now.Add(-longest).Unix())
	args = append(args, src.args...)
	row := d.ch.QueryRow(ctx, `SELECT `+strings.Join(sel, ", ")+` FROM `+src.table+` WHERE {{tenant}} AND `+src.timeCol+` >= `+src.timeFn+`(?)`+src.where, args...)
	dest := make([]any, 0, 2*len(windows))
	for i := range out {
		dest = append(dest, &out[i].Good, &out[i].Total)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Good, out[i].Total = fin(out[i].Good), fin(out[i].Total)
	}
	return out, nil
}

func (d *DB) kpiRules(ctx context.Context, ids []int64) (map[int64][]model.KpiAlertRule, error) {
	out := map[int64][]model.KpiAlertRule{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pg.Query(ctx, `SELECT r.id, r.kpi_id, r.kind, coalesce(r.long_window, ''), coalesce(r.short_window, ''), r.threshold, r.severity, r.channels,
	    r.state, r.state_since, r.last_value, r.last_eval_at
	  FROM kpi_alert_rules r JOIN kpis k ON k.id = r.kpi_id WHERE {{tenant:k}} AND r.kpi_id = ANY($1) ORDER BY r.id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r model.KpiAlertRule
		var id, kid int64
		var th float32
		var state string
		var since time.Time
		var evalAt *time.Time
		if err := rows.Scan(&id, &kid, &r.Kind, &r.LongWindow, &r.ShortWindow, &th, &r.Severity, &r.Channels,
			&state, &since, &r.LastValue, &evalAt); err != nil {
			return nil, err
		}
		r.ID, r.Threshold, r.State = strconv.FormatInt(id, 10), float64(th), "ok"
		if evalAt != nil { // engine state wins
			r.State, r.StateSince, r.LastEvalAt = state, ms(since), ms(*evalAt)
		}
		if r.Kind == "below_target" {
			r.Kind = "sli_below"
		}
		if r.Channels == nil {
			r.Channels = []string{}
		}
		r.Condition = describeKpiRule(r)
		out[kid] = append(out[kid], r)
	}
	return out, rows.Err()
}

func describeKpiRule(r model.KpiAlertRule) string {
	if r.Kind == "sli_below" {
		return fmt.Sprintf("Good events below %s%% for %s", strings.TrimSuffix(strconv.FormatFloat(r.Threshold*100, 'f', 2, 64), ".00"), r.LongWindow)
	}
	return fmt.Sprintf("Budget burns > %s× too fast over %s and %s", rules.Compact(r.Threshold), r.LongWindow, r.ShortWindow)
}

// fillKpi computes current/window ratios, burn rates, budget, status and rule states.
func (d *DB) fillKpi(ctx context.Context, k *model.Kpi, now time.Time) error {
	src, err := d.kpiSource(ctx, *k)
	if err != nil {
		return err
	}
	base := []time.Duration{time.Hour, 6 * time.Hour, parseWindow(k.Window)}
	idx := map[time.Duration]int{}
	windows := []time.Duration{}
	addW := func(w time.Duration) int {
		if i, ok := idx[w]; ok {
			return i
		}
		idx[w] = len(windows)
		windows = append(windows, w)
		return idx[w]
	}
	for _, w := range base {
		addW(w)
	}
	for _, r := range k.Alerts {
		if w := parseWindow(r.LongWindow); w > 0 {
			addW(w)
		}
		if w := parseWindow(r.ShortWindow); w > 0 {
			addW(w)
		}
	}
	counts, err := d.windowCounts(ctx, src, now, windows)
	if err != nil {
		return err
	}
	c1h, c6h, cw := counts[idx[time.Hour]], counts[idx[6*time.Hour]], counts[idx[parseWindow(k.Window)]]
	k.NoData = src == nil || cw.Total == 0
	k.Current, k.WindowGood = c1h.Ratio(), cw.Ratio()
	k.BurnRate1h, k.BurnRate6h = slo.BurnRate(c1h, k.Objective), slo.BurnRate(c6h, k.Objective)
	k.BudgetRemaining = slo.BudgetRemaining(cw, k.Objective)
	k.Status = model.Health(slo.Status(k.BudgetRemaining, k.BurnRate1h, k.BurnRate6h))
	if k.NoData {
		k.Status = model.OK
	}
	for i := range k.Alerts {
		r := &k.Alerts[i]
		lw, sw := parseWindow(r.LongWindow), parseWindow(r.ShortWindow)
		if lw == 0 || r.LastEvalAt != 0 {
			continue
		}
		long := counts[idx[lw]]
		if r.Kind == "sli_below" {
			if long.Total > 0 && long.Ratio() < r.Threshold {
				r.State = "firing"
			}
			continue
		}
		short := long
		if sw > 0 {
			short = counts[idx[sw]]
		}
		r.State = slo.RuleState(slo.BurnRate(long, k.Objective), slo.BurnRate(short, k.Objective), r.Threshold)
	}
	if !k.NoData {
		if k.Metric, err = d.kpiHeadline(ctx, *k, now); err != nil {
			return err
		}
		if k.Status != model.OK {
			if k.DegradedSince, err = d.degradedSince(ctx, *k, src, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// kpiHeadline is the KPI's headline metric: for latency KPIs the target's percentile over the last hour vs the 24 hours
// before it (endpoint p95 from red_1m, page vital p75 from rum_vitals_1m, app start p75 from rum_events); otherwise
// (and for journeys) the good-event ratio now vs over the window.
func (d *DB) kpiHeadline(ctx context.Context, k model.Kpi, now time.Time) (*model.KpiMetric, error) {
	ratio := &model.KpiMetric{Label: "Good events", Baseline: k.WindowGood, Current: k.Current, Unit: "ratio"}
	if k.Sli != "latency" {
		return ratio, nil
	}
	split, from := now.Add(-time.Hour).Unix(), now.Add(-25*time.Hour).Unix()
	t := k.Target
	var sql, label string
	var args []any
	switch t.Kind {
	case "endpoint":
		method, route, ok := strings.Cut(t.Ref, " ")
		if !ok {
			return ratio, nil
		}
		label = "p95 " + strings.ToLower(metricName(t.Kind, k.Good))
		sql = `SELECT toFloat64(quantilesTDigestWeightedMergeIf(0.95)(duration, minute < toDateTime(?))[1]), toFloat64(quantilesTDigestWeightedMergeIf(0.95)(duration, minute >= toDateTime(?))[1])
		  FROM red_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND service = ? AND method = ? AND route = ?`
		args = []any{split, split, from, t.ServiceID, method, route}
	case "page":
		vital := strings.ToUpper(metricName(t.Kind, k.Good))
		label = "p75 " + vital
		sql = `SELECT toFloat64(quantilesTDigestMergeIf(0.75)(value, minute < toDateTime(?))[1]), toFloat64(quantilesTDigestMergeIf(0.75)(value, minute >= toDateTime(?))[1])
		  FROM rum_vitals_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND app = ? AND view_name = ? AND upper(vital_name) = ?`
		args = []any{split, split, from, t.ServiceID, t.Ref, vital}
	case "app_start":
		label = "p75 " + strings.ToLower(metricName(t.Kind, k.Good))
		sql = `SELECT toFloat64(quantileIf(0.75)(duration_ms, ts < toDateTime(?))), toFloat64(quantileIf(0.75)(duration_ms, ts >= toDateTime(?)))
		  FROM rum_events WHERE {{tenant}} AND ts >= toDateTime(?) AND type = 'app_start' AND app = ?`
		args = []any{split, split, from, t.ServiceID}
	default: // journeys: only sli_1m good/total
		return ratio, nil
	}
	var base, cur float64
	if err := d.ch.QueryRow(ctx, sql, args...).Scan(&base, &cur); err != nil {
		return nil, err
	}
	return &model.KpiMetric{Label: label, Baseline: fin(base), Current: fin(cur), Unit: "ms"}, nil
}

// degradedSince is when the current degradation started: the earliest pending/firing rule's state_since (alert
// engine), otherwise the start of the trailing run of 5-minute buckets (last 24 h) whose good ratio is below the
// objective. 0 when none is found.
func (d *DB) degradedSince(ctx context.Context, k model.Kpi, src *sliSource, now time.Time) (int64, error) {
	var since int64
	for _, r := range k.Alerts {
		if (r.State == "firing" || r.State == "pending") && r.StateSince > 0 && (since == 0 || r.StateSince < since) {
			since = r.StateSince
		}
	}
	if since > 0 || src == nil {
		return since, nil
	}
	rows, err := d.ch.Query(ctx, `SELECT toInt64(toUnixTimestamp(toStartOfInterval(`+src.timeCol+`, INTERVAL 5 minute))) AS b, toFloat64(sum(`+src.good+`)), toFloat64(sum(`+src.total+`))
	  FROM `+src.table+` WHERE {{tenant}} AND `+src.timeCol+` >= `+src.timeFn+`(?)`+src.where+` GROUP BY b ORDER BY b DESC`,
		append([]any{now.Add(-24 * time.Hour).Unix()}, src.args...)...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	return DegradedRun(func(yield func(b int64, good, total float64) bool) {
		for rows.Next() {
			var b int64
			var g, t float64
			if err := rows.Scan(&b, &g, &t); err != nil || !yield(b, g, t) {
				return
			}
		}
	}, k.Objective), rows.Err()
}

// DegradedRun walks buckets newest first and returns the start (unix ms) of the trailing run whose good ratio is
// below objective; empty buckets neither extend nor break the run.
func DegradedRun(buckets func(yield func(b int64, good, total float64) bool), objective float64) int64 {
	var since int64
	buckets(func(b int64, good, total float64) bool {
		if total == 0 {
			return true
		}
		if good/total >= objective {
			return false
		}
		since = b * 1000
		return true
	})
	return since
}

// KpiFilter narrows KPI listing.
type KpiFilter struct{ ServiceID string }

// ListKpis returns KPIs with live status.
func (d *DB) ListKpis(ctx context.Context, now time.Time, f KpiFilter) ([]model.Kpi, error) {
	where, args := "", []any{}
	if f.ServiceID != "" {
		where, args = " AND target->>'serviceId' = $1", []any{f.ServiceID}
	}
	rows, err := d.pg.Query(ctx, `SELECT `+kpiCols+` FROM kpis WHERE {{tenant}}`+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	var ks []kpiRow
	for rows.Next() {
		k, err := scanKpi(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ks = append(ks, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(ks))
	for i, k := range ks {
		ids[i] = k.id
	}
	rs, err := d.kpiRules(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]model.Kpi, 0, len(ks))
	for _, k := range ks {
		if r := rs[k.id]; r != nil {
			k.m.Alerts = r
		}
		if err := d.fillKpi(ctx, &k.m, now); err != nil {
			return nil, err
		}
		out = append(out, k.m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BudgetRemaining < out[j].BudgetRemaining })
	return out, nil
}

// GetKpi returns one KPI with live status.
func (d *DB) GetKpi(ctx context.Context, now time.Time, id string) (*model.Kpi, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	k, err := scanKpi(d.pg.QueryRow(ctx, `SELECT `+kpiCols+` FROM kpis WHERE {{tenant}} AND id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rs, err := d.kpiRules(ctx, []int64{n})
	if err != nil {
		return nil, err
	}
	if rs[n] != nil {
		k.m.Alerts = rs[n]
	}
	if err := d.fillKpi(ctx, &k.m, now); err != nil {
		return nil, err
	}
	return &k.m, nil
}

func validateKpi(in *model.KpiInput) error {
	switch {
	case strings.TrimSpace(in.Name) == "":
		return Invalid("name is required")
	case in.Sli != "latency" && in.Sli != "availability" && in.Sli != "success_rate":
		return Invalid("sli must be latency|availability|success_rate")
	case in.Objective <= 0 || in.Objective >= 1:
		return Invalid("objective must be between 0 and 1 (exclusive)")
	case in.Window != "7d" && in.Window != "28d":
		return Invalid("window must be 7d|28d")
	case in.Target.ServiceID == "" || in.Target.Ref == "":
		return Invalid("target.serviceId and target.ref are required")
	case in.Sli == "latency" && in.Good.ThresholdMs <= 0:
		return Invalid("latency KPIs need good.thresholdMs")
	}
	switch in.Target.Kind {
	case "page", "endpoint", "journey", "app_start":
	default:
		return Invalid("target.kind must be page|endpoint|journey|app_start")
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	if in.Source != "manual" && in.Source != "code" && in.Source != "template" {
		return Invalid("source must be manual|code|template")
	}
	for i := range in.Alerts {
		r := &in.Alerts[i]
		if r.Kind == "sli_below" {
			r.Kind = "below_target"
		}
		if r.Kind != "burn_rate" && r.Kind != "below_target" {
			return Invalid("alert kind must be burn_rate|sli_below")
		}
		if !severitiesOK[r.Severity] {
			return Invalid("alert severity must be critical|warning|info")
		}
		if parseWindow(r.LongWindow) <= 0 {
			return Invalid("alert longWindow must be a duration like 1h, 6h, 15m")
		}
		if r.Channels == nil {
			r.Channels = []string{}
		}
	}
	return nil
}

// CreateKpi inserts a KPI and its alert rules.
func (d *DB) CreateKpi(ctx context.Context, now time.Time, in model.KpiInput) (*model.Kpi, error) {
	if err := validateKpi(&in); err != nil {
		return nil, err
	}
	target, _ := json.Marshal(in.Target)
	good, _ := json.Marshal(in.Good)
	var id int64
	if err := d.pg.QueryRow(ctx, `INSERT INTO kpis (project_id, name, description, owner, target, sli, good_when, objective, "window", source)
	  VALUES ({{project}}, $1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		in.Name, in.Description, in.Owner, target, in.Sli, good, in.Objective, in.Window, in.Source).Scan(&id); err != nil {
		return nil, err
	}
	if err := d.replaceKpiRules(ctx, id, in.Alerts); err != nil {
		return nil, err
	}
	return d.GetKpi(ctx, now, strconv.FormatInt(id, 10))
}

func (d *DB) replaceKpiRules(ctx context.Context, id int64, rs []model.KpiAlertRuleInput) error {
	if _, err := d.pg.Exec(ctx, `DELETE FROM kpi_alert_rules WHERE kpi_id IN (SELECT id FROM kpis WHERE {{tenant}} AND id = $1)`, id); err != nil {
		return err
	}
	for _, r := range rs {
		if _, err := d.pg.Exec(ctx, `INSERT INTO kpi_alert_rules (kpi_id, kind, long_window, short_window, threshold, severity, channels)
		  SELECT id, $2, $3, nullif($4, ''), $5, $6, $7 FROM kpis WHERE {{tenant}} AND id = $1`,
			id, r.Kind, r.LongWindow, r.ShortWindow, r.Threshold, r.Severity, r.Channels); err != nil {
			return err
		}
	}
	return nil
}

// UpdateKpi replaces a KPI definition and its rules.
func (d *DB) UpdateKpi(ctx context.Context, now time.Time, id string, in model.KpiInput) (*model.Kpi, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	if err := validateKpi(&in); err != nil {
		return nil, err
	}
	target, _ := json.Marshal(in.Target)
	good, _ := json.Marshal(in.Good)
	tag, err := d.pg.Exec(ctx, `UPDATE kpis SET name = $2, description = $3, owner = $4, target = $5, sli = $6, good_when = $7,
	  objective = $8, "window" = $9, source = $10 WHERE {{tenant}} AND id = $1`,
		n, in.Name, in.Description, in.Owner, target, in.Sli, good, in.Objective, in.Window, in.Source)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	if err := d.replaceKpiRules(ctx, n, in.Alerts); err != nil {
		return nil, err
	}
	return d.GetKpi(ctx, now, id)
}

// DeleteKpi removes a KPI (rules cascade).
func (d *DB) DeleteKpi(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `DELETE FROM kpis WHERE {{tenant}} AND id = $1`, n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// KpiSeries returns the good-event ratio, event count and burn rate per bucket.
func (d *DB) KpiSeries(ctx context.Context, q Q, id string) (model.SeriesSet, error) {
	k, err := d.GetKpi(ctx, q.Now, id)
	if err != nil {
		return model.SeriesSet{}, err
	}
	b := timerange.NewBuilder(q.W, "good", "total", "burnRate", "metric")
	b.Label("good", "Good events")
	b.Label("total", "Events")
	b.Label("burnRate", "Burn rate")
	b.Label("metric", "Good events")
	src, err := d.kpiSource(ctx, *k)
	if err != nil || src == nil {
		return b.Build(), err
	}
	latency, err := d.kpiMetricSeries(ctx, q, *k, b)
	if err != nil {
		return model.SeriesSet{}, err
	}
	rows, err := d.ch.Query(ctx, `SELECT `+q.W.BucketExpr(src.timeCol)+` AS b, toFloat64(sum(`+src.good+`)), toFloat64(sum(`+src.total+`)) FROM `+src.table+`
	  WHERE {{tenant}} AND `+src.timeCol+` >= `+src.timeFn+`(?) AND `+src.timeCol+` < `+src.timeFn+`(?)`+src.where+` GROUP BY b`,
		append([]any{secs(q.W.From), secs(q.W.To)}, src.args...)...)
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var bk int64
		var good, total float64
		if err := rows.Scan(&bk, &good, &total); err != nil {
			return model.SeriesSet{}, err
		}
		c := slo.Counts{Good: good, Total: total}
		b.Set("good", int(bk), c.Ratio())
		b.Set("total", int(bk), total)
		b.Set("burnRate", int(bk), slo.BurnRate(c, k.Objective))
		if !latency {
			b.Set("metric", int(bk), c.Ratio())
		}
	}
	// buckets without events count as fully good
	set := b.Build()
	for i := range set.Series[0].Data {
		if set.Series[1].Data[i] == 0 {
			set.Series[0].Data[i] = 1
			if !latency {
				set.Series[3].Data[i] = 1
			}
		}
	}
	return set, rows.Err()
}

// kpiMetricSeries fills the "metric" series with the headline value per bucket for latency KPIs (the same percentile
// kpiHeadline reports: endpoint p95, page p75 vital, app start p75) and reports whether it did; other KPIs use the
// good ratio.
func (d *DB) kpiMetricSeries(ctx context.Context, q Q, k model.Kpi, b *timerange.Builder) (bool, error) {
	if k.Sli != "latency" {
		return false, nil
	}
	t := k.Target
	var table, timeCol, timeFn, expr, where, label string
	var args []any
	switch t.Kind {
	case "endpoint":
		method, route, ok := strings.Cut(t.Ref, " ")
		if !ok {
			return false, nil
		}
		table, timeCol, timeFn, expr = "red_1m", "minute", "toDateTime", "quantilesTDigestWeightedMerge(0.95)(duration)[1]"
		where, args, label = " AND service = ? AND method = ? AND route = ?", []any{t.ServiceID, method, route}, "p95 "+strings.ToLower(metricName(t.Kind, k.Good))
	case "page":
		vital := strings.ToUpper(metricName(t.Kind, k.Good))
		table, timeCol, timeFn, expr = "rum_vitals_1m", "minute", "toDateTime", "quantilesTDigestMerge(0.75)(value)[1]"
		where, args, label = " AND app = ? AND view_name = ? AND upper(vital_name) = ?", []any{t.ServiceID, t.Ref, vital}, "p75 "+vital
	case "app_start":
		table, timeCol, timeFn, expr = "rum_events", "ts", "toDateTime", "quantile(0.75)(duration_ms)"
		where, args, label = " AND type = 'app_start' AND app = ?", []any{t.ServiceID}, "p75 "+strings.ToLower(metricName(t.Kind, k.Good))
	default:
		return false, nil
	}
	rows, err := d.ch.Query(ctx, `SELECT `+q.W.BucketExpr(timeCol)+` AS b, toFloat64(`+expr+`) FROM `+table+`
	  WHERE {{tenant}} AND `+timeCol+` >= `+timeFn+`(?) AND `+timeCol+` < `+timeFn+`(?)`+where+` GROUP BY b`,
		append([]any{secs(q.W.From), secs(q.W.To)}, args...)...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var bk int64
		var v float64
		if err := rows.Scan(&bk, &v); err != nil {
			return false, err
		}
		b.Set("metric", int(bk), fin(v))
	}
	b.Label("metric", label+" (ms)")
	return true, rows.Err()
}
