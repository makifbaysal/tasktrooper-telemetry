// Package model holds the API's JSON shapes. Field names mirror apps/web/src/data/types.ts and infra.ts so the
// UI can swap its mock functions for fetches without touching screens. Extra fields are additive.
package model

import (
	"encoding/json"

	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// SeriesSet is the chart shape { timestamps, series }.
type SeriesSet = timerange.Set

// Health is ok | warning | critical.
type Health string

const (
	OK       Health = "ok"
	Warning  Health = "warning"
	Critical Health = "critical"
)

// ---------------------------------------------------------------- APM

type Service struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"` // http | worker | consumer | web | mobile
	Language      string    `json:"language"`
	Version       string    `json:"version"`
	Rpm           float64   `json:"rpm"`
	ErrorRate     float64   `json:"errorRate"`
	P95           float64   `json:"p95"`
	PrevRpm       float64   `json:"prevRpm"`
	PrevErrorRate float64   `json:"prevErrorRate"`
	PrevP95       float64   `json:"prevP95"`
	Status        Health    `json:"status"`
	Instances     int       `json:"instances"`
	Lag           *float64  `json:"lag,omitempty"`
	Spark         []float64 `json:"spark,omitempty"` // rpm per bucket of the requested range
}

type Subscription struct {
	Process       string  `json:"process"`
	Source        string  `json:"source"` // kafka | rabbitmq | sqs | cron
	Topic         string  `json:"topic"`
	Group         string  `json:"group,omitempty"`
	Partitions    int     `json:"partitions,omitempty"`
	Schedule      string  `json:"schedule,omitempty"`
	Lag           float64 `json:"lag"`
	RetriesPerMin float64 `json:"retriesPerMin"`
	DlqPerMin     float64 `json:"dlqPerMin"`
}

type Endpoint struct {
	ID          string        `json:"id"`
	ServiceID   string        `json:"serviceId"`
	Method      string        `json:"method"`
	Route       string        `json:"route"`
	Rpm         float64       `json:"rpm"`
	ErrorRate   float64       `json:"errorRate"`
	P50         float64       `json:"p50"`
	P95         float64       `json:"p95"`
	P99         float64       `json:"p99"`
	Monitored   bool          `json:"monitored"`
	Criticality string        `json:"criticality"`
	Sub         *Subscription `json:"sub,omitempty"`
	Status      *StatusCounts `json:"status,omitempty"` // requests per status class over the range
	Spark       []float64     `json:"spark,omitempty"`
}

// StatusCounts is the error breakdown by status class.
type StatusCounts struct {
	S2xx float64 `json:"s2xx"`
	S3xx float64 `json:"s3xx"`
	S4xx float64 `json:"s4xx"`
	S5xx float64 `json:"s5xx"`
}

type Dependency struct {
	ID          string  `json:"id"`
	ServiceID   string  `json:"serviceId"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"` // db | cache | http | queue
	AvgMs       float64 `json:"avgMs"`
	P95         float64 `json:"p95"`
	ErrorRate   float64 `json:"errorRate"`
	CallsPerMin float64 `json:"callsPerMin"`
}

type EndpointCall struct {
	Dep         Dependency `json:"dep"`
	CallsPerReq float64    `json:"callsPerReq"`
	MsPerReq    float64    `json:"msPerReq"`
}

type TimeBreakdown struct {
	AvgMs float64            `json:"avgMs"`
	Parts map[string]float64 `json:"parts"` // app | db | cache | http | queue
}

type Caller struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Via  string `json:"via"`
}

type CallerTraffic struct {
	Caller      Caller    `json:"caller"`
	Rpm         float64   `json:"rpm"`
	Share       float64   `json:"share"`
	ErrorRate   float64   `json:"errorRate"`
	P95         float64   `json:"p95"`
	TopEndpoint *Endpoint `json:"topEndpoint,omitempty"`
}

type TraceSummary struct {
	ID      string  `json:"id"` // hex trace id
	Ts      int64   `json:"ts"`
	Dur     float64 `json:"dur"` // ms
	Status  int     `json:"status"`
	Service string  `json:"service"`
	Method  string  `json:"method"`
	Route   string  `json:"route"`
	IsError bool    `json:"isError"`
}

