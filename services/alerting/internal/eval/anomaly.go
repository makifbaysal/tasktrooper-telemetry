package eval

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// Anomaly baseline: the same minute-of-week in each of the last `weeks` weeks, widened by ±anomalySpread so
// the sample is large enough for a robust estimate (window 5m → 25 buckets per week, 100 over 4 weeks).
const (
	anomalySpread     = time.Hour
	anomalyMinSamples = 8
	anomalyMaxZ       = 1000 // |z| cap (MAD = 0 with a real deviation)
	madScale          = 1.4826
)

// Robust is the outcome of RobustZ.
type Robust struct {
	Median, MAD, Z float64
	N              int
}

// RobustZ returns the robust z-score of x against samples: (x − median) / (1.4826 · MAD). When MAD is 0 (flat
// baseline) the mean absolute deviation (× 1.2533) is used; when that is 0 too, any deviation is ±anomalyMaxZ.
func RobustZ(samples []float64, x float64) Robust {
	xs := make([]float64, 0, len(samples))
	for _, v := range samples {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			xs = append(xs, v)
		}
	}
	r := Robust{N: len(xs)}
	if r.N == 0 {
		r.Median, r.Z = math.NaN(), math.NaN()
		return r
	}
	r.Median = median(xs)
	dev := make([]float64, len(xs))
	mean := 0.0
	for i, v := range xs {
		dev[i] = math.Abs(v - r.Median)
		mean += dev[i]
	}
	mean /= float64(len(xs))
	r.MAD = median(dev)
	scale := madScale * r.MAD
	if scale == 0 {
		scale = 1.2533 * mean
	}
	d := x - r.Median
	switch {
	case d == 0:
		r.Z = 0
	case scale == 0:
		r.Z = math.Copysign(anomalyMaxZ, d)
	default:
		r.Z = math.Max(-anomalyMaxZ, math.Min(anomalyMaxZ, d/scale))
	}
	return r
}

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// AnomalyBreached applies direction (up | down | both), the z threshold and the minimum absolute deviation.
func AnomalyBreached(direction string, z, threshold, deviation, minDeviation float64) bool {
	if math.IsNaN(z) || math.Abs(deviation) < minDeviation {
		return false
	}
	switch strings.ToLower(direction) {
	case "up":
		return z > threshold
	case "down":
		return z < -threshold
	}
	return math.Abs(z) > threshold
}

// AnomalyRanges returns the [from, to) ranges whose buckets form the baseline of the window ending at `to`.
func AnomalyRanges(to time.Time, window time.Duration, weeks int) [][2]time.Time {
	var out [][2]time.Time
	for k := 1; k <= weeks; k++ {
		c := to.Add(-time.Duration(k) * 7 * 24 * time.Hour)
		out = append(out, [2]time.Time{c.Add(-anomalySpread - window), c.Add(anomalySpread)})
	}
	return out
}

type anomalyBucket struct {
	b                     int64
	count, errors, durSum float64
	q                     []float64
}

