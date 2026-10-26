package store

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// BaselineWindow returns the comparison window: "prev" (default: the window just before), "1d" or "7d" (the same
// window shifted back — seasonal).
func BaselineWindow(w timerange.Window, baseline string) (timerange.Window, error) {
	var shift time.Duration
	switch baseline {
	case "", "prev":
		return w.Prev(), nil
	case "1d":
		shift = 24 * time.Hour
	case "7d":
		shift = 7 * 24 * time.Hour
	default:
		return w, Invalid("baseline must be prev|1d|7d")
	}
	b := w
	b.From, b.To = w.From.Add(-shift), w.To.Add(-shift)
	return b, nil
}

func (d *DB) rcaStats(ctx context.Context, w timerange.Window, env, service string, ref *EndpointRef) (model.RcaStats, error) {
	var s model.RcaStats
	mins := w.Minutes()
	if ref.Process {
		rows, err := d.consumer(ctx, w, env, nil, " AND service = ? AND process = ?", []any{service, ref.Route}, false)
		if err != nil || len(rows) == 0 {
			return s, err
		}
		r := rows[0]
		return model.RcaStats{Requests: r.processed, Rpm: r.processed / mins, AvgMs: div(r.dsum, r.processed), P50: qt(r.q, 0), P95: qt(r.q, 1),
			ErrorRate: div(r.failed, r.processed)}, nil
	}
	rows, err := d.red(ctx, w, env, nil, " AND service = ? AND route = ? AND method = ?", []any{service, ref.Route, ref.Method}, false)
	if err != nil || len(rows) == 0 {
		return s, err
	}
	r := rows[0]
	return model.RcaStats{Requests: r.count, Rpm: r.count / mins, AvgMs: div(r.dsum, r.count), P50: qt(r.q, 0), P95: qt(r.q, 2),
		ErrorRate: div(r.errors, r.count)}, nil
}

func childStats(e edgeRow, requests, mins float64) model.RcaChildStats {
	return model.RcaChildStats{Calls: e.calls, CallsPerMin: e.calls / mins, CallsPerReq: div(e.calls, requests), MsPerReq: div(e.dsum, requests),
		AvgMs: div(e.dsum, e.calls), P50: qt(e.q, 0), P95: qt(e.q, 1), ErrorRate: div(e.errors, e.calls)}
}

// EndpointRca compares an endpoint and each dependency (edges_1m) between the range and a baseline window, so the
// UI's root-cause view uses measured deltas instead of approximating them.
func (d *DB) EndpointRca(ctx context.Context, q Q, service string, ref EndpointRef, baseline string) (*model.EndpointRca, error) {
	bw, err := BaselineWindow(q.W, baseline)
	if err != nil {
		return nil, err
	}
	out := &model.EndpointRca{Service: service, Endpoint: ref.Key(), From: ms(q.W.From), To: ms(q.W.To), BaselineFrom: ms(bw.From), BaselineTo: ms(bw.To),
		Children: []model.RcaChild{}}
	if out.Current, err = d.rcaStats(ctx, q.W, q.Env, service, &ref); err != nil {
		return nil, err
	}
	if out.Baseline, err = d.rcaStats(ctx, bw, q.Env, service, &ref); err != nil {
		return nil, err
	}
	cur, err := d.edges(ctx, q.W, q.Env, service, &ref, false, false, "", nil)
	if err != nil {
		return nil, err
	}
	base, err := d.edges(ctx, bw, q.Env, service, &ref, false, false, "", nil)
	if err != nil {
		return nil, err
	}
	type key struct{ dep, kind string }
	idx := map[key]int{}
	child := func(e edgeRow) *model.RcaChild {
		k := key{e.dep, e.kind}
		i, ok := idx[k]
		if !ok {
			i = len(out.Children)
			idx[k] = i
			out.Children = append(out.Children, model.RcaChild{Dep: e.dep, Kind: e.kind})
		}
		return &out.Children[i]
	}
	depCur, depBase := 0.0, 0.0
	for _, e := range cur {
		c := child(e)
		c.Current = childStats(e, out.Current.Requests, q.W.Minutes())
		depCur += c.Current.MsPerReq
	}
	for _, e := range base {
		c := child(e)
		c.Baseline = childStats(e, out.Baseline.Requests, bw.Minutes())
		depBase += c.Baseline.MsPerReq
	}
	out.OwnMs = [2]float64{math.Max(0, out.Current.AvgMs-depCur), math.Max(0, out.Baseline.AvgMs-depBase)}
	out.DeltaAvgMs = out.Current.AvgMs - out.Baseline.AvgMs
	for i := range out.Children {
		c := &out.Children[i]
		c.DeltaMsPerReq = c.Current.MsPerReq - c.Baseline.MsPerReq
		c.Share = div(c.DeltaMsPerReq, out.DeltaAvgMs)
	}
	sort.SliceStable(out.Children, func(i, j int) bool { return out.Children[i].DeltaMsPerReq > out.Children[j].DeltaMsPerReq })
	return out, nil
}
