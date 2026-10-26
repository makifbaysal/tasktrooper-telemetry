package writer

import (
	"math"

	"github.com/cespare/xxhash/v2"

	"github.com/tasktrooper/telemetry/services/ingest/internal/flatten"
	"github.com/tasktrooper/telemetry/services/ingest/internal/obs"
)

// TailSampler drops spans of uninteresting traces after the fact while keeping RED unbiased.
//
// Per trace (all spans of one trace id inside one writer flush):
//   - any span with is_error = 1, or an entry span slower than SlowNs → every span is kept with its weight unchanged;
//   - otherwise the trace is kept iff hash(trace_id) < Rate, and kept spans get sample_weight *= 1/Rate.
//
// Both rules give each span E[kept × weight] = its original weight, so every rollup that sums sample_weight
// (red_1m, consumer_1m, edges_1m, callers_1m, errors_1h) stays correct in expectation. The hash rule is deterministic,
// so all spans of a trace get the same decision even when they arrive in different batches or writer replicas.
// The error/slow rule only sees spans of the same flush (≤ flush interval / batch size): a trace whose error span
// arrives in another flush may lose its other spans with probability 1 − Rate (counts stay unbiased; only the
// waterfall is incomplete).
type TailSampler struct {
	Rate   float64 // (0, 1]; ≥ 1 disables sampling
	SlowNs uint64  // entry spans at least this slow keep their trace; 0 disables the rule
}

// Enabled reports whether the sampler drops anything.
func (s TailSampler) Enabled() bool { return s.Rate > 0 && s.Rate < 1 }

// keepByHash is the deterministic probabilistic decision for a trace id.
func (s TailSampler) keepByHash(traceID []byte) bool {
	h := xxhash.Sum64(traceID)
	return float64(h>>11)/float64(1<<53) < s.Rate
}

// Apply filters rows in place and returns the kept slice.
func (s TailSampler) Apply(rows []flatten.SpanRow) []flatten.SpanRow {
	if !s.Enabled() || len(rows) == 0 {
		return rows
	}
	type decision struct{ keep, forced bool }
	interesting := make(map[[16]byte]bool, len(rows)/4+1)
	for i := range rows {
		r := &rows[i]
		if r.IsError == 1 || (s.SlowNs > 0 && r.IsEntry == 1 && r.DurationNs >= s.SlowNs) {
			interesting[[16]byte(r.TraceID)] = true
		}
	}
	scale := float32(1 / s.Rate)
	decisions := make(map[[16]byte]decision, len(rows)/4+1)
	out := rows[:0]
	var kept, keptErr, dropped float64
	for i := range rows {
		r := rows[i]
		id := [16]byte(r.TraceID)
		d, ok := decisions[id]
		if !ok {
			if interesting[id] {
				d = decision{keep: true, forced: true}
			} else {
				d = decision{keep: s.keepByHash(r.TraceID)}
			}
			decisions[id] = d
		}
		switch {
		case !d.keep:
			dropped++
			continue
		case d.forced:
			keptErr++
		default:
			r.SampleWeight *= scale
			if math.IsInf(float64(r.SampleWeight), 0) {
				r.SampleWeight = math.MaxFloat32
			}
			kept++
		}
		out = append(out, r)
	}
	clear(rows[len(out):]) // drop references held by the tail
	obs.WriterSpansSampled.WithLabelValues("keep_forced").Add(keptErr)
	obs.WriterSpansSampled.WithLabelValues("keep_sampled").Add(kept)
	obs.WriterSpansSampled.WithLabelValues("drop").Add(dropped)
	return out
}
