package engine

import (
	"math"
	"sort"
	"strconv"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
)

// gamma is the relative bucket width of Hist (≈1% quantile error).
const gamma = 1.02

var logGamma = math.Log(gamma)

// Hist is a sparse log-bucketed latency histogram (milliseconds). It merges exactly, so region shards
// can be combined by the controller.
type Hist struct {
	B     map[int]uint64 `json:"b"`
	Count uint64         `json:"count"`
	SumMs float64        `json:"sum"`
	MaxMs float64        `json:"max"`
}

func bucketOf(ms float64) int {
	us := ms * 1000
	if us <= 1 {
		return 0
	}
	return int(math.Ceil(math.Log(us) / logGamma))
}

// Add records one value.
func (h *Hist) Add(ms float64) {
	if h.B == nil {
		h.B = map[int]uint64{}
	}
	h.B[bucketOf(ms)]++
	h.Count++
	h.SumMs += ms
	h.MaxMs = math.Max(h.MaxMs, ms)
}

// Merge adds o into h.
func (h *Hist) Merge(o Hist) {
	if h.B == nil {
		h.B = map[int]uint64{}
	}
	for k, v := range o.B {
		h.B[k] += v
	}
	h.Count += o.Count
	h.SumMs += o.SumMs
	h.MaxMs = math.Max(h.MaxMs, o.MaxMs)
}

// Quantile returns the q-quantile (0..1) in ms; 0 when empty.
func (h *Hist) Quantile(q float64) float64 {
	if h.Count == 0 {
		return 0
	}
	keys := make([]int, 0, len(h.B))
	for k := range h.B {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	rank := uint64(math.Ceil(q * float64(h.Count)))
	if rank == 0 {
		rank = 1
	}
	var acc uint64
	for _, k := range keys {
		acc += h.B[k]
		if acc >= rank {
			if k == 0 {
				return 0.001
			}
			// bucket k covers (γ^(k-1), γ^k] µs; report its midpoint
			v := math.Pow(gamma, float64(k)) * 2 / (1 + gamma) / 1000
			return math.Min(v, h.MaxMs)
		}
	}
	return h.MaxMs
}

// Mean returns the average in ms.
func (h *Hist) Mean() float64 {
	if h.Count == 0 {
		return 0
	}
	return h.SumMs / float64(h.Count)
}

// Counts are request/error tallies plus latency.
type Counts struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	Hist     Hist  `json:"hist"`
}

func (c *Counts) merge(o Counts) {
	c.Requests += o.Requests
	c.Errors += o.Errors
	c.Hist.Merge(o.Hist)
}

// Stats is a mergeable record of (part of) a run.
type Stats struct {
	Counts
	Steps   map[string]*Counts  `json:"steps"`
	Status  map[string]int64    `json:"status"`            // "200", "503", "error" (transport)
	Checks  map[string][2]int64 `json:"checks,omitempty"`  // name → [passed, failed]
	Dropped int64               `json:"dropped,omitempty"` // rps mode: arrivals with no free VU
	// VUSeconds integrates active VUs over time (usage accounting).
	VUSeconds float64 `json:"vuSeconds"`
	ElapsedS  float64 `json:"elapsedS"`
	PeakVUs   float64 `json:"peakVus"`
}

// NewStats returns empty stats.
func NewStats() *Stats {
	return &Stats{Steps: map[string]*Counts{}, Status: map[string]int64{}, Checks: map[string][2]int64{}}
}

func (s *Stats) record(step string, status int, ms float64, failed bool, checks []checkResult) {
	if s.Steps[step] == nil {
		s.Steps[step] = &Counts{}
	}
	c := s.Steps[step]
	c.Requests++
	s.Requests++
	if failed {
		c.Errors++
		s.Errors++
	}
	c.Hist.Add(ms)
	s.Hist.Add(ms)
	code := "error"
	if status > 0 {
		code = strconv.Itoa(status)
	}
	s.Status[code]++
	for _, r := range checks {
		v := s.Checks[r.name]
		if r.ok {
			v[0]++
		} else {
			v[1]++
		}
		s.Checks[r.name] = v
	}
}

// Merge adds o into s (region shards → run).
func (s *Stats) Merge(o *Stats) {
	if o == nil {
		return
	}
	if s.Steps == nil {
		s.Steps = map[string]*Counts{}
	}
	if s.Status == nil {
		s.Status = map[string]int64{}
	}
	if s.Checks == nil {
		s.Checks = map[string][2]int64{}
	}
	s.Counts.merge(o.Counts)
	for k, v := range o.Steps {
		if s.Steps[k] == nil {
			s.Steps[k] = &Counts{}
		}
		s.Steps[k].merge(*v)
	}
	for k, v := range o.Status {
		s.Status[k] += v
	}
	for k, v := range o.Checks {
		c := s.Checks[k]
		s.Checks[k] = [2]int64{c[0] + v[0], c[1] + v[1]}
	}
	s.Dropped += o.Dropped
	s.VUSeconds += o.VUSeconds
	s.ElapsedS = math.Max(s.ElapsedS, o.ElapsedS)
	s.PeakVUs += o.PeakVUs // shards run concurrently
}

// ErrorRate is errors / requests.
func (c *Counts) ErrorRate() float64 {
	if c.Requests == 0 {
		return 0
	}
	return float64(c.Errors) / float64(c.Requests)
}

