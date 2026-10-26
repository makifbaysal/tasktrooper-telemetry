// Package kpi computes per-minute SLIs (good/total) for KPIs from the rollups, writes them to
// telemetry.sli_1m and reads burn-rate windows back from it.
package kpi

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tasktrooper/telemetry/services/alerting/internal/burn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// CH is the ClickHouse surface the package needs.
type CH interface {
	eval.Querier
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// Minute is one SLI point.
type Minute struct {
	Minute      time.Time
	Good, Total float64
}

// Agg is a per-minute aggregate from a rollup, before the good-event rule is applied.
type Agg struct {
	Minute    time.Time
	Total     float64
	Bad       float64   // errors / 5xx / failed / crashed, per the SLI
	Quantiles []float64 // GridLevels values (latency SLIs only)
}

// DefaultLatencyMs is used when a latency KPI has no threshold.
const DefaultLatencyMs = 300

// Good applies the good-event rule: availability / success_rate → total − bad; latency → non-bad events
// faster than the threshold (CDF from the t-digest grid, assumed independent of failures).
func Good(sli string, gw model.GoodWhen, a Agg) float64 {
	ok := math.Max(0, a.Total-a.Bad)
	if sli != "latency" {
		return ok
	}
	th := gw.LatencyMs
	if gw.LcpMs > 0 {
		th = gw.LcpMs
	}
	if th <= 0 {
		th = DefaultLatencyMs
	}
	f := eval.CDF(eval.GridLevels, a.Quantiles, th)
	if math.IsNaN(f) {
		return 0
	}
	return ok * f
}

// Supported reports whether the target kind can be computed from rollups.
func Supported(k *model.Kpi) bool {
	switch k.Target.Kind {
	case "endpoint", "consumer", "view", "page":
		return k.Target.Svc() != ""
	}
	return false
}

// Aggregates reads per-minute aggregates for k over [from, to).
func Aggregates(ctx context.Context, ch eval.Querier, k *model.Kpi, from, to time.Time) ([]Agg, error) {
	t := k.Target
	args := []any{k.OrgID, k.ProjectID, t.Svc(), from.Unix(), to.Unix()}
	cond := "org_id = ? AND project_id = ? AND %s = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)"
	latency := k.SLI == "latency"
	var q string
	switch t.Kind {
	case "endpoint":
		w := fmt.Sprintf(cond, "service")
		method, route := model.SplitEndpoint(t.Ref)
		if route != "" {
			w += " AND route = ?"
			args = append(args, route)
		}
		if method != "" {
			w += " AND method = ?"
			args = append(args, method)
		}
		bad := "sum(errors)"
		switch {
		case k.GoodWhen.StatusBelow > 0 && k.GoodWhen.StatusBelow <= 400:
			bad = "sum(s4xx) + sum(s5xx)"
		case k.GoodWhen.StatusBelow > 0:
			bad = "sum(s5xx)"
		}
		qs := "CAST([], 'Array(Float64)')"
		if latency {
			qs = fmt.Sprintf("quantilesTDigestWeightedMerge(%s)(duration)", eval.GridSQL)
		}
		q = fmt.Sprintf("SELECT minute, sum(count), %s, %s FROM telemetry.red_1m WHERE %s", bad, qs, w)
	case "consumer":
		w := fmt.Sprintf(cond, "service")
		if t.Ref != "" {
			w += " AND process = ?"
			args = append(args, t.Ref)
		}
		qs := "CAST([], 'Array(Float64)')"
		if latency {
			qs = fmt.Sprintf("quantilesTDigestWeightedMerge(%s)(duration)", eval.GridSQL)
		}
		q = fmt.Sprintf("SELECT minute, sum(processed), sum(failed), %s FROM telemetry.consumer_1m WHERE %s", qs, w)
	case "view", "page":
		w := fmt.Sprintf(cond, "app")
		if t.Ref != "" {
			w += " AND view_name = ?"
			args = append(args, t.Ref)
		}
		if latency && (k.GoodWhen.LcpMs > 0 || k.GoodWhen.Vital != "") {
			vital := strings.ToUpper(k.GoodWhen.Vital)
			if vital == "" {
				vital = "LCP"
			}
			w += " AND vital_name = ?"
			args = append(args, vital)
			q = fmt.Sprintf("SELECT minute, toFloat64(sum(count)), toFloat64(0), quantilesTDigestMerge(%s)(value) FROM telemetry.rum_vitals_1m WHERE %s", eval.GridSQL, w)
		} else {
			bad := "toFloat64(sum(errors))"
			if k.SLI == "success_rate" {
				bad = "toFloat64(sum(crashes))"
			}
			qs := "CAST([], 'Array(Float64)')"
			if latency {
				qs = fmt.Sprintf("quantilesTDigestMerge(%s)(load)", eval.GridSQL)
			}
			q = fmt.Sprintf("SELECT minute, toFloat64(sum(views)), %s, %s FROM telemetry.rum_views_1m WHERE %s", bad, qs, w)
		}
	default:
		return nil, fmt.Errorf("kpi: unsupported target kind %q", t.Kind)
	}
	if t.Env != "" {
		q += " AND env = ?"
		args = append(args, t.Env)
	}
	q += " GROUP BY minute ORDER BY minute"
	rows, err := ch.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agg
	for rows.Next() {
		var a Agg
		if err := rows.Scan(&a.Minute, &a.Total, &a.Bad, &a.Quantiles); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Watermark returns the last minute written to sli_1m for k (zero time when none).
func Watermark(ctx context.Context, ch eval.Querier, k *model.Kpi) (time.Time, error) {
	var n uint64
	var t time.Time
	err := ch.QueryRow(ctx, "SELECT count(), max(minute) FROM telemetry.sli_1m WHERE org_id = ? AND project_id = ? AND kpi_id = ?",
		k.OrgID, k.ProjectID, uint64(k.ID)).Scan(&n, &t)
	if err != nil || n == 0 {
		return time.Time{}, err
	}
	return t, nil
}

// Write appends SLI minutes to sli_1m. sli_1m sums on merge, so each minute must be written exactly once.
func Write(ctx context.Context, ch CH, k *model.Kpi, ms []Minute) error {
	if len(ms) == 0 {
		return nil
	}
	b, err := ch.PrepareBatch(ctx, "INSERT INTO telemetry.sli_1m (org_id, project_id, kpi_id, minute, good, total)")
	if err != nil {
		return err
	}
	for _, m := range ms {
		if err := b.Append(k.OrgID, k.ProjectID, uint64(k.ID), m.Minute, m.Good, m.Total); err != nil {
			_ = b.Abort()
			return err
		}
	}
	return b.Send()
}

// Window sums sli_1m over [to − d, to).
func Window(ctx context.Context, ch eval.Querier, k *model.Kpi, to time.Time, d time.Duration) (burn.Counts, error) {
	var c burn.Counts
	err := ch.QueryRow(ctx, `SELECT sum(good), sum(total) FROM telemetry.sli_1m
		WHERE org_id = ? AND project_id = ? AND kpi_id = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)`,
		k.OrgID, k.ProjectID, uint64(k.ID), to.Add(-d).Unix(), to.Unix()).Scan(&c.Good, &c.Total)
	return c, err
}

// ToMinutes applies the good-event rule to aggregates, dropping empty minutes.
func ToMinutes(k *model.Kpi, aggs []Agg) []Minute {
	out := make([]Minute, 0, len(aggs))
	for _, a := range aggs {
		if a.Total <= 0 {
			continue
		}
		out = append(out, Minute{Minute: a.Minute, Good: Good(k.SLI, k.GoodWhen, a), Total: a.Total})
	}
	return out
}
