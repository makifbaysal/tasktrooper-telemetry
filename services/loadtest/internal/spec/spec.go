// Package spec parses a stored scenario spec (loadtest_scenarios.spec, the UI ScenarioEditor model —
// apps/web/src/lib/loadtest.ts saveScenario) into an executable Plan, and evaluates thresholds.
//
// Accepted shapes (both the UI and the contracts §14 variants):
//
//	target, mode ('vus'|'rps'), regions [string]
//	levels [{name, ramp, hold, target}] | levels [{durationS, target}] | stages [{duration, target}]
//	steps ["METHOD /route"], weights {key: fraction | pct}, flow 'weighted'|'sequential'
//	requests {key: {params "k: v\n…", headers "H: v\n…" | {H: v}, body "json template" | json {...},
//	          method?, path?, pools [{name, detail, values?}], checks?}}
//	thinkTime (s), thresholds ["http_req_failed rate < 1%", "http_req_duration p(95) < 500 ms"] | {p95Ms, errorRate}
//	gate {latencyX, cpu, abortErrorRate, abortP95Ms, minRequests}, pools {name: [values]}, session {name: template}
//	maxVus, timeoutS, script
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stage ramps linearly from the previous target to Target over Duration.
type Stage struct {
	Duration time.Duration
	Target   float64
}

// Check is one assertion on a response. Op is one of < <= > >= == !=.
type Check struct {
	Name  string
	Field string // "status" | "latency"
	Op    string
	Value float64
}

// Pass reports whether the check holds.
func (c Check) Pass(status int, latencyMs float64) bool {
	v := float64(status)
	if c.Field == "latency" {
		v = latencyMs
	}
	switch c.Op {
	case "<":
		return v < c.Value
	case "<=":
		return v <= c.Value
	case ">":
		return v > c.Value
	case ">=":
		return v >= c.Value
	case "!=":
		return v != c.Value
	default:
		return v == c.Value
	}
}

// KV is one ordered name/value template pair.
type KV struct{ Name, Value string }

// Step is one request template.
type Step struct {
	Key     string // "METHOD /route" as the UI names it
	Method  string
	Path    string // route with :params
	Params  []KV   // path params (":name" in Path) or query params
	Headers []KV
	// Body is a JSON template (parsed into BodyJSON when valid JSON) or raw text.
	Body     string
	BodyJSON any
	Checks   []Check
	Weight   float64 // normalized, sums to 1 across steps
}

// Threshold is one pass/fail criterion of a run.
type Threshold struct {
	Name   string  // as shown in the UI, e.g. "http_req_duration p(95) < 500 ms"
	Metric string  // "error_rate" | "p50" | "p90" | "p95" | "p99" | "avg" | "max"
	Op     string  // "<" | "<="
	Limit  float64 // fraction for error_rate, ms for latencies
}

// Gate holds the automatic abort limits (contracts §14 gate) plus the UI progression gate (kept, not enforced).
type Gate struct {
	AbortErrorRate float64 `json:"abortErrorRate,omitempty"`
	AbortP95Ms     float64 `json:"abortP95Ms,omitempty"`
	MinRequests    int     `json:"minRequests,omitempty"`
	LatencyX       float64 `json:"latencyX,omitempty"`
	CPU            float64 `json:"cpu,omitempty"`
}

// Plan is a validated, executable scenario.
type Plan struct {
	Target      *url.URL
	Mode        string // "vus" | "rps"
	Stages      []Stage
	Steps       []Step
	Sequential  bool
	ThinkTime   time.Duration
	Thresholds  []Threshold
	Gate        Gate
	Pools       map[string][]any
	Session     map[string]string
	Regions     []string
	MaxVUs      int // rps mode: pre-allocated VUs (0 = derive)
	Timeout     time.Duration
	Script      string
	Progression string
}

type rawLevel struct {
	Name      string   `json:"name"`
	Ramp      *float64 `json:"ramp"`
	Hold      *float64 `json:"hold"`
	DurationS *float64 `json:"durationS"`
	Duration  *float64 `json:"duration"`
	Target    float64  `json:"target"`
}

