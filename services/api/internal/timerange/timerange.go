// Package timerange maps the UI's TimeRange (1h|6h|24h|7d) to bucketed query windows and builds zero-filled series.
//
// Bucket sizes match apps/web/src/data/mock.ts RANGE_POINTS: 1h → 60×1m, 6h → 72×5m, 24h → 96×15m, 7d → 84×2h.
// Buckets are aligned to the step; the last bucket contains "now" (it is partial).
package timerange

import (
	"fmt"
	"math"
	"time"
)

// Range is the UI TimeRange.
type Range string

const (
	R1h  Range = "1h"
	R6h  Range = "6h"
	R24h Range = "24h"
	R7d  Range = "7d"
)

type spec struct {
	points int
	step   time.Duration
}

var specs = map[Range]spec{
	R1h:  {60, time.Minute},
	R6h:  {72, 5 * time.Minute},
	R24h: {96, 15 * time.Minute},
	R7d:  {84, 2 * time.Hour},
}

// Parse validates s; empty means 1h.
func Parse(s string) (Range, error) {
	if s == "" {
		return R1h, nil
	}
	r := Range(s)
	if _, ok := specs[r]; !ok {
		return "", fmt.Errorf("invalid range %q (want 1h|6h|24h|7d)", s)
	}
	return r, nil
}

// Window is a bucketed time window [From, To).
type Window struct {
	Range  Range
	Step   time.Duration
	Points int
	From   time.Time // start of the first bucket (inclusive)
	To     time.Time // end of the last bucket (exclusive)
}

// Window returns the bucketed window ending in the bucket that contains now.
func (r Range) Window(now time.Time) Window {
	s, ok := specs[r]
	if !ok {
		s = specs[R1h]
		r = R1h
	}
	last := now.UTC().Truncate(s.step)
	from := last.Add(-time.Duration(s.points-1) * s.step)
	return Window{Range: r, Step: s.step, Points: s.points, From: from, To: last.Add(s.step)}
}

// Custom builds a window of n buckets of step ending in the bucket containing now (used for KPI/alert sub-windows).
func Custom(now time.Time, step time.Duration, n int) Window {
	last := now.UTC().Truncate(step)
	return Window{Step: step, Points: n, From: last.Add(-time.Duration(n-1) * step), To: last.Add(step)}
}

// Hourly reports whether the window should read the hourly rollups (red_1h): only for 7d.
func (w Window) Hourly() bool { return w.Step >= time.Hour }

// Duration is To - From.
func (w Window) Duration() time.Duration { return w.To.Sub(w.From) }

// Prev is the window of equal length right before w (for "prev" comparisons).
func (w Window) Prev() Window {
	d := w.Duration()
	p := w
	p.From, p.To = w.From.Add(-d), w.From
	return p
}

// StepSeconds is the bucket size in seconds.
func (w Window) StepSeconds() int64 { return int64(w.Step / time.Second) }

// StepMinutes is the bucket size in minutes.
func (w Window) StepMinutes() float64 { return w.Step.Minutes() }

// Minutes is the window length in minutes (for per-minute rates over the whole window).
func (w Window) Minutes() float64 { return w.Duration().Minutes() }

// Timestamps returns bucket start times in unix ms.
func (w Window) Timestamps() []int64 {
	out := make([]int64, w.Points)
	for i := range out {
		out[i] = w.From.Add(time.Duration(i) * w.Step).UnixMilli()
	}
	return out
}

// Index returns the bucket of t, or -1 when t is outside the window.
func (w Window) Index(t time.Time) int {
	if t.Before(w.From) || !t.Before(w.To) {
		return -1
	}
	return int(t.Sub(w.From) / w.Step)
}

// BucketExpr is the ClickHouse expression mapping a DateTime column to its bucket index in w.
func (w Window) BucketExpr(col string) string {
	return fmt.Sprintf("intDiv(toInt64(toUnixTimestamp(%s)) - %d, %d)", col, w.From.Unix(), w.StepSeconds())
}

// Series is one named line of a chart.
type Series struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	Data  []float64 `json:"data"`
}

// Set is the API's chart shape: { timestamps, series }.
type Set struct {
	Timestamps []int64  `json:"timestamps"`
	Series     []Series `json:"series"`
}

// Builder accumulates zero-filled series over a window, keeping insertion order.
type Builder struct {
	w     Window
	order []string
	by    map[string]*Series
}

// NewBuilder returns a builder; ids are pre-created (in order) so they exist even with no data.
func NewBuilder(w Window, ids ...string) *Builder {
	b := &Builder{w: w, by: map[string]*Series{}}
	for _, id := range ids {
		b.series(id, id)
	}
	return b
}

func (b *Builder) series(id, label string) *Series {
	if s, ok := b.by[id]; ok {
		return s
	}
	s := &Series{ID: id, Label: label, Data: make([]float64, b.w.Points)}
	b.by[id] = s
	b.order = append(b.order, id)
	return s
}

// Label sets (or creates) a series' label.
func (b *Builder) Label(id, label string) { b.series(id, label).Label = label }

// Set stores v at bucket idx of series id; out-of-range buckets and non-finite values are ignored.
func (b *Builder) Set(id string, idx int, v float64) {
	if idx < 0 || idx >= b.w.Points || math.IsNaN(v) || math.IsInf(v, 0) {
		b.series(id, id)
		return
	}
	b.series(id, id).Data[idx] = v
}

// Add adds v at bucket idx of series id.
func (b *Builder) Add(id string, idx int, v float64) {
	if idx < 0 || idx >= b.w.Points || math.IsNaN(v) || math.IsInf(v, 0) {
		b.series(id, id)
		return
	}
	b.series(id, id).Data[idx] += v
}

// Data returns the data slice of id (creating it).
func (b *Builder) Data(id string) []float64 { return b.series(id, id).Data }

// Build returns the chart set.
func (b *Builder) Build() Set {
	out := Set{Timestamps: b.w.Timestamps(), Series: make([]Series, 0, len(b.order))}
	for _, id := range b.order {
		out.Series = append(out.Series, *b.by[id])
	}
	return out
}

// Ratio returns num/den element-wise (0 where den is 0).
func Ratio(num, den []float64) []float64 {
	out := make([]float64, len(num))
	for i := range num {
		if i < len(den) && den[i] != 0 {
			out[i] = num[i] / den[i]
		}
	}
	return out
}

// Scale multiplies every element by k into a new slice.
func Scale(xs []float64, k float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = x * k
	}
	return out
}