type Span struct {
	SpanID       string            `json:"spanId"`
	ParentSpanID string            `json:"parentSpanId,omitempty"`
	Service      string            `json:"service"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Category     string            `json:"category"` // app | db | cache | http | queue (TimeCategory)
	Start        float64           `json:"start"`    // ms from trace start
	Dur          float64           `json:"dur"`      // ms
	Status       int               `json:"status"`
	Failed       bool              `json:"failed"`
	Dep          string            `json:"dep,omitempty"`
	Route        string            `json:"route,omitempty"`
	ErrorType    string            `json:"errorType,omitempty"`
	Attrs        map[string]string `json:"attrs,omitempty"`
}

type Trace struct {
	ID       string   `json:"id"`
	Ts       int64    `json:"ts"`
	Dur      float64  `json:"dur"`
	Services []string `json:"services"`
	Errors   int      `json:"errors"`
	Spans    []Span   `json:"spans"`
}

type ErrorIssue struct {
	Fingerprint string  `json:"fingerprint"` // UInt64 as string (JS precision)
	Service     string  `json:"service"`
	Type        string  `json:"type"`
	Message     string  `json:"message"`
	Route       string  `json:"route"`
	FirstSeen   int64   `json:"firstSeen"`
	LastSeen    int64   `json:"lastSeen"`
	Count       float64 `json:"count"`
	SampleTrace string  `json:"sampleTraceId,omitempty"`
	Status      string  `json:"status"` // open | resolved | ignored
}

// DeployMarker merges span-derived versions (source "spans", ClickHouse deploys) with Postgres deploy_markers
// (Vercel, CI, manual, aws, gcp). Shape = UI DeployMarker (apps/web/src/data/integrations.ts).
type DeployMarker struct {
	Service       string `json:"service"`
	Env           string `json:"env"`
	Version       string `json:"version"`
	Ts            int64  `json:"ts"`
	Source        string `json:"source"` // spans | vercel | ci | manual | aws | gcp
	URL           string `json:"url,omitempty"`
	CommitMessage string `json:"commitMessage,omitempty"`
	Author        string `json:"author,omitempty"`
	State         string `json:"state,omitempty"` // building | ready | error | canceled
	IntegrationID string `json:"integrationId,omitempty"`
}

// Frame is one symbolicated stack frame (symbolicator contract, .ai/contracts.md §12).
type Frame struct {
	Fn    string `json:"fn"`
	File  string `json:"file"`
	Line  int    `json:"line"`
	Col   int    `json:"col"`
	InApp bool   `json:"inApp"`
}

// ErrorDetail is one backend error issue with its latest stack (symbolicated when the symbolicator knows the release).
type ErrorDetail struct {
	ErrorIssue
	Release      string  `json:"release"`
	Platform     string  `json:"platform"`
	Stack        string  `json:"stack"`
	Frames       []Frame `json:"frames"`
	Symbolicated bool    `json:"symbolicated"`
}

// ---------------------------------------------------------------- logs

type LogLine struct {
	ID      string `json:"id"`
	Ts      int64  `json:"ts"`
	Level   string `json:"level"` // debug | info | warn | error
	Service string `json:"service"`
	Message string `json:"message"`
	TraceID string `json:"traceId,omitempty"`
	Route   string `json:"route,omitempty"`
}

type LogPage struct {
	Lines []LogLine `json:"lines"`
	// pass as ?before= to get the next (older) page; 0 when there is none
	NextBefore int64              `json:"nextBefore"`
	Levels     map[string]float64 `json:"levels"`   // lines per level in the range (facet)
	Services   map[string]float64 `json:"services"` // lines per service in the range (facet)
}

type LogPattern struct {
	ID       string   `json:"id"` // pattern_id as string
	Pattern  string   `json:"pattern"`
	Level    string   `json:"level"`
	Services []string `json:"services"`
	Count    float64  `json:"count"`
	Last     int64    `json:"last"`
}

// ---------------------------------------------------------------- RUM

type Vital struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Good  float64 `json:"good"`
	Poor  float64 `json:"poor"`
	Unit  string  `json:"unit"` // ms | score
}

type AppSummary struct {
	ActiveUsers     float64 `json:"activeUsers"`
	PrevActiveUsers float64 `json:"prevActiveUsers"`
	Sessions        float64 `json:"sessions"`
	PageViewsPerMin float64 `json:"pageViewsPerMin"`
	AvgSessionSec   float64 `json:"avgSessionSec"`
	ErrorRate       float64 `json:"errorRate"`
	PrevErrorRate   float64 `json:"prevErrorRate"`
	Vitals          []Vital `json:"vitals"`
}

type RumPage struct {
	ID        string  `json:"id"`
	Path      string  `json:"path"`
	Views     float64 `json:"views"`
	Users     float64 `json:"users"`
	AvgTime   float64 `json:"avgTime"` // seconds
	Lcp       float64 `json:"lcp"`
	Inp       float64 `json:"inp"`
	Cls       float64 `json:"cls"`
	ErrorRate float64 `json:"errorRate"`
	Bounce    float64 `json:"bounce"`
}

type Session struct {
	ID         string  `json:"id"`
	User       string  `json:"user"`
	Identified bool    `json:"identified"`
	Country    string  `json:"country"`
	Device     string  `json:"device"`
	StartedAt  int64   `json:"startedAt"`
	Duration   float64 `json:"duration"` // seconds
	Pages      float64 `json:"pages"`
	Errors     float64 `json:"errors"`
	Entry      string  `json:"entry"`
	Exit       string  `json:"exit"`
}

type RumError struct {
	Fingerprint string  `json:"fingerprint"`
	Type        string  `json:"type"`
	Message     string  `json:"message"`
	View        string  `json:"view"`
	Count       float64 `json:"count"`
	Sessions    float64 `json:"sessions"`
	Crash       bool    `json:"crash"`
	FirstSeen   int64   `json:"firstSeen"`
	LastSeen    int64   `json:"lastSeen"`
	AppVersion  string  `json:"appVersion"`
}

// RumErrorDetail is one RUM error / crash with its latest stack.
type RumErrorDetail struct {
	RumError
	App          string  `json:"app"`
	Platform     string  `json:"platform"`
	Stack        string  `json:"stack"`
	Frames       []Frame `json:"frames"`
	Symbolicated bool    `json:"symbolicated"`
}

type RumResource struct {
	URL         string  `json:"url"`
	Method      string  `json:"method"`
	CallsPerMin float64 `json:"callsPerMin"`
	ErrorRate   float64 `json:"errorRate"`
	P50         float64 `json:"p50"`
	P95         float64 `json:"p95"`
}

type AppStart struct {
	Kind  string  `json:"kind"` // cold | warm | hot
	Count float64 `json:"count"`
	P50   float64 `json:"p50"`
	P75   float64 `json:"p75"`
	P95   float64 `json:"p95"`
}

type AppAPICall struct {
	Endpoint  Endpoint `json:"endpoint"`
	Service   Service  `json:"service"`
	Rpm       float64  `json:"rpm"`
	ErrorRate float64  `json:"errorRate"`
	P95       float64  `json:"p95"`
}

// ---------------------------------------------------------------- alerting

type QueueRule struct {
	ClusterID string  `json:"clusterId"`
	Group     string  `json:"group"`
	Topic     string  `json:"topic,omitempty"`
	Metric    string  `json:"metric"` // lag | time_behind | partition_lag | growing | not_drained | rebalances | oldest_age
	Threshold float64 `json:"threshold"`
	ForMin    float64 `json:"forMin"`
}

// MonitorTarget is what a monitor watches.
type MonitorTarget struct {
	Service  string `json:"service,omitempty"`
	Endpoint string `json:"endpoint,omitempty"` // "METHOD /route"
	Process  string `json:"process,omitempty"`
	View     string `json:"view,omitempty"`
	KpiID    string `json:"kpiId,omitempty"`
	Env      string `json:"env,omitempty"`
}

// MonitorCondition is the threshold rule.
type MonitorCondition struct {
	Op          string  `json:"op"` // > | < | >= | <=
	Threshold   float64 `json:"threshold"`
	ForMin      float64 `json:"forMin"`
	MinRequests float64 `json:"minRequests,omitempty"`
	// Window is the evaluation window ("5m" or minutes; alert engine default 5m).
	Window any `json:"window,omitempty"`
}

type Monitor struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Target     string     `json:"target"`    // human readable
	Condition  string     `json:"condition"` // human readable
	Severity   string     `json:"severity"`
	State      string     `json:"state"`  // ok | pending | firing | muted
	Source     string     `json:"source"` // manual | code | template
	LastChange int64      `json:"lastChange"`
	Channels   []string   `json:"channels"`
	Queue      *QueueRule `json:"queue,omitempty"`

	// structured definition (for the editor)
	Status     string           `json:"status"` // active | muted | draft
	TargetSpec MonitorTarget    `json:"targetSpec"`
	Query      json.RawMessage  `json:"query"`
	Rule       MonitorCondition `json:"rule"`
	LastValue  *float64         `json:"lastValue,omitempty"`
	// MutedUntil (unix ms) silences notifications until then; State reads "muted" while it is in the future.
	MutedUntil int64 `json:"mutedUntil,omitempty"`
}

// MonitorPatch changes parts of a monitor. mutedUntil: unix ms; 0 or null unmutes.
type MonitorPatch struct {
	Name       *string           `json:"name,omitempty"`
	Severity   *string           `json:"severity,omitempty"`
	Channels   []string          `json:"channels,omitempty"`
	Status     *string           `json:"status,omitempty"`
	Condition  *MonitorCondition `json:"condition,omitempty"`
	MutedUntil *int64            `json:"mutedUntil,omitempty"`
	// MutedUntilSet is true when the body carried the mutedUntil key (null unmutes).
	MutedUntilSet bool `json:"-"`
}

// MaintenanceScope limits a window to services / monitors; empty = the whole project.
type MaintenanceScope struct {
	Services   []string `json:"services,omitempty"`
	MonitorIDs []string `json:"monitorIds,omitempty"`
}

type MaintenanceWindow struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	StartsAt int64            `json:"startsAt"`
	EndsAt   int64            `json:"endsAt"`
	Scope    MaintenanceScope `json:"scope"`
	Active   bool             `json:"active"`
}

type MaintenanceWindowInput struct {
	Name     string           `json:"name"`
	StartsAt int64            `json:"startsAt"`
	EndsAt   int64            `json:"endsAt"`
	Scope    MaintenanceScope `json:"scope"`
}

// NotificationChannel is a delivery target. Secret config fields (webhookUrl, routingKey, apiKey, secret) are
// sealed into the secrets table and returned only as their reference ("secret:<id>").
type NotificationChannel struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Type    string         `json:"type"` // slack | pagerduty | email | webhook | opsgenie | teams
	Config  map[string]any `json:"config"`
	Secrets []string       `json:"secrets"` // config keys held in the secrets table
}

type NotificationChannelInput struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Config map[string]any `json:"config"`
}

type MonitorInput struct {
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	Target    MonitorTarget    `json:"target"`
	Query     json.RawMessage  `json:"query,omitempty"`
	Condition MonitorCondition `json:"condition"`
	Severity  string           `json:"severity"`
	Channels  []string         `json:"channels"`
	Source    string           `json:"source,omitempty"`
	Status    string           `json:"status,omitempty"`
	Queue     *QueueRule       `json:"queue,omitempty"`
}

type Preview struct {
	Timestamps []int64   `json:"timestamps"`
	Data       []float64 `json:"data"`
	Th         float64   `json:"th"`
	Episodes   int       `json:"episodes"`
	FiringNow  bool      `json:"firingNow"`
	Now        float64   `json:"now"`
}

type Incident struct {
	ID         string          `json:"id"`
	MonitorID  string          `json:"monitorId"`
	KpiID      string          `json:"kpiId,omitempty"`
	Title      string          `json:"title"`
	Severity   string          `json:"severity"`
	State      string          `json:"state"`
	StartedAt  int64           `json:"startedAt"`
	Value      string          `json:"value"`
	Service    string          `json:"service"`
	Endpoint   string          `json:"endpoint,omitempty"`
	AckedAt    int64           `json:"ackedAt,omitempty"`
	ResolvedAt int64           `json:"resolvedAt,omitempty"`
	PeakValue  *float64        `json:"peakValue,omitempty"`
	RootCause  json.RawMessage `json:"rootCause,omitempty"`
	// Quiet: resolved from pending without ever notifying (hidden unless ?includeQuiet=true).
	Quiet bool `json:"quiet,omitempty"`
}

type IncidentEvent struct {
	ID   string          `json:"id"`
	Ts   int64           `json:"ts"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// ---------------------------------------------------------------- KPIs

type KpiTarget struct {
	Kind       string `json:"kind"` // page | endpoint | journey | app_start
	ServiceID  string `json:"serviceId"`
	Ref        string `json:"ref"`
	EndpointID string `json:"endpointId,omitempty"`
}

// KpiGood is the structured good-event rule stored in kpis.good_when.
type KpiGood struct {
	Metric      string  `json:"metric,omitempty"`      // Duration | LCP | INP | Cold start ...
	ThresholdMs float64 `json:"thresholdMs,omitempty"` // latency SLIs
	StatusBelow int     `json:"statusBelow,omitempty"` // default 500
	NoError     bool    `json:"noError,omitempty"`
	Description string  `json:"description,omitempty"`
}

type KpiAlertRule struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"` // burn_rate | sli_below
	Condition   string   `json:"condition"`
	Severity    string   `json:"severity"`
	Channels    []string `json:"channels"`
	State       string   `json:"state"` // ok | pending | firing | no_data
	LongWindow  string   `json:"longWindow,omitempty"`
	ShortWindow string   `json:"shortWindow,omitempty"`
	Threshold   float64  `json:"threshold"`
	// Set when the alert engine has evaluated the rule (kpi_alert_rules.state); otherwise State is computed by the api.
	StateSince int64    `json:"stateSince,omitempty"`
	LastValue  *float64 `json:"lastValue,omitempty"`
	LastEvalAt int64    `json:"lastEvalAt,omitempty"`
}