type rawPool struct {
	Name   string `json:"name"`
	Values []any  `json:"values"`
}

type rawRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Params  json.RawMessage `json:"params"`
	Headers json.RawMessage `json:"headers"`
	Body    json.RawMessage `json:"body"`
	JSON    json.RawMessage `json:"json"`
	Pools   []rawPool       `json:"pools"`
	Checks  json.RawMessage `json:"checks"`
}

type raw struct {
	Target      string                `json:"target"`
	Mode        string                `json:"mode"`
	Regions     json.RawMessage       `json:"regions"`
	Levels      []rawLevel            `json:"levels"`
	Stages      []rawLevel            `json:"stages"`
	Steps       []string              `json:"steps"`
	Weights     map[string]float64    `json:"weights"`
	Flow        string                `json:"flow"`
	Requests    map[string]rawRequest `json:"requests"`
	ThinkTime   float64               `json:"thinkTime"`
	Thresholds  json.RawMessage       `json:"thresholds"`
	Gate        Gate                  `json:"gate"`
	Pools       map[string][]any      `json:"pools"`
	Session     map[string]string     `json:"session"`
	MaxVUs      int                   `json:"maxVus"`
	TimeoutS    float64               `json:"timeoutS"`
	Script      string                `json:"script"`
	Progression string                `json:"progression"`
}

