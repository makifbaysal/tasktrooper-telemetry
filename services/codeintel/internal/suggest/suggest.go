// Package suggest turns enriched inventory items into alert suggestions (alert_suggestions, monitor_draft in the
// api `POST /monitors` body shape) and generated load test drafts (loadtest_scenarios, contracts §14).
package suggest

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

// MonitorTarget / MonitorCondition / QueueRule / MonitorDraft mirror services/api/internal/model MonitorInput.
type MonitorTarget struct {
	Service  string `json:"service,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Process  string `json:"process,omitempty"`
	View     string `json:"view,omitempty"`
	Env      string `json:"env,omitempty"`
}

type MonitorCondition struct {
	Op          string  `json:"op"`
	Threshold   float64 `json:"threshold"`
	ForMin      float64 `json:"forMin"`
	MinRequests float64 `json:"minRequests,omitempty"`
	Window      any     `json:"window,omitempty"`
}

type QueueRule struct {
	ClusterID string  `json:"clusterId"`
	Group     string  `json:"group"`
	Topic     string  `json:"topic,omitempty"`
	Metric    string  `json:"metric"`
	Threshold float64 `json:"threshold"`
	ForMin    float64 `json:"forMin"`
}

// MonitorDraft is the api MonitorInput body (POST /monitors). Source is left to the accept handler.
type MonitorDraft struct {
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	Target    MonitorTarget    `json:"target"`
	Query     map[string]any   `json:"query,omitempty"`
	Condition MonitorCondition `json:"condition"`
	Severity  string           `json:"severity"`
	Channels  []string         `json:"channels"`
	Status    string           `json:"status,omitempty"`
	Queue     *QueueRule       `json:"queue,omitempty"`
}

// Suggestion is one alert_suggestions row.
type Suggestion struct {
	ItemID      int64
	Title       string
	Kind        string
	Target      string
	Condition   string
	Criticality string
	CodeRef     string
	Confidence  float64
	Rationale   string
	Calibrated  bool
	Draft       MonitorDraft
}

// Cluster is a broker_clusters row (consumer_lag needs a cluster id).
type Cluster struct {
	Slug, Kind string // kafka | rabbitmq | sqs | pubsub
}

// Calibration minimum: thresholds come from traffic when the item saw at least this many requests in 7 days.
const minCalibrationCount = 500

// Build returns suggestions for critical/high items without a monitor. Items must carry their database id.
func Build(items []*model.Item, clusters []Cluster) []Suggestion {
	var out []Suggestion
	for _, it := range items {
		if it.ID == 0 || it.Monitored || it.Shadow || it.Dead || model.CriticalityRank(it.Criticality) < model.CriticalityRank(model.High) {
			continue
		}
		switch it.Kind {
		case model.KindEndpoint:
			out = append(out, errorRate(it))
			if it.Criticality == model.Critical {
				out = append(out, latency(it), errorNew(it))
			}
		case model.KindConsumer:
			if s, ok := consumerLag(it, clusters); ok {
				out = append(out, s)
			} else {
				out = append(out, consumerStalled(it))
			}
		case model.KindCron:
			out = append(out, cronMissed(it))
		}
	}
	return out
}

func severity(it *model.Item) string {
	if it.Criticality == model.Critical {
		return "critical"
	}
	return "warning"
}

func base(it *model.Item, kind string) Suggestion {
	conf := 0.6
	if it.LLM != nil {
		conf = 0.75
		for _, m := range it.LLM.SuggestedMonitors {
			if templateKind(m.Template) == kind {
				conf = 0.85
			}
		}
	}
	codeRef := ""
	if it.File != "" {
		codeRef = it.File + ":" + strconv.Itoa(it.Line)
	}
	why := it.Rationale
	if it.LLM != nil && it.LLM.Summary != "" {
		why = it.LLM.Summary + " " + why
	}
	return Suggestion{ItemID: it.ID, Kind: kind, Target: it.Service + " · " + it.Key, Criticality: it.Criticality, CodeRef: codeRef,
		Confidence: conf, Rationale: strings.TrimSpace(why)}
}

func templateKind(t string) string {
	switch t {
	case "error_rate":
		return "metric_threshold"
	case "latency":
		return "latency"
	case "error_new":
		return "error_new"
	case "consumer_lag", "dlq":
		return "consumer_lag"
	case "job_missed":
		return "job_missed"
	}
	return t
}

func calibrated(s *Suggestion) {
	s.Calibrated = true
	s.Confidence = math.Min(0.95, s.Confidence+0.1)
}

func reasonFor(it *model.Item, template string) string {
	if it.LLM == nil {
		return ""
	}
	for _, m := range it.LLM.SuggestedMonitors {
		if m.Template == template && m.Reason != "" {
			return " " + m.Reason
		}
	}
	return ""
}

func errorRate(it *model.Item) Suggestion {
	s := base(it, "metric_threshold")
	th, minReq := 0.02, 20.0
	if it.Criticality != model.Critical {
		th = 0.05
	}
	note := fmt.Sprintf("default threshold %s (no traffic baseline yet; recalibrated once observed)", pct(th))
	if st := it.Stats; st != nil && st.Count >= minCalibrationCount {
		th = math.Max(2*st.ErrorRate, 0.01)
		th = math.Min(th, 0.5)
		th = math.Round(th*1000) / 1000
		minReq = math.Max(10, math.Round(st.RPM*5*0.2)) // 20 % of a normal 5-minute window
		note = fmt.Sprintf("threshold = max(2 × 7-day error rate %s, 1%%)", pct(st.ErrorRate))
		calibrated(&s)
	}
	s.Title = "Error rate · " + it.Service + " " + it.Key
	s.Condition = fmt.Sprintf("error rate > %s for 5m", pct(th))
	s.Rationale = strings.TrimSpace(s.Rationale + reasonFor(it, "error_rate") + ". " + note + ".")
	s.Draft = MonitorDraft{Name: s.Title, Kind: "metric_threshold", Target: MonitorTarget{Service: it.Service, Endpoint: it.Key},
		Query: map[string]any{"metric": "error_rate"}, Condition: MonitorCondition{Op: ">", Threshold: th, ForMin: 5, MinRequests: minReq, Window: "5m"},
		Severity: severity(it), Channels: []string{}, Status: "active"}
	return s
}

func latency(it *model.Item) Suggestion {
	s := base(it, "latency")
	th := 1000.0
	note := "default threshold 1000 ms (no traffic baseline yet)"
	if st := it.Stats; st != nil && st.Count >= minCalibrationCount && st.P95 > 0 {
		th = math.Max(50, math.Ceil(st.P95*1.5/10)*10)
		note = fmt.Sprintf("threshold = 7-day p95 %.0f ms × 1.5", st.P95)
		calibrated(&s)
	}
	s.Title = "Latency p95 · " + it.Service + " " + it.Key
	s.Condition = fmt.Sprintf("p95 > %.0f ms for 10m", th)
	s.Rationale = strings.TrimSpace(s.Rationale + reasonFor(it, "latency") + ". " + note + ".")
	s.Draft = MonitorDraft{Name: s.Title, Kind: "latency", Target: MonitorTarget{Service: it.Service, Endpoint: it.Key},
		Query: map[string]any{"percentile": "p95"}, Condition: MonitorCondition{Op: ">", Threshold: th, ForMin: 10, MinRequests: 20, Window: "5m"},
		Severity: severity(it), Channels: []string{}, Status: "active"}
	return s
}

func errorNew(it *model.Item) Suggestion {
	s := base(it, "error_new")
	s.Title = "New error · " + it.Service + " " + it.Key
	s.Condition = "new error fingerprint (30d lookback)"
	s.Rationale = strings.TrimSpace(s.Rationale + reasonFor(it, "error_new") + ". A first-seen error on a critical path is worth a look before it trends.")
	s.Draft = MonitorDraft{Name: s.Title, Kind: "error_new", Target: MonitorTarget{Service: it.Service, Endpoint: it.Key},
		Query: map[string]any{"service": it.Service, "lookback": "30d"}, Condition: MonitorCondition{Op: ">", Threshold: 0, ForMin: 0, Window: "5m"},
		Severity: "warning", Channels: []string{}, Status: "active"}
	return s
}

// clusterFor picks the broker cluster of a consumer: the only cluster of the matching kind.
func clusterFor(it *model.Item, clusters []Cluster) (Cluster, bool) {
	kind := ""
	switch it.Framework {
	case "kafkajs", "sarama", "franz-go", "kafka-go", "kafka-python", "confluent-kafka":
		kind = "kafka"
	case "amqplib":
		kind = "rabbitmq"
	case "sqs-consumer", "aws-sdk-sqs":
		kind = "sqs"
	}
	if kind == "" {
		return Cluster{}, false
	}
	var found []Cluster
	for _, c := range clusters {
		if c.Kind == kind {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		return Cluster{}, false
	}
	return found[0], true
}

func consumerLag(it *model.Item, clusters []Cluster) (Suggestion, bool) {
	c, ok := clusterFor(it, clusters)
	if !ok {
		return Suggestion{}, false
	}
	s := base(it, "consumer_lag")
	topic := strings.Split(it.Source, ",")[0]
	r := QueueRule{ClusterID: c.Slug, Group: it.Key, Topic: topic, Metric: "time_behind", Threshold: 300, ForMin: 5}
	cond := "time behind > 300 s for 5m"
	if c.Kind != "kafka" {
		r.Metric, r.Threshold, cond = "oldest_age", 600, "oldest message age > 600 s for 5m"
	}
	s.Title = "Consumer lag · " + it.Service + " " + it.Key
	s.Target = c.Slug + " · " + it.Key
	s.Condition = cond
	s.Rationale = strings.TrimSpace(s.Rationale + reasonFor(it, "consumer_lag") + ". Default threshold; tune after a week of lag data.")
	s.Draft = MonitorDraft{Name: s.Title, Kind: "consumer_lag", Target: MonitorTarget{Service: it.Service, Process: it.Key},
		Condition: MonitorCondition{Op: ">", Threshold: r.Threshold, ForMin: r.ForMin}, Severity: severity(it), Channels: []string{},
		Status: "active", Queue: &r}
	return s, true
}

// consumerStalled is job_missed on the consumer process: no message processed for longer than usual.
func consumerStalled(it *model.Item) Suggestion {
	s := base(it, "job_missed")
	interval := 60.0
	note := "default 60 min allowance (no traffic baseline yet)"
	if st := it.Stats; st != nil && st.Count >= 50 {
		avg := 10080 / st.Count // minutes between messages on average over 7 days
		interval = math.Min(1440, math.Max(15, math.Ceil(avg*10)))
		note = fmt.Sprintf("allowance = 10 × average gap between messages (%.1f min)", avg)
		calibrated(&s)
	}
	grace := math.Max(1, math.Round(interval*0.1))
	s.Title = "Consumer stalled · " + it.Service + " " + it.Key
	s.Condition = fmt.Sprintf("no message processed for %.0fm", interval+grace)
	s.Rationale = strings.TrimSpace(s.Rationale + reasonFor(it, "consumer_lag") + ". No broker cluster to watch lag on, so this watches processing instead; " + note + ".")
	q := map[string]any{"process": it.Key, "intervalMin": interval, "graceMin": grace}
	if t := strings.Split(it.Source, ",")[0]; t != "" && t != it.Key {
		q["topic"] = t
	}
	s.Draft = MonitorDraft{Name: s.Title, Kind: "job_missed", Target: MonitorTarget{Service: it.Service, Process: it.Key}, Query: q,
		Condition: MonitorCondition{Op: ">", Threshold: interval + grace, ForMin: 0}, Severity: severity(it), Channels: []string{}, Status: "active"}
	return s
}

func cronMissed(it *model.Item) Suggestion {
	s := base(it, "job_missed")
	interval, ok := CronIntervalMin(it.Source)
	note := "interval from the schedule " + strconv.Quote(it.Source)
	if !ok {
		interval, note = 1440, "schedule not recognized; assumed daily"
	}
	grace := math.Max(1, math.Round(interval*0.1))
	s.Title = "Job missed · " + it.Service + " " + it.Key
	s.Condition = fmt.Sprintf("no run for %.0fm", interval+grace)
	s.Rationale = strings.TrimSpace(s.Rationale + reasonFor(it, "job_missed") + ". " + note + ".")
	s.Draft = MonitorDraft{Name: s.Title, Kind: "job_missed", Target: MonitorTarget{Service: it.Service, Process: it.Key},
		Query:     map[string]any{"process": it.Key, "intervalMin": interval, "graceMin": grace},
		Condition: MonitorCondition{Op: ">", Threshold: interval + grace, ForMin: 0}, Severity: severity(it), Channels: []string{}, Status: "active"}
	return s
}

// CronIntervalMin approximates the run interval of a 5/6-field cron expression, "@every 5m", "@hourly"…, or a
// celery beat schedule in seconds ("30.0").
func CronIntervalMin(expr string) (float64, bool) {
	expr = strings.TrimSpace(expr)
	switch expr {
	case "@yearly", "@annually":
		return 525600, true
	case "@monthly":
		return 43200, true
	case "@weekly":
		return 10080, true
	case "@daily", "@midnight":
		return 1440, true
	case "@hourly":
		return 60, true
	}
	if d, ok := strings.CutPrefix(expr, "@every "); ok {
		if v, ok := parseGoDurationMin(d); ok {
			return v, true
		}
	}
	if v, err := strconv.ParseFloat(expr, 64); err == nil && v > 0 { // seconds (celery beat)
		return math.Max(1, v/60), true
	}
	f := strings.Fields(expr)
	if len(f) == 6 { // seconds field
		f = f[1:]
	}
	if len(f) != 5 {
		return 0, false
	}
	step := func(field string, unit float64) (float64, bool) {
		switch {
		case field == "*":
			return unit, true
		case strings.HasPrefix(field, "*/"):
			n, err := strconv.ParseFloat(field[2:], 64)
			return n * unit, err == nil && n > 0
		case strings.Contains(field, ","):
			return unit * 0, false
		}
		return 0, false
	}
	minute, hour, dom, _, dow := f[0], f[1], f[2], f[3], f[4]
	if v, ok := step(minute, 1); ok {
		return v, true
	}
	if parts := strings.Split(minute, ","); len(parts) > 1 && hour == "*" {
		return 60 / float64(len(parts)), true
	}
	if v, ok := step(hour, 60); ok {
		return v, true
	}
	if parts := strings.Split(hour, ","); len(parts) > 1 {
		return 1440 / float64(len(parts)), true
	}
	if dom == "*" && (dow == "*" || strings.Contains(dow, "-") || strings.Contains(dow, ",")) {
		return 1440, true
	}
	if dom == "*" {
		return 10080, true
	}
	return 43200, true
}

func parseGoDurationMin(s string) (float64, bool) {
	var total float64
	num := ""
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9' || c == '.':
			num += string(c)
		default:
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			switch c {
			case 'h':
				total += v * 60
			case 'm':
				total += v
			case 's':
				total += v / 60
			default:
				return 0, false
			}
			num = ""
		}
	}
	return math.Max(1, total), num == "" && total > 0
}

func pct(v float64) string {
	s := strconv.FormatFloat(v*100, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

// ---------- load test drafts ----------

// Level is one load level (UI `Level` + contracts §14 `durationS`).
type Level struct {
	Name      string  `json:"name"`
	Ramp      float64 `json:"ramp"`
	Hold      float64 `json:"hold"`
	DurationS float64 `json:"durationS"`
	Target    float64 `json:"target"`
}

// Stage is the UI's flattened view of levels.
type Stage struct {
	Duration float64 `json:"duration"`
	Target   float64 `json:"target"`
}

// Request is how a step is called (contracts §14 `requests`).
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	JSON    any               `json:"json,omitempty"`
	Notes   string            `json:"notes,omitempty"`
}

// Spec is the loadtest_scenarios.spec of a generated draft.
type Spec struct {
	Target     string             `json:"target"`
	Services   []string           `json:"services"`
	Mode       string             `json:"mode"`
	Levels     []Level            `json:"levels"`
	Stages     []Stage            `json:"stages"`
	Steps      []string           `json:"steps"`
	Weights    map[string]float64 `json:"weights"`
	Requests   map[string]Request `json:"requests"`
	ThinkTime  float64            `json:"thinkTime"`
	Regions    []string           `json:"regions"`
	Gate       map[string]float64 `json:"gate"`
	Thresholds map[string]float64 `json:"thresholds"`
	Generated  map[string]any     `json:"generated"`
}

// Scenario is one generated draft (one per service with critical endpoints).
type Scenario struct {
	Name    string
	Service string
	Spec    Spec
}

// LoadTests drafts a scenario per service with at least one critical endpoint, over its critical and high HTTP
// endpoints; weights follow the observed production mix (equal without traffic).
func LoadTests(items []*model.Item, repo string, sha string) []Scenario {
	bySvc := map[string][]*model.Item{}
	hasCritical := map[string]bool{}
	for _, it := range items {
		if it.Kind != model.KindEndpoint || it.Shadow || it.Dead || model.CriticalityRank(it.Criticality) < model.CriticalityRank(model.High) {
			continue
		}
		method, _ := route.SplitKey(it.Key)
		if method == "GRPC" {
			continue
		}
		if method == "ANY" && (it.Stats == nil || it.Stats.Method == "") {
			continue
		}
		bySvc[it.Service] = append(bySvc[it.Service], it)
		if it.Criticality == model.Critical {
			hasCritical[it.Service] = true
		}
	}
	var out []Scenario
	for svc, eps := range bySvc {
		if !hasCritical[svc] {
			continue
		}
		spec := Spec{Services: []string{svc}, Weights: map[string]float64{}, Requests: map[string]Request{}, ThinkTime: 1,
			Regions: []string{}, Gate: map[string]float64{"latencyX": 3, "cpu": 0.85},
			Generated: map[string]any{"by": "codeintel", "repo": repo, "sha": sha,
				"note": "Draft from code + production traffic. Set target to a verified base URL before running."}}
		var total, p95 float64
		for _, it := range eps {
			if it.Stats != nil {
				total += it.Stats.Count
				p95 = math.Max(p95, it.Stats.P95)
			}
		}
		for _, it := range eps {
			method, p := route.SplitKey(it.Key)
			if method == "ANY" {
				method = strings.ToUpper(it.Stats.Method)
			}
			key := method + " " + p
			spec.Steps = append(spec.Steps, key)
			w := 1 / float64(len(eps))
			if total > 0 {
				w = 0
				if it.Stats != nil {
					w = it.Stats.Count / total
				}
			}
			spec.Weights[key] = math.Round(w*1000) / 1000
			req := Request{Method: method, Path: templatePath(p)}
			if method == "POST" || method == "PUT" || method == "PATCH" {
				req.Headers = map[string]string{"content-type": "application/json"}
				req.JSON = map[string]any{}
				if it.LLM != nil && it.LLM.LoadtestHint != nil && len(it.LLM.LoadtestHint.PayloadExample) > 0 {
					req.JSON = it.LLM.LoadtestHint.PayloadExample
				}
			}
			if it.LLM != nil && it.LLM.LoadtestHint != nil && !it.LLM.LoadtestHint.Idempotent {
				req.Notes = "not idempotent: run against a test tenant/sandbox account"
			}
			spec.Requests[key] = req
		}
		if total > 0 { // open model: production rps → 1×, 2×, 3×
			rps := total / (7 * 24 * 3600)
			base := math.Max(1, math.Round(rps*10)/10)
			spec.Mode = "rps"
			spec.Levels = []Level{
				{Name: "Production", Ramp: 60, Hold: 120, Target: base},
				{Name: "2× production", Ramp: 60, Hold: 120, Target: 2 * base},
				{Name: "3× production", Ramp: 60, Hold: 180, Target: 3 * base},
				{Name: "Cool-down", Ramp: 60, Hold: 0, Target: 0},
			}
			spec.Generated["productionRps"] = math.Round(rps*1000) / 1000
		} else {
			spec.Mode = "vus"
			spec.Levels = []Level{
				{Name: "Warm-up", Ramp: 30, Hold: 60, Target: 10},
				{Name: "Level 2", Ramp: 30, Hold: 120, Target: 25},
				{Name: "Peak", Ramp: 30, Hold: 180, Target: 50},
				{Name: "Cool-down", Ramp: 60, Hold: 0, Target: 0},
			}
		}
		for i := range spec.Levels {
			l := &spec.Levels[i]
			l.DurationS = l.Ramp + l.Hold
			spec.Stages = append(spec.Stages, Stage{Duration: l.Ramp, Target: l.Target})
			if l.Hold > 0 {
				spec.Stages = append(spec.Stages, Stage{Duration: l.Hold, Target: l.Target})
			}
		}
		th := 500.0
		if p95 > 0 {
			th = math.Ceil(p95*1.5/10) * 10
		}
		spec.Thresholds = map[string]float64{"p95Ms": th, "errorRate": 0.01}
		out = append(out, Scenario{Name: svc + " · critical paths (generated)", Service: svc, Spec: spec})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// templatePath turns ":id" params into template variables the runner fills ({{int 1-1000}}).
func templatePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, ":") {
			segs[i] = "{{int 1-1000}}"
		}
	}
	return strings.Join(segs, "/")
}