type KpiThreshold struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
}

type KpiMetric struct {
	Label    string  `json:"label"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	Unit     string  `json:"unit"`
}

type Kpi struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	Owner           string         `json:"owner"`
	Target          KpiTarget      `json:"target"`
	Sli             string         `json:"sli"`
	GoodWhen        string         `json:"goodWhen"`
	Good            KpiGood        `json:"good"`
	Threshold       *KpiThreshold  `json:"threshold,omitempty"`
	Objective       float64        `json:"objective"`
	Window          string         `json:"window"`
	Current         float64        `json:"current"`
	WindowGood      float64        `json:"windowGood"`
	BudgetRemaining float64        `json:"budgetRemaining"`
	BurnRate1h      float64        `json:"burnRate1h"`
	BurnRate6h      float64        `json:"burnRate6h"`
	Status          Health         `json:"status"`
	Source          string         `json:"source"`
	Alerts          []KpiAlertRule `json:"alerts"`
	DegradedSince   int64          `json:"degradedSince,omitempty"`
	Metric          *KpiMetric     `json:"metric,omitempty"`
	NoData          bool           `json:"noData,omitempty"`
}

type KpiAlertRuleInput struct {
	Kind        string   `json:"kind"`
	LongWindow  string   `json:"longWindow,omitempty"`
	ShortWindow string   `json:"shortWindow,omitempty"`
	Threshold   float64  `json:"threshold"`
	Severity    string   `json:"severity"`
	Channels    []string `json:"channels"`
}

type KpiInput struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Owner       string              `json:"owner"`
	Target      KpiTarget           `json:"target"`
	Sli         string              `json:"sli"`
	Good        KpiGood             `json:"good"`
	Objective   float64             `json:"objective"`
	Window      string              `json:"window"`
	Source      string              `json:"source,omitempty"`
	Alerts      []KpiAlertRuleInput `json:"alerts"`
}

// ---------------------------------------------------------------- infrastructure

type DbNode struct {
	ID          string   `json:"id"`
	Role        string   `json:"role"`
	Zone        string   `json:"zone"`
	CPU         float64  `json:"cpu"`
	Memory      float64  `json:"memory"`
	Connections float64  `json:"connections"`
	ReplLagMs   *float64 `json:"replLagMs,omitempty"`
}

type Problem struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

type Database struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Engine          string   `json:"engine"`
	Version         string   `json:"version"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	Env             string   `json:"env"`
	InstanceClass   string   `json:"instanceClass"`
	Collector       string   `json:"collector"`
	Source          string   `json:"source"`
	Status          Health   `json:"status"`
	CPU             float64  `json:"cpu"`
	PrevCPU         float64  `json:"prevCpu"`
	Memory          float64  `json:"memory"`
	Disk            float64  `json:"disk"`
	DiskGb          float64  `json:"diskGb"`
	Connections     float64  `json:"connections"`
	MaxConnections  float64  `json:"maxConnections"`
	P95             float64  `json:"p95"`
	PrevP95         float64  `json:"prevP95"`
	SlowPerMin      float64  `json:"slowPerMin"`
	OpsPerSec       float64  `json:"opsPerSec"`
	HitRatio        float64  `json:"hitRatio"`
	EvictionsPerMin *float64 `json:"evictionsPerMin,omitempty"`
	Nodes           []DbNode `json:"nodes"`
	// Meta is the cloud provider metadata (databases.meta), e.g. Neon project/branch/endpoint/state/cu*/storage*/activeHours
	Meta            map[string]any `json:"meta"`
	Problem         *Problem       `json:"problem,omitempty"`
	AddedAt         int64          `json:"addedAt,omitempty"`
	Since           int64          `json:"since,omitempty"` // = addedAt (created_at), the UI field name
	User            string         `json:"user,omitempty"`  // monitoring user (default trooper_monitor)
	CredentialsRef  string         `json:"credentialsRef,omitempty"`
	CloudResourceID string         `json:"cloudResourceId,omitempty"`
	NoData          bool           `json:"noData,omitempty"`
	Zone            string         `json:"zone,omitempty"`
	IntegrationID   string         `json:"integrationId,omitempty"`
	// Missing: the cloud resource was not found in the last discovery (meta.missing).
	Missing bool `json:"missing,omitempty"`
}