// Parse parses and validates a stored spec.
func Parse(data []byte) (*Plan, error) {
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	p := &Plan{Mode: r.Mode, Script: strings.TrimSpace(r.Script), Pools: map[string][]any{}, Session: r.Session,
		Gate: r.Gate, MaxVUs: r.MaxVUs, Progression: r.Progression}
	if p.Mode == "" {
		p.Mode = "vus"
	}
	if p.Mode != "vus" && p.Mode != "rps" {
		return nil, fmt.Errorf("spec: mode must be vus|rps, got %q", p.Mode)
	}
	p.Regions = parseRegions(r.Regions)
	if r.Target != "" {
		u, err := url.Parse(strings.TrimSpace(r.Target))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("spec: target must be an http(s) base URL, got %q", r.Target)
		}
		p.Target = u
	}
	p.Timeout = 30 * time.Second
	if r.TimeoutS > 0 {
		p.Timeout = time.Duration(r.TimeoutS * float64(time.Second))
	}
	p.ThinkTime = time.Duration(math.Max(0, r.ThinkTime) * float64(time.Second))

	// stages: levels (ramp+hold) win over the derived stages
	switch {
	case len(r.Levels) > 0:
		p.Stages = levelStages(r.Levels)
	default:
		p.Stages = levelStages(r.Stages)
	}
	ths, err := parseThresholds(r.Thresholds)
	if err != nil {
		return nil, err
	}
	p.Thresholds = ths
	for name, vals := range r.Pools {
		p.Pools[name] = vals
	}
	if p.Script != "" {
		return p, nil // script mode: k6 owns the rest
	}
	if p.Target == nil {
		return nil, errors.New("spec: target is required")
	}
	if len(p.Stages) == 0 || Duration(p.Stages) <= 0 {
		return nil, errors.New("spec: levels/stages with a positive duration are required")
	}
	if Peak(p.Stages) <= 0 {
		return nil, errors.New("spec: peak load must be > 0")
	}
	keys := r.Steps
	if len(keys) == 0 {
		for k := range r.Requests {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	if len(keys) == 0 {
		return nil, errors.New("spec: at least one step is required")
	}
	p.Sequential = r.Flow == "sequential" || (r.Flow == "" && len(r.Weights) == 0)
	var wsum float64
	for _, k := range keys {
		st, err := buildStep(k, r.Requests[k], p.Pools)
		if err != nil {
			return nil, err
		}
		st.Weight = math.Max(0, r.Weights[k])
		if len(r.Weights) == 0 {
			st.Weight = 1
		}
		wsum += st.Weight
		p.Steps = append(p.Steps, st)
	}
	if wsum <= 0 {
		return nil, errors.New("spec: step weights sum to 0")
	}
	for i := range p.Steps { // fractions or percents: normalize either way
		p.Steps[i].Weight /= wsum
	}
	if p.Gate.MinRequests <= 0 {
		p.Gate.MinRequests = 20
	}
	return p, nil
}

func parseRegions(b json.RawMessage) []string {
	if len(b) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(b, &list) == nil {
		return list
	}
	var m map[string]float64 // {region: weight}
	if json.Unmarshal(b, &m) == nil {
		for k := range m {
			list = append(list, k)
		}
		sort.Strings(list)
	}
	return list
}

func levelStages(levels []rawLevel) []Stage {
	var out []Stage
	add := func(sec float64, target float64) {
		prev := 0.0
		if len(out) > 0 {
			prev = out[len(out)-1].Target
		}
		if sec == 0 && target != prev {
			out = append(out, Stage{Target: math.Max(0, target)}) // ramp 0: jump straight to the target
			return
		}
		if sec > 0 {
			out = append(out, Stage{Duration: time.Duration(sec * float64(time.Second)), Target: math.Max(0, target)})
		}
	}
	for _, l := range levels {
		switch {
		case l.Ramp != nil || l.Hold != nil:
			ramp := 0.0
			if l.Ramp != nil {
				ramp = *l.Ramp
			}
			add(ramp, l.Target)
			if l.Hold != nil {
				add(*l.Hold, l.Target)
			}
		case l.DurationS != nil:
			add(*l.DurationS, l.Target)
		case l.Duration != nil:
			add(*l.Duration, l.Target)
		}
	}
	return out
}

// Duration is the total length of the stages.
func Duration(st []Stage) time.Duration {
	var d time.Duration
	for _, s := range st {
		d += s.Duration
	}
	return d
}

// Peak is the highest stage target.
func Peak(st []Stage) float64 {
	m := 0.0
	for _, s := range st {
		m = math.Max(m, s.Target)
	}
	return m
}

// TargetAt is the load level at t: linear ramps between stage targets, starting from 0.
func TargetAt(st []Stage, t time.Duration) float64 {
	from := 0.0
	for _, s := range st {
		if t < s.Duration {
			return from + (s.Target-from)*float64(t)/float64(s.Duration)
		}
		t -= s.Duration
		from = s.Target
	}
	return from
}

// Integral is ∫₀ᵗ TargetAt dt in target·seconds (arrivals for rps mode, VU-seconds for vus mode).
func Integral(st []Stage, t time.Duration) float64 {
	from, acc := 0.0, 0.0
	for _, s := range st {
		if t <= 0 {
			break
		}
		if s.Duration == 0 {
			from = s.Target
			continue
		}
		d := min(t, s.Duration)
		end := from + (s.Target-from)*float64(d)/float64(s.Duration)
		acc += (from + end) / 2 * d.Seconds()
		t -= d
		from = s.Target
	}
	if t > 0 {
		acc += from * t.Seconds()
	}
	return acc
}

// Scale returns stages with every target multiplied by f (a region's share of the load).
func Scale(st []Stage, f float64) []Stage {
	out := make([]Stage, len(st))
	for i, s := range st {
		out[i] = Stage{Duration: s.Duration, Target: s.Target * f}
	}
	return out
}

// PeakVUs is the number of virtual users the plan needs at most (UI saveScenario's estimate for rps mode).
func (p *Plan) PeakVUs() int {
	peak := Peak(p.Stages)
	if p.Mode == "rps" {
		if p.MaxVUs > 0 {
			return p.MaxVUs
		}
		return max(1, int(math.Ceil(peak*(0.2+p.ThinkTime.Seconds()))))
	}
	return int(math.Ceil(peak))
}

// VUMinutes is the planned VU-minutes (usage accounting).
func (p *Plan) VUMinutes() float64 {
	if p.Mode == "rps" {
		// open model: VUs ≈ rate × (latency + think time)
		return Integral(p.Stages, Duration(p.Stages)) * (0.2 + p.ThinkTime.Seconds()) / 60
	}
	return Integral(p.Stages, Duration(p.Stages)) / 60
}

// ---------------------------------------------------------------- steps

var methodRe = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(\S+)$`)

func buildStep(key string, rq rawRequest, pools map[string][]any) (Step, error) {
	st := Step{Key: key, Method: strings.ToUpper(strings.TrimSpace(rq.Method)), Path: strings.TrimSpace(rq.Path)}
	if m := methodRe.FindStringSubmatch(strings.TrimSpace(key)); m != nil {
		if st.Method == "" {
			st.Method = m[1]
		}
		if st.Path == "" {
			st.Path = m[2]
		}
	}
	if st.Method == "" {
		st.Method = "GET"
	}
	if st.Path == "" {
		return st, fmt.Errorf("spec: step %q: want \"METHOD /path\" or requests[%q].path", key, key)
	}
	if strings.Contains(st.Path, "://") {
		return st, fmt.Errorf("spec: step %q: absolute URLs are not allowed; paths are relative to target", key)
	}
	var err error
	if st.Params, err = kvList(rq.Params); err != nil {
		return st, fmt.Errorf("spec: step %q params: %w", key, err)
	}
	if st.Headers, err = kvList(rq.Headers); err != nil {
		return st, fmt.Errorf("spec: step %q headers: %w", key, err)
	}
	switch {
	case len(rq.JSON) > 0 && string(rq.JSON) != "null":
		st.Body = string(rq.JSON)
		_ = json.Unmarshal(rq.JSON, &st.BodyJSON)
	case len(rq.Body) > 0 && string(rq.Body) != "null":
		var s string
		if json.Unmarshal(rq.Body, &s) == nil {
			st.Body = s
		} else {
			st.Body = string(rq.Body) // body given as a JSON object
		}
		if strings.TrimSpace(st.Body) != "" {
			var v any
			if json.Unmarshal([]byte(st.Body), &v) == nil {
				st.BodyJSON = v
			}
		}
	}
	for _, pl := range rq.Pools {
		if pl.Name != "" && len(pl.Values) > 0 {
			if _, ok := pools[pl.Name]; !ok {
				pools[pl.Name] = pl.Values
			}
		}
	}
	if st.Checks, err = parseChecks(rq.Checks); err != nil {
		return st, fmt.Errorf("spec: step %q checks: %w", key, err)
	}
	return st, nil
}

// kvList accepts "name: value" lines or a {name: value} object.
func kvList(b json.RawMessage) ([]KV, error) {
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		var out []KV
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("bad line %q (want name: value)", line)
			}
			out = append(out, KV{strings.TrimSpace(k), strings.TrimSpace(v)})
		}
		return out, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, KV{k, fmt.Sprint(m[k])})
	}
	return out, nil
}

var checkRe = regexp.MustCompile(`^\s*(status|latency|duration)\s*(<=|>=|==|!=|<|>)\s*([\d.]+)\s*(ms|s)?\s*$`)

// parseChecks accepts ["status == 200", "latency < 500ms"] or [{status: 200}, {maxMs: 500}] or {status, maxMs}.
func parseChecks(b json.RawMessage) ([]Check, error) {
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil {
		items = []json.RawMessage{b}
	}
	var out []Check
	for _, it := range items {
		var s string
		if json.Unmarshal(it, &s) == nil {
			m := checkRe.FindStringSubmatch(s)
			if m == nil {
				return nil, fmt.Errorf("unsupported check %q (want 'status == 200' or 'latency < 500ms')", s)
			}
			v, _ := strconv.ParseFloat(m[3], 64)
			field := m[1]
			if field == "duration" {
				field = "latency"
			}
			if field == "latency" && m[4] == "s" {
				v *= 1000
			}
			out = append(out, Check{Name: strings.TrimSpace(s), Field: field, Op: m[2], Value: v})
			continue
		}
		var o struct {
			Status *float64 `json:"status"`
			MaxMs  *float64 `json:"maxMs"`
		}
		if err := json.Unmarshal(it, &o); err != nil {
			return nil, fmt.Errorf("unsupported check %s", it)
		}
		if o.Status != nil {
			out = append(out, Check{Name: fmt.Sprintf("status == %g", *o.Status), Field: "status", Op: "==", Value: *o.Status})
		}
		if o.MaxMs != nil {
			out = append(out, Check{Name: fmt.Sprintf("latency < %gms", *o.MaxMs), Field: "latency", Op: "<", Value: *o.MaxMs})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- thresholds

var (
	failedRe = regexp.MustCompile(`http_req_failed\s+rate\s*(<=|<)\s*([\d.]+)\s*(%)?`)
	durRe    = regexp.MustCompile(`http_req_duration\s+(p\((\d+(?:\.\d+)?)\)|avg|max|med)\s*(<=|<)\s*([\d.]+)\s*(ms|s)?`)
)

// ErrorRateName / P95Name are the UI threshold labels (lib/loadtest.ts limitOf parses them).
func ErrorRateName(rate float64) string {
	return "http_req_failed rate < " + strconv.FormatFloat(math.Round(rate*100*100)/100, 'f', -1, 64) + "%"
}

// P95Name is the UI label of a p95 threshold.
func P95Name(ms float64) string {
	return "http_req_duration p(95) < " + strconv.FormatFloat(ms, 'f', -1, 64) + " ms"
}

func parseThresholds(b json.RawMessage) ([]Threshold, error) {
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	var list []string
	if json.Unmarshal(b, &list) == nil {
		var out []Threshold
		for _, s := range list {
			t, err := ParseThreshold(s)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, nil
	}
	var o struct {
		P95Ms     *float64 `json:"p95Ms"`
		P99Ms     *float64 `json:"p99Ms"`
		ErrorRate *float64 `json:"errorRate"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("spec: thresholds: %w", err)
	}
	var out []Threshold
	if o.ErrorRate != nil {
		out = append(out, Threshold{Name: ErrorRateName(*o.ErrorRate), Metric: "error_rate", Op: "<", Limit: *o.ErrorRate})
	}
	if o.P95Ms != nil {
		out = append(out, Threshold{Name: P95Name(*o.P95Ms), Metric: "p95", Op: "<", Limit: *o.P95Ms})
	}
	if o.P99Ms != nil {
		out = append(out, Threshold{Name: "http_req_duration p(99) < " + strconv.FormatFloat(*o.P99Ms, 'f', -1, 64) + " ms", Metric: "p99", Op: "<", Limit: *o.P99Ms})
	}
	return out, nil
}

