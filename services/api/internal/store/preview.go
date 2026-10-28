package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// PreviewQuery is the subset of monitors.query the log / consumer / RUM previews read. Field meanings and defaults
// follow services/alerting (internal/eval, README "Monitor query shapes").
type PreviewQuery struct {
	Service     string   `json:"service"`
	Env         string   `json:"env"`
	Metric      string   `json:"metric"`
	Severity    string   `json:"severity"`
	MinSeverity string   `json:"minSeverity"`
	PatternID   flexUint `json:"patternId"`
	Contains    string   `json:"contains"`
	Lookback    string   `json:"lookback"`
	Process     string   `json:"process"`
	Topic       string   `json:"topic"`
	Group       string   `json:"consumerGroup"`
	App         string   `json:"app"`
	View        string   `json:"view"`
	Vital       string   `json:"vital"`
	Percentile  string   `json:"percentile"`
	DeviceType  string   `json:"deviceType"`
	Per         string   `json:"per"`
}

// flexUint accepts a JSON number or a decimal string (pattern ids are UInt64).
type flexUint uint64

func (f *flexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		v, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return fmt.Errorf("patternId: %w", err)
		}
		n = uint64(v)
	}
	*f = flexUint(n)
	return nil
}

// ParseWindow reads condition.window ("5m", "1h" or minutes); default 5 minutes like the engine.
func ParseWindow(v any) time.Duration {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return time.Duration(x * float64(time.Minute))
		}
	case string:
		if d := parseWindow(x); d > 0 {
			return d
		}
		if n, err := strconv.ParseFloat(x, 64); err == nil && n > 0 {
			return time.Duration(n * float64(time.Minute))
		}
	}
	return 5 * time.Minute
}

// ParsePercentile mirrors the engine: "p95" | "95" | "0.95" → 0.95; anything else → def.
func ParsePercentile(s string, def float64) float64 {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "p")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return def
	}
	if f >= 1 {
		f /= 100
	}
	if f >= 1 {
		return def
	}
	return f
}