type DatabaseInput struct {
	ID              string `json:"id,omitempty"` // slug; derived from name when empty
	Name            string `json:"name"`
	Engine          string `json:"engine"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Collector       string `json:"collector"`
	User            string `json:"user,omitempty"` // monitoring user; empty = collector default (trooper_monitor)
	CredentialsRef  string `json:"credentialsRef,omitempty"`
	CloudResourceID string `json:"cloudResourceId,omitempty"`
	Env             string `json:"env,omitempty"`
}

type DbQueryRow struct {
	ID          string  `json:"id"` // query hash as string
	Text        string  `json:"text"`
	ServiceID   string  `json:"serviceId"`
	Endpoint    string  `json:"endpoint"`
	PerCall     float64 `json:"perCall"`
	AvgMs       float64 `json:"avgMs"`
	P95         float64 `json:"p95"`
	Rows        float64 `json:"rows"`
	CallsPerMin float64 `json:"callsPerMin"`
	Share       float64 `json:"share"`
}

type DbUser struct {
	Service Service    `json:"service"`
	Dep     Dependency `json:"dep"`
}

type BrokerNode struct {
	ID              string   `json:"id"`
	Host            string   `json:"host"`
	CPU             float64  `json:"cpu"`
	Memory          float64  `json:"memory"`
	Disk            float64  `json:"disk"`
	NetInMBs        float64  `json:"netInMBs"`
	Leaders         *float64 `json:"leaders,omitempty"`
	UnderReplicated *float64 `json:"underReplicated,omitempty"`
	Status          Health   `json:"status"`
	Note            string   `json:"note,omitempty"`
}

type Cluster struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"` // kafka | rabbitmq | sqs | pubsub
	Version        string          `json:"version"`
	IntegrationID  string          `json:"integrationId,omitempty"`
	Missing        bool            `json:"missing,omitempty"`
	Endpoint       string          `json:"endpoint"`
	Vhost          string          `json:"vhost,omitempty"`
	Source         string          `json:"source"`
	Collector      string          `json:"collector"`
	Nodes          []BrokerNode    `json:"nodes"`
	AddedAt        int64           `json:"addedAt,omitempty"`
	Auth           json.RawMessage `json:"auth,omitempty"`
	CredentialsRef string          `json:"credentialsRef,omitempty"`
	Status         Health          `json:"status"`
	TotalLag       float64         `json:"totalLag"`
	Topics         int             `json:"topics"`
	Groups         int             `json:"groups"`
}

