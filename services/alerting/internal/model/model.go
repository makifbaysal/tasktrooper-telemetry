// Package model holds the monitor/KPI definitions the engine evaluates and the JSON shapes
// stored in Postgres (monitors.query / monitors.condition / kpis.target / kpis.good_when).
package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// State is monitors.state.
type State string

const (
	StateOK      State = "ok"
	StatePending State = "pending"
	StateFiring  State = "firing"
	StateNoData  State = "no_data"
)

// Monitor is one row of monitors joined with its project's org.
type Monitor struct {
	ID         int64
	OrgID      uint32
	ProjectID  uint32
	Name       string
	Kind       string
	Target     Target
	Query      Query
	Condition  Condition
	Severity   string
	Channels   []string
	State      State
	StateSince time.Time
	LastValue  *float64
	UpdatedAt  time.Time
	// MutedUntil is monitors.muted_until (refreshed every tick: editing it does not bump updated_at).
	MutedUntil *time.Time
	// ClusterKind is broker_clusters.kind (kafka|rabbitmq|sqs|pubsub) of query.clusterId, for consumer_lag.
	ClusterKind string
}

// Target is monitors.target: { service, endpoint?, process?, view? }.
type Target struct {
	Service  string `json:"service"`
	Endpoint string `json:"endpoint"`
	Process  string `json:"process"`
	View     string `json:"view"`
	Env      string `json:"env"`
}

// Query is the union of every kind's monitors.query fields (see README § Monitor query shapes).
type Query struct {
	// red_1m / consumer_1m scope
	Service string `json:"service"`
	Route   string `json:"route"`
	Method  string `json:"method"`
	Env     string `json:"env"`

	Metric     string  `json:"metric"`     // metric_threshold: error_rate|p50|p95|p99|rpm|apdex; dlq: dlq|failure_rate|failed|retried|retry_rate; consumer_lag: lag metric
	Percentile string  `json:"percentile"` // p50|p75|p95|p99
	StatusMin  int     `json:"statusMin"`  // error_rate: count status >= statusMin as errors (500 → s5xx) instead of is_error
	ApdexTMs   float64 `json:"tMs"`

	Direction      string `json:"direction"`      // throughput: rpm (default) | drop | spike | change
	BaselineOffset string `json:"baselineOffset"` // throughput baseline shift, default 168h

	// logs
	Severity    string   `json:"severity"`
	MinSeverity string   `json:"minSeverity"`
	PatternID   FlexUint `json:"patternId"`
	Contains    string   `json:"contains"`
	Lookback    string   `json:"lookback"` // log_pattern "new" lookback, default 7d

	// consumers / brokers (QueueRule)
	Process       string   `json:"process"`
	Topic         string   `json:"topic"`
	ConsumerGroup string   `json:"consumerGroup"`
	ClusterID     string   `json:"clusterId"`
	Group         string   `json:"group"`
	Threshold     *float64 `json:"threshold"`
	ForMin        *float64 `json:"forMin"`

	// RUM
	App        string `json:"app"`
	View       string `json:"view"`
	Vital      string `json:"vital"`
	DeviceType string `json:"deviceType"`
	Per        string `json:"per"` // crash_rate: session (default) | view

	// messaging_system filter: kafka | rabbitmq | aws_sqs | gcp_pubsub (consumer_1m; consumer_lag cross-check)
	MessagingSystem string `json:"messagingSystem"`

	// anomaly: metric (error_rate|p50|p95|p99|rpm), direction up|down|both, minDeviation (metric units), weeks (4)
	MinDeviation float64 `json:"minDeviation"`
	Weeks        int     `json:"weeks"`

	// composite: { op: and|or, monitorIds: [...] }
	Op         string      `json:"op"`
	MonitorIDs []FlexInt64 `json:"monitorIds"`

	// job_missed: expected every intervalMin, tolerated graceMin late
	IntervalMin float64 `json:"intervalMin"`
	GraceMin    float64 `json:"graceMin"`
}

// FlexInt64 decodes an id written as a JSON number or string (the UI uses string ids).
type FlexInt64 int64

func (f *FlexInt64) UnmarshalJSON(b []byte) error {
	var u FlexUint
	if err := u.UnmarshalJSON(b); err != nil {
		return err
	}
	*f = FlexInt64(u)
	return nil
}

// Condition is monitors.condition: { op, threshold, forMin, window, minRequests }.
type Condition struct {
	Op          string
	OpSet       bool // op was given explicitly (apdex defaults to '<')
	Threshold   float64
	For         time.Duration
	Window      time.Duration
	MinRequests float64
	// Renotify re-sends a still-firing, unacknowledged incident every this long (renotifyMin; 0 = off).
	Renotify time.Duration
	// Stream: evaluate log_match / error_new from the Kafka stream too (TT_ALERT_STREAM=true).
	Stream bool
}