// monitorSeverities mirrors the engine: severity (exact) or minSeverity and above.
func monitorSeverities(q PreviewQuery) []string {
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

// WindowSum turns per-bucket counts into the engine's "count over the evaluation window" at each bucket end:
// a rolling sum of the buckets covering the window, or a pro-rated share when a bucket is longer than the window.
func WindowSum(counts []float64, stepMin, windowMin float64) []float64 {
	out := make([]float64, len(counts))
	if stepMin <= 0 || windowMin <= 0 {
		copy(out, counts)
		return out
	}
	if stepMin >= windowMin {
		for i, c := range counts {
			out[i] = c * windowMin / stepMin
		}
		return out
	}
	n := int(math.Ceil(windowMin / stepMin))
	sum := 0.0
	for i, c := range counts {
		sum += c
		if i >= n {
			sum -= counts[i-n]
		}
		out[i] = sum
	}
	return out
}

type where struct {
	sql  string
	args []any
}

func (w *where) eq(col, v string) *where {
	if v != "" {
		w.sql += " AND " + col + " = ?"
		w.args = append(w.args, v)
	}
	return w
}

func (w *where) in(col string, vs []string) *where {
	if len(vs) > 0 {
		w.sql += " AND " + col + " IN (?)"
		w.args = append(w.args, vs)
	}
	return w
}

// bucketed runs sel (one or more Float64 columns) per bucket over [w.From, w.To) and fills one series per column.
func (d *DB) bucketed(ctx context.Context, w timerange.Window, table, timeCol, timeFn, sel string, ncols int, wh *where) ([][]float64, error) {
	out := make([][]float64, ncols)
	for i := range out {
		out[i] = make([]float64, w.Points)
	}
	rows, err := d.ch.Query(ctx, `SELECT `+w.BucketExpr(timeCol)+` AS b, `+sel+` FROM `+table+`
	  WHERE {{tenant}} AND `+timeCol+` >= `+timeFn+`(?) AND `+timeCol+` < `+timeFn+`(?)`+wh.sql+` GROUP BY b`,
		append([]any{secs(w.From), secs(w.To)}, wh.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int64
		vals := make([]float64, ncols)
		dest := []any{&b}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if b >= 0 && int(b) < w.Points {
			for i, v := range vals {
				out[i][b] = fin(v)
			}
		}
	}
	return out, rows.Err()
}

// PreviewSeries returns, per bucket of the range, the value a log_count / log_match / log_pattern / log_pattern_new,
// dlq / consumer_failures, rum_vital, crash_rate or llm monitor compares (same sources and formulas as the alert engine;
// counts are summed over condition.window). volume is the per-bucket denominator used for minRequests (nil = none).
func (d *DB) PreviewSeries(ctx context.Context, q Q, kind string, t model.MonitorTarget, raw json.RawMessage, cond model.MonitorCondition) (data, volume []float64, err error) {
	var pq PreviewQuery
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &pq); err != nil {
			return nil, nil, Invalid("query: " + err.Error())
		}
	}
	w, stepMin := q.W, q.W.StepMinutes()
	winMin := ParseWindow(cond.Window).Minutes()
	svc := pq.Service
	if svc == "" {
		svc = t.Service
	}
	env := pq.Env
	if env == "" {
		env = t.Env
	}
	switch kind {
	case "llm":
		return d.llmPreview(ctx, q, t, raw, cond)
	case "log_count", "log_match", "log_pattern", "log_pattern_new":
		sev := monitorSeverities(pq)
		if (kind == "log_pattern" || kind == "log_pattern_new") && pq.PatternID == 0 {
			return d.newPatterns(ctx, q, svc, sev, pq.Lookback, winMin)
		}
		var cols [][]float64
		if pq.Contains != "" {
			wh := (&where{}).eq("service", svc).eq("env", env).in("toString(severity)", sev)
			wh.sql += " AND positionCaseInsensitive(body, ?) > 0"
			wh.args = append(wh.args, pq.Contains)
			cols, err = d.bucketed(ctx, w, "logs", "ts", "toDateTime", "toFloat64(count())", 1, wh)
		} else {
			wh := (&where{}).eq("service", svc).in("severity", sev)
			if pq.PatternID != 0 {
				wh.sql += " AND pattern_id = ?"
				wh.args = append(wh.args, uint64(pq.PatternID))
			}
			cols, err = d.bucketed(ctx, w, "log_patterns_1m", "minute", "toDateTime", "toFloat64(sum(count))", 1, wh)
		}
		if err != nil {
			return nil, nil, err
		}
		return WindowSum(cols[0], stepMin, winMin), nil, nil
	case "dlq", "consumer_failures":
		process := pq.Process
		if process == "" {
			process = t.Process
		}
		wh := (&where{}).eq("service", svc).eq("env", env).eq("process", process).eq("topic", pq.Topic).eq("consumer_group", pq.Group)
		cols, err := d.bucketed(ctx, w, "consumer_1m", "minute", "toDateTime",
			"toFloat64(sum(processed)), toFloat64(sum(failed)), toFloat64(sum(retried)), toFloat64(sum(dlq))", 4, wh)
		if err != nil {
			return nil, nil, err
		}
		processed, failed, retried, dlq := cols[0], cols[1], cols[2], cols[3]
		metric := strings.ToLower(pq.Metric)
		if metric == "" {
			metric = "dlq"
			if kind == "consumer_failures" {
				metric = "failure_rate"
			}
		}
		switch metric {
		case "failed":
			return WindowSum(failed, stepMin, winMin), nil, nil
		case "retried":
			return WindowSum(retried, stepMin, winMin), nil, nil
		case "failure_rate", "retry_rate":
			num := failed
			if metric == "retry_rate" {
				num = retried
			}
			out := make([]float64, len(num))
			for i := range out {
				out[i] = div(num[i], processed[i])
			}
			return out, processed, nil
		case "dlq":
			return WindowSum(dlq, stepMin, winMin), nil, nil
		}
		return nil, nil, Invalid("query.metric must be dlq|failed|retried|failure_rate|retry_rate")
	case "rum_vital":
		app := pq.App
		if app == "" {
			app = t.Service
		}
		view := pq.View
		if view == "" {
			view = t.View
		}
		vital := strings.ToUpper(pq.Vital)
		if vital == "" {
			vital = "LCP"
		}
		if app == "" {
			return nil, nil, Invalid("rum_vital preview needs query.app or target.service")
		}
		p := ParsePercentile(pq.Percentile, 0.75)
		wh := (&where{}).eq("app", app).eq("vital_name", vital).eq("view_name", view).eq("device_type", pq.DeviceType).eq("env", pq.Env)
		cols, err := d.bucketed(ctx, w, "rum_vitals_1m", "minute", "toDateTime",
			fmt.Sprintf("toFloat64(quantilesTDigestMerge(%g)(value)[1]), toFloat64(sum(count))", p), 2, wh)
		if err != nil {
			return nil, nil, err
		}
		return cols[0], cols[1], nil
	case "crash_rate":
		app := pq.App
		if app == "" {
			app = t.Service
		}
		view := pq.View
		if view == "" {
			view = t.View
		}
		if app == "" {
			return nil, nil, Invalid("crash_rate preview needs query.app or target.service")
		}
		wh := (&where{}).eq("app", app).eq("view_name", view).eq("env", pq.Env)
		cols, err := d.bucketed(ctx, w, "rum_views_1m", "minute", "toDateTime",
			"toFloat64(sum(crashes)), toFloat64(uniqMerge(sessions)), toFloat64(sum(views))", 3, wh)
		if err != nil {
			return nil, nil, err
		}
		den := cols[1]
		if pq.Per == "view" {
			den = cols[2]
		}
		out := make([]float64, w.Points)
		for i := range out {
			out[i] = div(cols[0][i], den[i])
		}
		return out, den, nil
	}
	return nil, nil, Invalid("no preview for kind " + strconv.Quote(kind))
}

// newPatterns counts log patterns (error/fatal by default) first seen in each bucket that did not occur during the
// lookback (default 7d) before the range, summed over the window — log_pattern without patternId.
func (d *DB) newPatterns(ctx context.Context, q Q, svc string, sev []string, lookback string, winMin float64) ([]float64, []float64, error) {
	look := parseWindow(lookback)
	if look <= 0 {
		look = 7 * 24 * time.Hour
	}
	if len(sev) == 0 {
		sev = []string{"error", "fatal"}
	}
	w := q.W
	wh := (&where{}).eq("service", svc).in("severity", sev)
	rows, err := d.ch.Query(ctx, `SELECT `+w.BucketExpr("first")+` AS b, toFloat64(count()) FROM (
	    SELECT pattern_id, min(minute) AS first FROM log_patterns_1m
	    WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+wh.sql+` GROUP BY pattern_id)
	  WHERE first >= toDateTime(?) GROUP BY b`, append(append([]any{secs(w.From.Add(-look)), secs(w.To)}, wh.args...), secs(w.From))...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	counts := make([]float64, w.Points)
	for rows.Next() {
		var b int64
		var n float64
		if err := rows.Scan(&b, &n); err != nil {
			return nil, nil, err
		}
		if b >= 0 && int(b) < w.Points {
			counts[b] = n
		}
	}
	return WindowSum(counts, w.StepMinutes(), winMin), nil, rows.Err()
}