type ClusterInput struct {
	ID             string          `json:"id,omitempty"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Endpoint       string          `json:"endpoint"`
	Vhost          string          `json:"vhost,omitempty"`
	Auth           json.RawMessage `json:"auth,omitempty"`
	CredentialsRef string          `json:"credentialsRef,omitempty"`
	Collector      string          `json:"collector,omitempty"`
}

type Topic struct {
	ClusterID   string   `json:"clusterId"`
	Name        string   `json:"name"`
	Partitions  int      `json:"partitions"`
	InPerSec    float64  `json:"inPerSec"`
	AvgBytes    float64  `json:"avgBytes"`
	Retention   string   `json:"retention"`
	RetentionMs float64  `json:"retentionMs,omitempty"` // raw retention.ms; -1 = unlimited (collector convention)
	Producers   []string `json:"producers"`
	Binding     string   `json:"binding,omitempty"`
	Lag         float64  `json:"lag"`
	Health      Health   `json:"health"`
	// from broker_topics_1m (0 when not reported)
	Replication   float64 `json:"replication,omitempty"`
	SizeBytes     float64 `json:"sizeBytes,omitempty"`
	BytesInPerSec float64 `json:"bytesInPerSec,omitempty"`
}

type GroupRead struct {
	Topic         string    `json:"topic"`
	PartitionLag  []float64 `json:"partitionLag"`
	OldestAgeS    *float64  `json:"oldestAgeS,omitempty"`
	ConsumePerSec float64   `json:"consumePerSec"`
	Unacked       *float64  `json:"unacked,omitempty"`
}

type ConsumerGroup struct {
	ClusterID         string      `json:"clusterId"`
	ID                string      `json:"id"`
	ServiceID         string      `json:"serviceId,omitempty"`
	Process           string      `json:"process,omitempty"`
	External          string      `json:"external,omitempty"`
	Members           float64     `json:"members"`
	State             string      `json:"state"` // Stable | Rebalancing | Empty
	RebalancesPerHour float64     `json:"rebalancesPerHour"`
	Trend             string      `json:"trend"` // growing | steady | shrinking | batch
	Reads             []GroupRead `json:"reads"`
	Note              string      `json:"note,omitempty"`
	Lag               float64     `json:"lag"`
	Health            Health      `json:"health"`
}

type GroupOwnerInput struct {
	ServiceID string `json:"serviceId"`
	Process   string `json:"process,omitempty"`
}

type ConnTestInput struct {
	Kind     string `json:"kind"`               // postgres | mysql | redis | mongodb | kafka | rabbitmq
	Host     string `json:"host,omitempty"`     // databases
	Port     int    `json:"port,omitempty"`     // databases
	Endpoint string `json:"endpoint,omitempty"` // kafka bootstrap servers or rabbitmq management URL
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
	Vhost    string `json:"vhost,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
}