// anomaly compares the current window with the seasonal baseline (robust z-score with MAD).
// query.metric: error_rate (default) | p50 | p95 | p99 | latency | rpm; query.direction: up | down | both (default
// both); query.minDeviation in metric units; query.weeks (4); condition.threshold = z (default 3).
func (e *Evaluator) anomaly(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	w := m.Condition.Window
	if w < time.Minute {
		w = 5 * time.Minute
	}
	table, step := "red_1m", time.Minute
	if w >= time.Hour && w%time.Hour == 0 {
		table, step = "red_1h", time.Hour
	}
	to := now.Truncate(step)
	from := to.Add(-w)
	weeks := m.Query.Weeks
	if weeks <= 0 {
		weeks = 4
	}
	metric := strings.ToLower(m.Query.Metric)
	if metric == "" {
		metric = "error_rate"
	}
	level := 0.5
	switch metric {
	case "error_rate", "er", "errors", "rpm", "throughput", "requests":
	default:
		level = ParsePercentile(metric, 0.95)
		metric = "latency"
	}

	svc, route, method, env := m.Scope()
	ws := int64(w / time.Second)
	phase := to.Unix() % ws
	wh := tenantWhere(m).eq("service", svc).eq("route", route).eq("method", method).eq("env", env)
	var ors []string
	ranges := append(AnomalyRanges(to, w, weeks), [2]time.Time{from, to})
	for _, r := range ranges {
		ors = append(ors, "(minute >= toDateTime(?) AND minute < toDateTime(?))")
		wh.args = append(wh.args, r[0].Unix(), r[1].Unix())
	}
	wh.parts = append(wh.parts, "("+strings.Join(ors, " OR ")+")")
	q := fmt.Sprintf(`SELECT intDiv(toUnixTimestamp(minute) - %d, %d) AS b, sum(count), sum(errors), sum(duration_sum),
		quantilesTDigestWeightedMerge(%g)(duration) FROM telemetry.%s WHERE %s GROUP BY b ORDER BY b`, phase, ws, level, table, wh.sql())
	rows, err := e.CH.Query(ctx, q, wh.args...)
	if err != nil {
		return Result{}, err
	}
	var buckets []anomalyBucket
	for rows.Next() {
		var b anomalyBucket
		if err := rows.Scan(&b.b, &b.count, &b.errors, &b.durSum, &b.q); err != nil {
			rows.Close()
			return Result{}, err
		}
		buckets = append(buckets, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Result{}, err
	}

	curB := (from.Unix() - phase) / ws
	minCount := math.Max(1, m.Condition.MinRequests)
	val := func(b anomalyBucket) float64 {
		switch metric {
		case "error_rate", "er", "errors":
			if b.count < minCount {
				return math.NaN()
			}
			return b.errors / b.count
		case "rpm", "throughput", "requests":
			return b.count / w.Minutes()
		}
		if b.count < minCount || len(b.q) == 0 {
			return math.NaN()
		}
		return b.q[0]
	}
	cur := math.NaN()
	var curCount float64
	var samples []float64
	for _, b := range buckets {
		if b.b == curB {
			cur, curCount = val(b), b.count
			continue
		}
		samples = append(samples, val(b))
	}
	if metric == "rpm" || metric == "throughput" || metric == "requests" {
		// buckets without traffic are real zeros for throughput, not gaps
		have := map[int64]bool{}
		for _, b := range buckets {
			have[b.b] = true
		}
		for _, r := range ranges[:len(ranges)-1] {
			for t := r[0]; t.Before(r[1]); t = t.Add(w) {
				if k := (t.Unix() - phase) / ws; !have[k] && t.Add(w).Compare(r[1]) <= 0 {
					samples = append(samples, 0)
				}
			}
		}
		if math.IsNaN(cur) {
			cur = 0
		}
	}
	if math.IsNaN(cur) {
		return Result{NoData: true}, nil
	}
	rz := RobustZ(samples, cur)
	if rz.N < anomalyMinSamples {
		return Result{NoData: true, Detail: map[string]any{"reason": "insufficient baseline", "samples": rz.N}}, nil
	}
	th := m.Condition.Threshold
	if th <= 0 {
		th = 3
	}
	dir := strings.ToLower(m.Query.Direction)
	if dir == "" {
		dir = "both"
	}
	r := Result{
		Value:    rz.Z,
		Breached: AnomalyBreached(dir, rz.Z, th, cur-rz.Median, m.Query.MinDeviation),
		Detail: map[string]any{"op": ">", "threshold": th, "direction": dir, "metric": metric, "current": cur,
			"median": rz.Median, "mad": rz.MAD, "samples": rz.N, "requests": curCount},
	}
	if m.Condition.MinRequests > 0 && curCount < m.Condition.MinRequests {
		r.Breached = false
	}
	return r, nil
}