// ThresholdResult is one evaluated threshold (summary.thresholds; api model.Threshold + value).
type ThresholdResult struct {
	Name   string  `json:"name"`
	Passed bool    `json:"passed"`
	Value  float64 `json:"value"`
}

// MetricValue returns the stat a threshold metric refers to.
func (c *Counts) MetricValue(metric string) float64 {
	switch metric {
	case "error_rate":
		return c.ErrorRate()
	case "avg":
		return c.Hist.Mean()
	case "max":
		return c.Hist.MaxMs
	}
	if len(metric) > 1 && metric[0] == 'p' {
		if q, err := strconv.ParseFloat(metric[1:], 64); err == nil {
			return c.Hist.Quantile(q / 100)
		}
	}
	return 0
}

// Evaluate checks thresholds against stats. No requests at all fails every threshold.
func Evaluate(ths []spec.Threshold, s *Stats) ([]ThresholdResult, bool) {
	out := make([]ThresholdResult, 0, len(ths))
	all := true
	for _, t := range ths {
		v := s.MetricValue(t.Metric)
		ok := s.Requests > 0 && t.Pass(v)
		all = all && ok
		out = append(out, ThresholdResult{Name: t.Name, Passed: ok, Value: round(v, 4)})
	}
	return out, all
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

// StepSummary is the per-step part of the final summary.
type StepSummary struct {
	Key       string  `json:"key"`
	Requests  int64   `json:"count"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"errorRate"`
	Rps       float64 `json:"rps"`
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	P99       float64 `json:"p99"`
	AvgMs     float64 `json:"avgMs"`
}

// Summary is the final loadtest_runs.summary document. The first fields are what the api's run
// model reads (progress, rps, p95, errorRate, vus, thresholds).
type Summary struct {
	Progress    float64             `json:"progress"`
	Rps         float64             `json:"rps"`
	P95         float64             `json:"p95"`
	ErrorRate   float64             `json:"errorRate"`
	Vus         float64             `json:"vus"`
	Thresholds  []ThresholdResult   `json:"thresholds"`
	P50         float64             `json:"p50"`
	P99         float64             `json:"p99"`
	AvgMs       float64             `json:"avgMs"`
	MaxMs       float64             `json:"maxMs"`
	Requests    int64               `json:"requests"`
	Errors      int64               `json:"errors"`
	DurationS   float64             `json:"durationS"`
	VUMinutes   float64             `json:"vuMinutes"`
	Dropped     int64               `json:"dropped,omitempty"`
	Steps       []StepSummary       `json:"steps"`
	StatusCodes map[string]int64    `json:"statusCodes"`
	Checks      map[string][2]int64 `json:"checks,omitempty"`
	AbortReason string              `json:"abortReason,omitempty"`
	Warnings    []string            `json:"warnings,omitempty"`
	Regions     []RegionSummary     `json:"regions,omitempty"`
}

// RegionSummary is one shard's headline numbers.
type RegionSummary struct {
	Region    string  `json:"region"`
	State     string  `json:"state"`
	Requests  int64   `json:"requests"`
	ErrorRate float64 `json:"errorRate"`
	P95       float64 `json:"p95"`
	Error     string  `json:"error,omitempty"`
}

// Summarize builds the summary of stats over elapsed seconds.
func Summarize(s *Stats, ths []spec.Threshold) (Summary, bool) {
	el := math.Max(s.ElapsedS, 0.001)
	res, ok := Evaluate(ths, s)
	sum := Summary{
		Progress: 1, Rps: round(float64(s.Requests)/el, 2), P95: round(s.Hist.Quantile(0.95), 2),
		ErrorRate: round(s.ErrorRate(), 5), Vus: s.PeakVUs, Thresholds: res,
		P50: round(s.Hist.Quantile(0.5), 2), P99: round(s.Hist.Quantile(0.99), 2), AvgMs: round(s.Hist.Mean(), 2),
		MaxMs: round(s.Hist.MaxMs, 2), Requests: s.Requests, Errors: s.Errors, DurationS: round(s.ElapsedS, 1),
		VUMinutes: round(s.VUSeconds/60, 3), Dropped: s.Dropped, StatusCodes: s.Status, Checks: s.Checks,
		Steps: []StepSummary{},
	}
	if len(sum.Checks) == 0 {
		sum.Checks = nil
	}
	keys := make([]string, 0, len(s.Steps))
	for k := range s.Steps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := s.Steps[k]
		sum.Steps = append(sum.Steps, StepSummary{
			Key: k, Requests: c.Requests, Errors: c.Errors, ErrorRate: round(c.ErrorRate(), 5),
			Rps: round(float64(c.Requests)/el, 2), P50: round(c.Hist.Quantile(0.5), 2), P95: round(c.Hist.Quantile(0.95), 2),
			P99: round(c.Hist.Quantile(0.99), 2), AvgMs: round(c.Hist.Mean(), 2),
		})
	}
	return sum, ok
}

// Live is the loadtest_runs.live document (contracts §14).
type Live struct {
	TS        int64   `json:"ts"` // unix ms
	Vus       float64 `json:"vus"`
	Rps       float64 `json:"rps"`
	P95       float64 `json:"p95"`
	ErrorRate float64 `json:"errorRate"`
	Progress  float64 `json:"progress"`
	Requests  int64   `json:"-"` // live = exactly { ts, vus, rps, p95, errorRate, progress }
}