// ---------------------------------------------------------------- integrations (ADR-012)

// DiscoveredResource is a database / cluster / project / queue an integration found.
type DiscoveredResource struct {
	Kind   string `json:"kind"` // database | cluster | project | queue | subscription
	RefID  string `json:"refId,omitempty"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// Integration is a cloud connection. Config never contains secrets (they live in the secrets table).
type Integration struct {
	ID              string               `json:"id"`
	Provider        string               `json:"provider"` // aws | gcp | vercel | neon
	Name            string               `json:"name"`
	Status          string               `json:"status"` // pending | ok | error | disabled
	Error           string               `json:"error,omitempty"`
	LastSyncAt      int64                `json:"lastSyncAt,omitempty"`
	Config          json.RawMessage      `json:"config"`
	Resources       []DiscoveredResource `json:"resources"`
	CreatedAt       int64                `json:"createdAt"`
	ResourceCount   int                  `json:"resourceCount"`
	HasSecret       bool                 `json:"hasSecret"`
	SyncRequestedAt int64                `json:"syncRequestedAt,omitempty"`
}

// IntegrationInput is the UI IntegrationDraft. secret is sent once, sealed, never returned.
type IntegrationInput struct {
	Provider string          `json:"provider"`
	Name     string          `json:"name"`
	Config   json.RawMessage `json:"config"`
	Secret   string          `json:"secret,omitempty"`
}

// IntegrationPatch updates name / config / status; a non-empty secret rotates the stored one.
type IntegrationPatch struct {
	Name   *string         `json:"name,omitempty"`
	Config json.RawMessage `json:"config,omitempty"`
	Secret *string         `json:"secret,omitempty"`
	Status *string         `json:"status,omitempty"` // disabled | pending (re-enable)
}

type IntegrationTestResult struct {
	OK        bool                 `json:"ok"`
	Message   string               `json:"message"`
	Resources []DiscoveredResource `json:"resources,omitempty"`
}

type ConnTestResult struct {
	OK        bool    `json:"ok"`
	Message   string  `json:"message"`
	LatencyMs float64 `json:"latencyMs"`
	Version   string  `json:"version,omitempty"`
}

// CollectorDatabase / CollectorCluster follow services/collector/README.md ("collector-config"): Postgres rows,
// numeric id + slug (the slug is trooper.db.id / trooper.broker.cluster), nullable optional columns.
type CollectorDatabase struct {
	ID              int64   `json:"id"`
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	Engine          string  `json:"engine"`
	Host            string  `json:"host"`
	Port            int     `json:"port"`
	Collector       string  `json:"collector"`
	User            *string `json:"user,omitempty"`
	CredentialsRef  *string `json:"credentialsRef"`
	CloudResourceID *string `json:"cloudResourceId"`
	Env             string  `json:"env"`
}

type CollectorCluster struct {
	ID             int64           `json:"id"`
	Slug           string          `json:"slug"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Endpoint       string          `json:"endpoint"`
	Vhost          *string         `json:"vhost"`
	Auth           json.RawMessage `json:"auth"`
	CredentialsRef *string         `json:"credentialsRef"`
	Collector      string          `json:"collector"`
}