// ParseThreshold parses one k6-style threshold label.
func ParseThreshold(s string) (Threshold, error) {
	if m := failedRe.FindStringSubmatch(s); m != nil {
		v, _ := strconv.ParseFloat(m[2], 64)
		if m[3] == "%" || v > 1 {
			v /= 100
		}
		return Threshold{Name: s, Metric: "error_rate", Op: m[1], Limit: v}, nil
	}
	if m := durRe.FindStringSubmatch(s); m != nil {
		metric := m[1]
		switch {
		case m[2] != "":
			metric = "p" + m[2]
		case metric == "med":
			metric = "p50"
		}
		v, _ := strconv.ParseFloat(m[4], 64)
		if m[5] == "s" {
			v *= 1000
		}
		return Threshold{Name: s, Metric: metric, Op: m[3], Limit: v}, nil
	}
	return Threshold{}, fmt.Errorf("spec: unsupported threshold %q", s)
}

// Pass reports whether value satisfies the threshold.
func (t Threshold) Pass(v float64) bool {
	if t.Op == "<=" {
		return v <= t.Limit
	}
	return v < t.Limit
}

// Host returns the target host (lower case, no port).
func (p *Plan) Host() string {
	if p.Target == nil {
		return ""
	}
	return strings.ToLower(p.Target.Hostname())
}