// UnmarshalJSON accepts forMin (minutes) or for ("5m"), window as "5m" or minutes, min_requests or minRequests.
func (c *Condition) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.Op, _ = raw["op"].(string)
	c.OpSet = c.Op != ""
	if c.Op == "" {
		c.Op = ">"
	}
	c.Threshold, _ = toFloat(raw["threshold"])
	if v, ok := raw["forMin"]; ok {
		c.For, _ = ParseDuration(v)
	} else if v, ok := raw["for"]; ok {
		c.For, _ = ParseDuration(v)
	}
	if v, ok := raw["window"]; ok {
		c.Window, _ = ParseDuration(v)
	}
	if c.Window <= 0 {
		c.Window = 5 * time.Minute
	}
	if v, ok := raw["minRequests"]; ok {
		c.MinRequests, _ = toFloat(v)
	} else if v, ok := raw["min_requests"]; ok {
		c.MinRequests, _ = toFloat(v)
	}
	if v, ok := raw["renotifyMin"]; ok {
		c.Renotify, _ = ParseDuration(v)
	}
	c.Stream, _ = raw["stream"].(bool)
	return nil
}

// Breached reports whether v violates the condition.
func (c Condition) Breached(v float64) bool { return Compare(c.Op, v, c.Threshold) }

// Compare applies op ('>', '>=', '<', '<=', '==', '!=') to v and th. NaN never breaches.
func Compare(op string, v, th float64) bool {
	if math.IsNaN(v) {
		return false
	}
	switch op {
	case ">=":
		return v >= th
	case "<":
		return v < th
	case "<=":
		return v <= th
	case "==", "=":
		return v == th
	case "!=":
		return v != th
	default:
		return v > th
	}
}

// LowerIsWorse reports whether the op alerts on low values (peak tracking uses min then).
func LowerIsWorse(op string) bool { return op == "<" || op == "<=" }

// ParseDuration accepts numbers (minutes), Go durations ("90s", "1h30m") and day suffixes ("7d").
func ParseDuration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return time.Duration(x * float64(time.Minute)), nil
	case int:
		return time.Duration(x) * time.Minute, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return time.Duration(f * float64(time.Minute)), nil
		}
		if strings.HasSuffix(s, "d") {
			f, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
			if err != nil {
				return 0, err
			}
			return time.Duration(f * 24 * float64(time.Hour)), nil
		}
		return time.ParseDuration(s)
	}
	return 0, fmt.Errorf("model: bad duration %v", v)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// FlexUint decodes a uint64 written as a JSON number or string (pattern ids exceed 2^53).
type FlexUint uint64

func (f *FlexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		fl, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = uint64(fl)
	}
	*f = FlexUint(v)
	return nil
}

var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true, "CONSUME": true, "VIEW": true}

// SplitEndpoint splits "POST /checkout" into ("POST", "/checkout"); a bare route has no method.
func SplitEndpoint(ep string) (method, route string) {
	ep = strings.TrimSpace(ep)
	if i := strings.IndexByte(ep, ' '); i > 0 && httpMethods[strings.ToUpper(ep[:i])] {
		return strings.ToUpper(ep[:i]), strings.TrimSpace(ep[i+1:])
	}
	return "", ep
}

// Scope resolves the effective service/route/method/env: query fields win over target.
func (m *Monitor) Scope() (service, route, method, env string) {
	q, t := m.Query, m.Target
	service, route, method, env = q.Service, q.Route, q.Method, q.Env
	if service == "" {
		service = t.Service
	}
	if env == "" {
		env = t.Env
	}
	if route == "" && t.Endpoint != "" {
		m2, r2 := SplitEndpoint(t.Endpoint)
		route = r2
		if method == "" {
			method = m2
		}
	} else if route != "" && method == "" {
		method, route = SplitEndpoint(route)
	}
	return
}

// Kpi is one row of kpis with its alert rules.
type Kpi struct {
	ID        int64
	OrgID     uint32
	ProjectID uint32
	Name      string
	Target    KpiTarget
	SLI       string // latency | availability | success_rate
	GoodWhen  GoodWhen
	Objective float64
	Window    string
	Rules     []KpiRule
}

// KpiTarget is kpis.target: { kind: endpoint|consumer|view|page, service, ref }.
type KpiTarget struct {
	Kind      string `json:"kind"`
	Service   string `json:"service"`
	ServiceID string `json:"serviceId"`
	Ref       string `json:"ref"`
	Env       string `json:"env"`
}

// Svc returns service, falling back to serviceId.
func (t KpiTarget) Svc() string {
	if t.Service != "" {
		return t.Service
	}
	return t.ServiceID
}

// GoodWhen is kpis.good_when.
type GoodWhen struct {
	LatencyMs   float64 `json:"latencyMs"`
	StatusBelow int     `json:"statusBelow"`
	LcpMs       float64 `json:"lcpMs"`
	Vital       string  `json:"vital"`
}

// KpiRule is one kpi_alert_rules row.
type KpiRule struct {
	ID          int64
	Kind        string // burn_rate | below_target
	LongWindow  time.Duration
	ShortWindow time.Duration
	Threshold   float64
	Severity    string
	Channels    []string
	// persisted state (kpi_alert_rules.state/state_since)
	State      State
	StateSince time.Time
}