type CollectorConfig struct {
	Interval       int                 `json:"interval"` // seconds
	OrgID          uint32              `json:"orgId"`
	ProjectID      uint32              `json:"projectId"`
	Databases      []CollectorDatabase `json:"databases"`
	BrokerClusters []CollectorCluster  `json:"brokerClusters"`
}

// ---------------------------------------------------------------- load tests

type LoadTestScenario struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Target   string          `json:"target"`
	Source   string          `json:"source"`
	Steps    int             `json:"steps"`
	PeakVus  float64         `json:"peakVus"`
	Duration float64         `json:"duration"` // seconds
	Regions  []string        `json:"regions"`
	LastRun  string          `json:"lastRun,omitempty"`
	Spec     json.RawMessage `json:"spec"`
}

type ScenarioInput struct {
	Name   string          `json:"name"`
	Source string          `json:"source,omitempty"`
	Spec   json.RawMessage `json:"spec"`
}

type Threshold struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

type LoadTestRun struct {
	ID         string      `json:"id"`
	ScenarioID string      `json:"scenarioId"`
	State      string      `json:"state"` // queued | running | passed | failed | aborted
	StartedAt  int64       `json:"startedAt"`
	FinishedAt int64       `json:"finishedAt,omitempty"`
	Progress   float64     `json:"progress"`
	Rps        float64     `json:"rps"`
	P95        float64     `json:"p95"`
	ErrorRate  float64     `json:"errorRate"`
	Vus        float64     `json:"vus"`
	Thresholds []Threshold `json:"thresholds"`
}

