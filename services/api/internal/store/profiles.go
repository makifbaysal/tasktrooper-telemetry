package store

import (
	"context"
	"sort"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/flame"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// Continuous profiling (ADR-022, contracts §24): profile_stacks (stack rows) + profiles (one row per profile and type).
// Trees are built in Go from SELECT stack, sum(value) … GROUP BY stack_hash.

// ProfileFilter narrows the profile queries. From/To is the window ([From, To)).
type ProfileFilter struct {
	Service, Type, Env, Version, Host, Route, Method string
	From, To                                         time.Time
}

// MaxProfileStacks bounds the distinct stacks read per tree; the rest is folded into one [other] stack so totals
// stay exact (the writer caps a profile at 10k stacks; a wide window over many hosts / versions can exceed this).
const MaxProfileStacks = 200_000

// ProfileTypeNames are the known profile types (contracts §24).
var ProfileTypeNames = map[string]bool{"cpu": true, "wall": true, "alloc_space": true, "alloc_objects": true, "inuse_space": true,
	"inuse_objects": true, "goroutines": true, "lock": true, "block": true}

func (f ProfileFilter) validate(needType bool) error {
	if f.Service == "" {
		return Invalid("service is required")
	}
	if needType && !ProfileTypeNames[f.Type] {
		return Invalid("type must be cpu|wall|alloc_space|alloc_objects|inuse_space|inuse_objects|goroutines|lock|block")
	}
	if !f.From.Before(f.To) {
		return Invalid("from must be before to")
	}
	if f.To.Sub(f.From) > 15*24*time.Hour {
		return Invalid("profile windows are limited to 14 days (retention)")
	}
	return nil
}

// conds returns the WHERE tail for env / version / host (+ route / method on profile_stacks).
func (f ProfileFilter) conds(stacks bool) (string, []any) {
	where := ""
	var args []any
	add := func(col, v string) {
		if v != "" {
			where += " AND " + col + " = ?"
			args = append(args, v)
		}
	}
	add("env", f.Env)
	add("version", f.Version)
	add("host", f.Host)
	if stacks {
		add("route", f.Route)
		add("method", f.Method)
	}
	return where, args
}

// ProfileTypes lists the profile types with data for a service in the window.
func (d *DB) ProfileTypes(ctx context.Context, f ProfileFilter) (model.ProfileTypes, error) {
	out := model.ProfileTypes{Types: []model.ProfileType{}}
	if err := f.validate(false); err != nil {
		return out, err
	}
	w, wa := f.conds(false)
	rows, err := d.ch.Query(ctx, `SELECT type, any(unit), topK(1)(runtime)[1], arraySort(groupUniqArray(50)(host)), arraySort(groupUniqArray(50)(version))
	  FROM profiles WHERE {{tenant}} AND service = ? AND ts >= fromUnixTimestamp64Milli(?) AND ts < fromUnixTimestamp64Milli(?)`+w+`
	  GROUP BY type ORDER BY type`, append([]any{f.Service, f.From.UnixMilli(), f.To.UnixMilli()}, wa...)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var t model.ProfileType
		if err := rows.Scan(&t.Type, &t.Unit, &t.Runtime, &t.Hosts, &t.Versions); err != nil {
			rows.Close()
			return out, err
		}
		t.Hosts, t.Versions = nonEmpty(t.Hosts), nonEmpty(t.Versions)
		out.Types = append(out.Types, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out.Types) == 0 {
		return out, err
	}
	rrows, err := d.ch.Query(ctx, `SELECT type, toInt64(uniqExact(route, method)) FROM profile_stacks
	  WHERE {{tenant}} AND service = ? AND ts >= toDateTime(?) AND ts < toDateTime(?) AND route != ''`+w+` GROUP BY type`,
		append([]any{f.Service, secs(f.From), secs(f.To)}, wa...)...)
	if err != nil {
		return out, err
	}
	defer rrows.Close()
	routes := map[string]int{}
	for rrows.Next() {
		var t string
		var n int64
		if err := rrows.Scan(&t, &n); err != nil {
			return out, err
		}
		routes[t] = int(n)
	}
	for i := range out.Types {
		out.Types[i].Routes = routes[out.Types[i].Type]
	}
	return out, rrows.Err()
}

func nonEmpty(xs []string) []string {
	out := []string{}
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// profileStacks reads the aggregated stacks of f (≤ MaxProfileStacks; the remainder becomes an [other] stack)
// and the unit.
func (d *DB) profileStacks(ctx context.Context, f ProfileFilter) ([]flame.Stack, string, int64, error) {
	w, wa := f.conds(true)
	base := append([]any{f.Service, f.Type, secs(f.From), secs(f.To)}, wa...)
	var total int64
	var unit string
	if err := d.ch.QueryRow(ctx, `SELECT toInt64(sum(value)), any(unit) FROM profile_stacks
	  WHERE {{tenant}} AND service = ? AND type = ? AND ts >= toDateTime(?) AND ts < toDateTime(?) AND value > 0`+w, base...).Scan(&total, &unit); err != nil {
		return nil, "", 0, err
	}
	if total == 0 {
		return nil, unit, 0, nil
	}
	rows, err := d.ch.Query(ctx, `SELECT any(stack), toInt64(sum(value)) AS v FROM profile_stacks
	  WHERE {{tenant}} AND service = ? AND type = ? AND ts >= toDateTime(?) AND ts < toDateTime(?) AND value > 0`+w+`
	  GROUP BY stack_hash ORDER BY v DESC LIMIT ?`, append(base, MaxProfileStacks)...)
	if err != nil {
		return nil, "", 0, err
	}
	defer rows.Close()
	var out []flame.Stack
	var seen int64
	for rows.Next() {
		var s flame.Stack
		if err := rows.Scan(&s.Frames, &s.Value); err != nil {
			return nil, "", 0, err
		}
		seen += s.Value
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, err
	}
	if rest := total - seen; rest > 0 {
		out = append(out, flame.Stack{Frames: []string{"[other]"}, Value: rest})
	}
	return out, unit, flame.Total(out), nil
}

func (d *DB) profileCount(ctx context.Context, f ProfileFilter) (int, error) {
	w, wa := f.conds(false)
	var n uint64
	err := d.ch.QueryRow(ctx, `SELECT count() FROM profiles WHERE {{tenant}} AND service = ? AND type = ?
	  AND ts >= fromUnixTimestamp64Milli(?) AND ts < fromUnixTimestamp64Milli(?)`+w,
		append([]any{f.Service, f.Type, f.From.UnixMilli(), f.To.UnixMilli()}, wa...)...).Scan(&n)
	return int(n), err
}

// Flamegraph returns the merged, pruned flame graph of f.
func (d *DB) Flamegraph(ctx context.Context, f ProfileFilter) (*model.Flamegraph, error) {
	if err := f.validate(true); err != nil {
		return nil, err
	}
	stacks, unit, total, err := d.profileStacks(ctx, f)
	if err != nil {
		return nil, err
	}
	n, err := d.profileCount(ctx, f)
	if err != nil {
		return nil, err
	}
	return &model.Flamegraph{Type: f.Type, Unit: unitOr(unit, f.Type), Total: total, Profiles: n, From: f.From.UnixMilli(), To: f.To.UnixMilli(),
		Root: flame.Build(stacks)}, nil
}

// FlameDiff compares target f with base b (same service / type; different window or version).
func (d *DB) FlameDiff(ctx context.Context, f, b ProfileFilter) (*model.FlameDiff, error) {
	if err := f.validate(true); err != nil {
		return nil, err
	}
	if err := b.validate(true); err != nil {
		return nil, err
	}
	target, unit, total, err := d.profileStacks(ctx, f)
	if err != nil {
		return nil, err
	}
	base, bunit, btotal, err := d.profileStacks(ctx, b)
	if err != nil {
		return nil, err
	}
	if unit == "" {
		unit = bunit
	}
	return &model.FlameDiff{Type: f.Type, Unit: unitOr(unit, f.Type), Total: total, BaseTotal: btotal, Root: flame.Diff(target, base)}, nil
}

// TopFunctions returns functions by self value.
func (d *DB) TopFunctions(ctx context.Context, f ProfileFilter, limit int) (*model.TopFunctions, error) {
	if err := f.validate(true); err != nil {
		return nil, err
	}
	stacks, unit, total, err := d.profileStacks(ctx, f)
	if err != nil {
		return nil, err
	}
	return &model.TopFunctions{Unit: unitOr(unit, f.Type), Total: total, Functions: flame.Top(stacks, clampLimit(limit, 50, 500))}, nil
}

// unitOr is the unit of a type when there is no data.
func unitOr(unit, typ string) string {
	if unit != "" {
		return unit
	}
	switch typ {
	case "cpu", "wall", "lock", "block":
		return "nanoseconds"
	case "alloc_space", "inuse_space":
		return "bytes"
	}
	return "count"
}

// ProfileSeries: cpu / wall → average cores (total / profile duration) per host; other types → value per minute.
// ≤ 8 hosts by volume, the rest summed into "other".
func (d *DB) ProfileSeries(ctx context.Context, w timerange.Window, f ProfileFilter) (model.SeriesSet, error) {
	f.From, f.To = w.From, w.To
	if err := f.validate(true); err != nil {
		return model.SeriesSet{}, err
	}
	cond, ca := f.conds(false)
	val := "toFloat64(sum(total)) / ?"
	vargs := []any{w.StepMinutes()}
	switch f.Type {
	case "cpu", "wall":
		// average cores of the profiles in the bucket, summed over a host's processes (services on one host)
		val = "toFloat64(sum(total / greatest(1, dateDiff('millisecond', ts, end_ts)) / 1e6) / greatest(1, uniqExact(toStartOfMinute(ts))))"
		vargs = nil
	case "alloc_space", "alloc_objects", "lock", "block":
		// rate types: normalise each profile by its own window (SDKs that profile 10 s of every 60 s — .NET,
		// Python — would otherwise read ~6× low), then per minute; summed over processes, averaged over minutes
		val = "toFloat64(sum(total / greatest(1, dateDiff('millisecond', ts, end_ts)) * 60000) / greatest(1, uniqExact(toStartOfMinute(ts))))"
		vargs = nil
	}
	rows, err := d.ch.Query(ctx, `SELECT host, `+w.BucketExpr("ts")+` AS b, `+val+` FROM profiles
	  WHERE {{tenant}} AND service = ? AND type = ? AND ts >= fromUnixTimestamp64Milli(?) AND ts < fromUnixTimestamp64Milli(?)`+cond+`
	  GROUP BY host, b`, append(append(vargs, f.Service, f.Type, w.From.UnixMilli(), w.To.UnixMilli()), ca...)...)
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	type pt struct {
		b int
		v float64
	}
	by := map[string][]pt{}
	vol := map[string]float64{}
	for rows.Next() {
		var h string
		var b int64
		var v float64
		if err := rows.Scan(&h, &b, &v); err != nil {
			return model.SeriesSet{}, err
		}
		v = fin(v)
		by[h] = append(by[h], pt{int(b), v})
		vol[h] += v
	}
	if err := rows.Err(); err != nil {
		return model.SeriesSet{}, err
	}
	hosts := make([]string, 0, len(by))
	for h := range by {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool {
		if vol[hosts[i]] != vol[hosts[j]] {
			return vol[hosts[i]] > vol[hosts[j]]
		}
		return hosts[i] < hosts[j]
	})
	b := timerange.NewBuilder(w)
	for i, h := range hosts {
		id, lbl := h, h
		if lbl == "" {
			id, lbl = "unknown", "unknown host"
		}
		if i >= 8 {
			id, lbl = "other", "Other"
		}
		b.Label(id, lbl)
		for _, p := range by[h] {
			b.Add(id, p.b, p.v)
		}
	}
	return b.Build(), nil
}
