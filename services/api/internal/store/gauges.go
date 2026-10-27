package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// Readers for the gauge tables of deploy/clickhouse/003 (hosts_1m, containers_1m, k8s_*_1m, faas_1m): one row per
// entity and minute, SimpleAggregateFunction(max) gauges where a value that was not reported is -Inf, anyLast state
// strings. Rows of one minute may not be merged yet, so every reader first collapses (entity, minute) with max /
// anyLast, and treats non-finite values as "no data" (never returned: JSON cannot encode them).

// gaugeRow is the latest state of one entity.
type gaugeRow struct {
	keys []string  // key expressions, as strings
	v    []float64 // latest finite value per gauge column (0 when none)
	ok   []bool    // whether the gauge had a finite value at all
	s    []string  // latest value per string column
	last time.Time // newest minute with a row
}

func (g gaugeRow) val(i int) float64 { return g.v[i] }

// gaugeQuery describes a latest-state read.
type gaugeQuery struct {
	table string
	keys  []string // expressions (e.g. "host", "toString(pid)")
	cols  []string // gauge columns
	strs  []string // string columns (anyLast / key columns)
	where string   // extra " AND ..." on raw columns
	args  []any
}

// latest returns, per key, the latest finite value of every gauge column within [from, to).
func (d *DB) latest(ctx context.Context, g gaugeQuery, from, to time.Time) ([]gaugeRow, error) {
	var inner, outer, groupIn, groupOut []string
	for i, k := range g.keys {
		inner = append(inner, fmt.Sprintf("%s AS k%d", k, i))
		outer = append(outer, fmt.Sprintf("k%d", i))
		groupIn = append(groupIn, fmt.Sprintf("k%d", i))
		groupOut = append(groupOut, fmt.Sprintf("k%d", i))
	}
	inner = append(inner, "minute AS m")
	outer = append(outer, "toInt64(toUnixTimestamp(max(m)))")
	for i, c := range g.cols {
		inner = append(inner, fmt.Sprintf("max(%s) AS v%d", c, i))
		outer = append(outer, fmt.Sprintf("toFloat64(argMaxIf(v%d, m, isFinite(v%d))), toUInt8(countIf(isFinite(v%d)) > 0)", i, i, i))
	}
	for i, c := range g.strs {
		inner = append(inner, fmt.Sprintf("toString(anyLast(%s)) AS s%d", c, i))
		outer = append(outer, fmt.Sprintf("argMax(s%d, m)", i))
	}
	sql := `SELECT ` + strings.Join(outer, ", ") + ` FROM (SELECT ` + strings.Join(inner, ", ") + ` FROM ` + g.table +
		` WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)` + g.where +
		` GROUP BY ` + strings.Join(append(groupIn, "m"), ", ") + `) GROUP BY ` + strings.Join(groupOut, ", ")
	rows, err := d.ch.Query(ctx, sql, append([]any{secs(from), secs(to)}, g.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gaugeRow
	for rows.Next() {
		r := gaugeRow{keys: make([]string, len(g.keys)), v: make([]float64, len(g.cols)), ok: make([]bool, len(g.cols)), s: make([]string, len(g.strs))}
		oks := make([]uint8, len(g.cols))
		var last int64
		dst := make([]any, 0, len(g.keys)+1+2*len(g.cols)+len(g.strs))
		for i := range r.keys {
			dst = append(dst, &r.keys[i])
		}
		dst = append(dst, &last)
		for i := range r.v {
			dst = append(dst, &r.v[i], &oks[i])
		}
		for i := range r.s {
			dst = append(dst, &r.s[i])
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		for i := range r.v {
			r.v[i], r.ok[i] = fin(r.v[i]), oks[i] == 1
		}
		r.last = time.Unix(last, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// seriesQuery describes a bucketed read over a window.
type seriesQuery struct {
	table     string
	entity    []string // columns identifying one entity (collapsed per minute first)
	group     string   // optional expression splitting the result (e.g. "host"); "" = one group
	cols      []string
	where     string
	args      []any
	sumOver   bool   // per minute: sum over the entities (cluster totals); else per bucket over all rows
	perBucket string // avg | max | sum
}

// gaugeBuckets returns group → column index → per-bucket values (0 where there is no finite data).
func (d *DB) gaugeBuckets(ctx context.Context, w timerange.Window, s seriesQuery) (map[string][][]float64, error) {
	grp := s.group
	if grp == "" {
		grp = "''"
	}
	var l1 []string
	l1 = append(l1, grp+" AS g", "minute AS m")
	for i, c := range s.cols {
		l1 = append(l1, fmt.Sprintf("max(%s) AS v%d", c, i))
	}
	groupBy := append([]string{"g"}, s.entity...)
	groupBy = append(groupBy, "m")
	src := `SELECT ` + strings.Join(l1, ", ") + ` FROM ` + s.table + ` WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)` +
		s.where + ` GROUP BY ` + strings.Join(groupBy, ", ")
	if s.sumOver {
		var l2 []string
		l2 = append(l2, "g", "m")
		for i := range s.cols {
			l2 = append(l2, fmt.Sprintf("if(countIf(isFinite(v%d)) = 0, nan, sumIf(v%d, isFinite(v%d))) AS v%d", i, i, i, i))
		}
		src = `SELECT ` + strings.Join(l2, ", ") + ` FROM (` + src + `) GROUP BY g, m`
	}
	agg := s.perBucket
	if agg == "" {
		agg = "avg"
	}
	var l3 []string
	l3 = append(l3, "g", "toInt64("+w.BucketExpr("m")+") AS b")
	for i := range s.cols {
		l3 = append(l3, fmt.Sprintf("toFloat64(%sIf(v%d, isFinite(v%d)))", agg, i, i))
	}
	sql := `SELECT ` + strings.Join(l3, ", ") + ` FROM (` + src + `) GROUP BY g, b`
	rows, err := d.ch.Query(ctx, sql, append([]any{secs(w.From), secs(w.To)}, s.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][][]float64{}
	for rows.Next() {
		var g string
		var b int64
		vals := make([]float64, len(s.cols))
		dst := []any{&g, &b}
		for i := range vals {
			dst = append(dst, &vals[i])
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		cur, ok := out[g]
		if !ok {
			cur = make([][]float64, len(s.cols))
			for i := range cur {
				cur[i] = make([]float64, w.Points)
			}
			out[g] = cur
		}
		if b < 0 || int(b) >= w.Points {
			continue
		}
		for i, v := range vals {
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				cur[i][b] = v
			}
		}
	}
	return out, rows.Err()
}

// increase returns, per key, max - min of a cumulative counter over [from, to) (finite values only).
func (d *DB) increase(ctx context.Context, table, key, col, where string, args []any, from, to time.Time) (map[string]float64, error) {
	rows, err := d.ch.Query(ctx, `SELECT toString(`+key+`), toFloat64(maxIf(`+col+`, isFinite(`+col+`))), toFloat64(minIf(`+col+`, isFinite(`+col+`))), countIf(isFinite(`+col+`))
	  FROM `+table+` WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+where+` GROUP BY 1`,
		append([]any{secs(from), secs(to)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var k string
		var hi, lo float64
		var n uint64
		if err := rows.Scan(&k, &hi, &lo, &n); err != nil {
			return nil, err
		}
		if n > 0 {
			out[k] = math.Max(0, fin(hi-lo))
		}
	}
	return out, rows.Err()
}

// set builds a SeriesSet from per-bucket columns (ids[i] ↔ cols[i]); nil columns are zero-filled.
func set(w timerange.Window, ids []string, cols [][]float64) timerange.Set {
	b := timerange.NewBuilder(w, ids...)
	for i, id := range ids {
		if i < len(cols) && cols[i] != nil {
			copy(b.Data(id), cols[i])
		}
	}
	return b.Build()
}

// andEq appends " AND col = ?" when v is set.
func andEq(where string, args []any, col, v string) (string, []any) {
	if v == "" {
		return where, args
	}
	return where + " AND " + col + " = ?", append(args, v)
}