// ---------------------------------------------------------------- root cause baseline

// RcaStats are an endpoint's (or process's) totals over one window.
type RcaStats struct {
	Requests  float64 `json:"requests"`
	Rpm       float64 `json:"rpm"`
	AvgMs     float64 `json:"avgMs"`
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	ErrorRate float64 `json:"errorRate"`
}

// RcaChildStats is one dependency (edges_1m) of the endpoint over one window.
type RcaChildStats struct {
	Calls       float64 `json:"calls"`
	CallsPerMin float64 `json:"callsPerMin"`
	CallsPerReq float64 `json:"callsPerReq"`
	MsPerReq    float64 `json:"msPerReq"` // dependency time per endpoint request
	AvgMs       float64 `json:"avgMs"`
	P50         float64 `json:"p50"`
	P95         float64 `json:"p95"`
	ErrorRate   float64 `json:"errorRate"`
}

type RcaChild struct {
	Dep      string        `json:"dep"`
	Kind     string        `json:"kind"` // db | cache | http | queue
	Current  RcaChildStats `json:"current"`
	Baseline RcaChildStats `json:"baseline"`
	// DeltaMsPerReq = current − baseline ms per request; Share = its part of the endpoint's avg latency change
	DeltaMsPerReq float64 `json:"deltaMsPerReq"`
	Share         float64 `json:"share"`
}

// EndpointRca compares an endpoint and each of its dependencies between the range and a baseline window.
type EndpointRca struct {
	Service      string     `json:"service"`
	Endpoint     string     `json:"endpoint"` // "METHOD /route" or process
	From         int64      `json:"from"`
	To           int64      `json:"to"`
	BaselineFrom int64      `json:"baselineFrom"`
	BaselineTo   int64      `json:"baselineTo"`
	Current      RcaStats   `json:"current"`
	Baseline     RcaStats   `json:"baseline"`
	OwnMs        [2]float64 `json:"ownMs"` // own code ms per request [current, baseline] (avg − Σ dependency ms, ≥ 0)
	DeltaAvgMs   float64    `json:"deltaAvgMs"`
	Children     []RcaChild `json:"children"`
}

// ---------------------------------------------------------------- code insights (no backend yet: empty lists)

type CodeSuggestion struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Kind        string  `json:"kind"` // monitor kind
	Target      string  `json:"target"`
	Condition   string  `json:"condition"`
	Criticality string  `json:"criticality"`
	Confidence  float64 `json:"confidence"`
	Rationale   string  `json:"rationale"`
	CodeRef     string  `json:"codeRef"`
	Calibrated  bool    `json:"calibrated"`
}

type Repo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Branch     string   `json:"branch"`
	Sha        string   `json:"sha"`
	AnalyzedAt int64    `json:"analyzedAt"`
	Services   []string `json:"services"`
	Language   string   `json:"language"`
	Status     string   `json:"status"` // ready | analyzing | error
}

type InventoryItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // endpoint | consumer | cron | view | dependency
	Key         string `json:"key"`
	Source      string `json:"source,omitempty"`
	Service     string `json:"service"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Criticality string `json:"criticality"`
	Observed    bool   `json:"observed"`
	Monitored   bool   `json:"monitored"`
}
