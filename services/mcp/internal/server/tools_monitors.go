package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Monitor and KPI writes (ADR-032). tt_preview_monitor is read-only and always registered. The write tools are
// registered only with TT_MCP_ALLOW_WRITE and change nothing unless called with confirm: true: without it they
// return a dry run (the exact body that would be sent) so the agent can show it to the user first. Everything an
// agent creates carries source "agent"; delete only touches monitors / KPIs with that source.

const agentSource = "agent"

// monitorBody is MonitorInput (contracts §8, openapi MonitorInput). Query is kind specific and passed through.
type monitorBody struct {
	Name      string         `json:"name" jsonschema:"Short human name, e.g. \"tt-api p95 latency\""`
	Kind      string         `json:"kind" jsonschema:"latency | error_rate | throughput | apdex | metric_threshold | anomaly | error_new | log_count | log_pattern | job_missed | consumer_lag | dlq | rum_vital | crash_rate | composite | infra | faas | llm (query shapes: the trooper-alerts skill)"`
	Target    monitorTarget  `json:"target"`
	Query     map[string]any `json:"query,omitempty" jsonschema:"Kind specific. latency {percentile: p50|p95|p99}; error_rate {statusMin?: 500}; throughput {direction: rpm|drop|spike|change}; apdex {tMs: 500}; anomaly {metric: error_rate|p95|rpm, direction: up|down|both, minDeviation?, weeks?: 4}; error_new {lookback?: 30d}; log_count {minSeverity: error, contains?}; log_pattern {} (new patterns); job_missed {intervalMin, graceMin, process?}; dlq {metric: dlq|failed|failure_rate, process?}; rum_vital {vital: LCP|INP|CLS, percentile: p75, view?}; crash_rate {per: session}; composite {op: and|or, monitorIds}; infra {entity: host|node|cluster|container|pod|workload|database, metric, agg?: max|avg, filter?} — host/node/cluster: cpu|memory|disk (0..1; host also load1, node also not_ready; cluster = mean over its nodes), filter {cluster?, host?(node name for node)}; container/pod: cpu_ratio|memory_ratio|restarts|throttled (pod also not_ready); workload: unavailable; database: cpu|memory|disk|connections_ratio (0..1)|p95_ms|repl_lag_ms|hit_ratio (op <)|evictions|slow, filter {database?: id from tt_list_databases, engine?}; faas / llm: copy from tt_list_monitor_templates"`
	Condition monitorCond    `json:"condition"`
	Severity  string         `json:"severity" jsonschema:"critical | warning | info"`
	Channels  []string       `json:"channels" jsonschema:"Targets copied from tt_list_notify_channels (slack:<id>#<name>, a configured channel name, email:a@b.c). Empty list = incidents only, no notification"`
	Status    string         `json:"status,omitempty" jsonschema:"active (default) or draft (saved but not evaluated)"`
	Queue     map[string]any `json:"queue,omitempty" jsonschema:"consumer_lag only: {clusterId, group, metric: lag|time_behind|partition_lag|growing|not_drained|rebalances|oldest_age, threshold, forMin} (clusterId + group from tt_list_consumer_groups)"`
	Source    string         `json:"source,omitempty" jsonschema:"Ignored: set to agent by the server"`
}

type monitorTarget struct {
	Service  string `json:"service,omitempty" jsonschema:"Service name from tt_list_services"`
	Endpoint string `json:"endpoint,omitempty" jsonschema:"\"METHOD /route\" from tt_list_endpoints (not the slug); omit for the whole service"`
	Process  string `json:"process,omitempty" jsonschema:"job_missed: the cron / worker process that must report"`
}

type monitorCond struct {
	Op          string   `json:"op" jsonschema:"> >= < <= (apdex: <)"`
	Threshold   float64  `json:"threshold" jsonschema:"Rates are fractions (2% error rate = 0.02); latency in ms; apdex 0..1; anomaly: z-score (3 = default); job_missed: ignored (derived from intervalMin + graceMin), pass 0; counts for error_new / log_* / dlq"`
	ForMin      *int     `json:"forMin,omitempty" jsonschema:"Minutes the condition must hold before firing (0 = first evaluation). 5 is a good default"`
	MinRequests *float64 `json:"minRequests,omitempty" jsonschema:"Skip windows with fewer requests (rates and latency; avoids alerts on 2 requests)"`
	Window      string   `json:"window,omitempty" jsonschema:"Window the value is measured over, re-checked every minute: error rate / p95 / rpm / counts of the LAST window, e.g. 5m = \"error rate over the last 5 minutes > 2%\". 1m, 5m, 10m, 15m, 30m, 1h. Default 5m (anomaly 15m); ignored by job_missed, error_new, composite, consumer_lag"`
	RenotifyMin *int     `json:"renotifyMin,omitempty" jsonschema:"Remind every N minutes while firing and unacknowledged"`
}

func (b *monitorBody) check() error {
	b.Name = strings.TrimSpace(b.Name)
	switch {
	case b.Name == "":
		return toolErrf("name is required")
	case b.Kind == "":
		return toolErrf("kind is required")
	case b.Severity != "critical" && b.Severity != "warning" && b.Severity != "info":
		return toolErrf("severity must be critical, warning or info")
	case b.Condition.Op == "":
		return toolErrf("condition.op is required (> or <)")
	case b.Status != "" && b.Status != "active" && b.Status != "draft":
		return toolErrf("status must be active or draft")
	}
	if b.Channels == nil {
		b.Channels = []string{}
	}
	if b.Condition.Window == "" && windowed(b.Kind) { // state the window explicitly, as the editor does
		b.Condition.Window = "5m"
		if b.Kind == "anomaly" {
			b.Condition.Window = "15m"
		}
	}
	b.Source = agentSource
	return nil
}

// windowed reports whether a kind measures its value over condition.window.
func windowed(kind string) bool {
	switch kind {
	case "job_missed", "error_new", "composite", "consumer_lag":
		return false
	}
	return true
}

// ---- preview (read-only, always registered) ----

func (s *Server) registerMonitorPreview(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "tt_preview_monitor",
		Description: "Replay an UNSAVED monitor over past data and report how often it would have fired (episodes), whether it fires now and the metric series. " +
			"Call it for every monitor before proposing or creating it: 0 episodes on a noisy metric or 20 episodes a day both mean the threshold is wrong. " +
			"Supports metric_threshold, latency, error_rate, throughput, log_*, dlq, consumer_lag, consumer_failures, rum_vital, crash_rate and llm; other kinds return an error and are evaluated only after saving.",
	}, s.previewMonitor)
}

type previewInput struct {
	RangeInput
	Monitor monitorBody `json:"monitor" jsonschema:"The monitor exactly as it would be passed to tt_create_monitor"`
}

func (s *Server) previewMonitor(ctx context.Context, _ *mcp.CallToolRequest, in previewInput) (*mcp.CallToolResult, any, error) {
	if in.Range == nil {
		in.Range = ptr("24h")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	if err := in.Monitor.check(); err != nil {
		return nil, nil, err
	}
	var doc map[string]any
	if err := s.api.Send(ctx, http.MethodPost, "/api/v1/monitors/preview", q, in.Monitor, &doc); err != nil {
		return nil, nil, err
	}
	// the per-bucket series is long; keep the verdict and a short tail
	if data, ok := doc["data"].([]any); ok && len(data) > 60 {
		doc["data"] = data[len(data)-60:]
		if ts, ok := doc["timestamps"].([]any); ok && len(ts) > 60 {
			doc["timestamps"] = ts[len(ts)-60:]
		}
		doc["note"] = "data / timestamps trimmed to the last 60 buckets; episodes and firingNow cover the whole range"
	}
	return shaped(doc, nil, "")
}

// ---- writes (TT_MCP_ALLOW_WRITE) ----

func (s *Server) registerMonitorWriteTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "tt_create_monitor",
		Description: "Create a monitor (alert rule). WRITE: without confirm: true it only returns the body it would send (dry run). " +
			"Only set confirm after the user approved this exact monitor. Check tt_list_monitors first to avoid duplicates and tt_preview_monitor for noise. Created with source agent.",
	}, s.createMonitor)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "tt_update_monitor",
		Description: "Change a monitor's name, severity, channels, status (active | muted | draft), condition (threshold, measured window, forMin, minRequests), or mute it until a time. " +
			"The condition is replaced as a whole: send every field, including window. WRITE: dry run unless confirm: true. " +
			"Repository-managed monitors (source code) are refused.",
	}, s.updateMonitor)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_delete_monitor",
		Description: "Delete a monitor that an agent created (source agent); others are refused, mute them with tt_update_monitor instead. WRITE: dry run unless confirm: true.",
	}, s.deleteMonitor)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_accept_suggestion",
		Description: "Turn a code-analysis suggestion (tt_list_alert_suggestions) into a monitor, optionally overriding its threshold, severity or channels. WRITE: dry run unless confirm: true.",
	}, s.acceptSuggestion)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "tt_create_kpi",
		Description: "Create a KPI (SLO) with burn-rate alerts on an endpoint, page, journey or app start. WRITE: dry run unless confirm: true. " +
			"Check tt_list_kpis first. Created with source agent.",
	}, s.createKpi)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_update_kpi",
		Description: "Replace a KPI (objective, good-event rule, window, alerts). Pass the full KPI as for tt_create_kpi. WRITE: dry run unless confirm: true.",
	}, s.updateKpi)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_delete_kpi",
		Description: "Delete a KPI that an agent created (source agent); others are refused. WRITE: dry run unless confirm: true.",
	}, s.deleteKpi)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_send_slack_test",
		Description: "Post a test message to a Slack channel to check the bot can reach it before routing alerts there (fails readably when a private channel lacks /invite). WRITE: dry run unless confirm: true.",
	}, s.sendSlackTest)
}

type confirmField struct {
	Confirm bool `json:"confirm,omitempty" jsonschema:"true only after the user approved this exact change; false or omitted = dry run, nothing is written"`
}

// dryRun is what a write tool returns without confirm.
func dryRun(method, path string, body any) (*mcp.CallToolResult, any, error) {
	return shaped(map[string]any{
		"dryRun": true, "wouldSend": map[string]any{"method": method, "path": path, "body": body},
		"next": "show this to the user; call again with confirm: true once they approve",
	}, nil, "")
}

type createMonitorInput struct {
	confirmField
	Monitor monitorBody `json:"monitor"`
}

func (s *Server) createMonitor(ctx context.Context, _ *mcp.CallToolRequest, in createMonitorInput) (*mcp.CallToolResult, any, error) {
	if err := in.Monitor.check(); err != nil {
		return nil, nil, err
	}
	if !in.Confirm {
		return dryRun(http.MethodPost, "/api/v1/monitors", in.Monitor)
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPost, "/api/v1/monitors", nil, in.Monitor, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"created": doc}, nil, "")
}

type monitorPatch struct {
	Name       *string      `json:"name,omitempty"`
	Severity   *string      `json:"severity,omitempty" jsonschema:"critical | warning | info"`
	Channels   []string     `json:"channels,omitempty" jsonschema:"Replaces the whole list"`
	Status     *string      `json:"status,omitempty" jsonschema:"active | muted | draft"`
	Condition  *monitorCond `json:"condition,omitempty" jsonschema:"Replaces the whole condition"`
	MutedUntil *int64       `json:"mutedUntil,omitempty" jsonschema:"Mute until this unix ms; 0 unmutes"`
}

type updateMonitorInput struct {
	confirmField
	ID    string       `json:"id" jsonschema:"Monitor id from tt_list_monitors"`
	Patch monitorPatch `json:"patch"`
}

func (s *Server) updateMonitor(ctx context.Context, _ *mcp.CallToolRequest, in updateMonitorInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, toolErrf("id is required (from tt_list_monitors)")
	}
	p := in.Patch
	if p.Severity != nil && *p.Severity != "critical" && *p.Severity != "warning" && *p.Severity != "info" {
		return nil, nil, toolErrf("severity must be critical, warning or info")
	}
	if p.Status != nil && *p.Status != "active" && *p.Status != "muted" && *p.Status != "draft" {
		return nil, nil, toolErrf("status must be active, muted or draft")
	}
	if p.Name == nil && p.Severity == nil && p.Channels == nil && p.Status == nil && p.Condition == nil && p.MutedUntil == nil {
		return nil, nil, toolErrf("patch is empty: set at least one field")
	}
	src, err := s.sourceOf(ctx, "/api/v1/monitors/"+url.PathEscape(id))
	if err != nil {
		return nil, nil, err
	}
	if src == "code" {
		return nil, nil, toolErrf("monitor %s is managed in a repository (.trooper/monitors/*.yaml): change the YAML instead", id)
	}
	path := "/api/v1/monitors/" + url.PathEscape(id)
	if !in.Confirm {
		return dryRun(http.MethodPatch, path, p)
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPatch, path, nil, p, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"updated": doc}, nil, "")
}

type idConfirmInput struct {
	confirmField
	ID string `json:"id" jsonschema:"Id from the matching list tool"`
}

func (s *Server) deleteMonitor(ctx context.Context, _ *mcp.CallToolRequest, in idConfirmInput) (*mcp.CallToolResult, any, error) {
	return s.deleteOwned(ctx, "monitor", "/api/v1/monitors/", in)
}

func (s *Server) deleteKpi(ctx context.Context, _ *mcp.CallToolRequest, in idConfirmInput) (*mcp.CallToolResult, any, error) {
	return s.deleteOwned(ctx, "KPI", "/api/v1/kpis/", in)
}

// deleteOwned deletes a monitor / KPI only when an agent created it.
func (s *Server) deleteOwned(ctx context.Context, what, base string, in idConfirmInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, toolErrf("id is required")
	}
	path := base + url.PathEscape(id)
	src, err := s.sourceOf(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	if src != agentSource {
		return nil, nil, toolErrf("%s %s was not created by an agent (source %q); only the user can delete it", what, id, src)
	}
	if !in.Confirm {
		return dryRun(http.MethodDelete, path, nil)
	}
	if err := s.api.Send(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"deleted": id}, nil, "")
}

// sourceOf reads the source field of one monitor / KPI.
func (s *Server) sourceOf(ctx context.Context, path string) (string, error) {
	var doc map[string]any
	if err := s.api.Get(ctx, path, nil, &doc); err != nil {
		return "", err
	}
	src, _ := doc["source"].(string)
	return src, nil
}

type acceptSuggestionInput struct {
	confirmField
	ID        string   `json:"id" jsonschema:"Suggestion id from tt_list_alert_suggestions"`
	Threshold *float64 `json:"threshold,omitempty" jsonschema:"Override the drafted threshold"`
	Severity  string   `json:"severity,omitempty" jsonschema:"Override: critical | warning | info"`
	Channels  []string `json:"channels,omitempty" jsonschema:"Override the notification targets"`
}

func (s *Server) acceptSuggestion(ctx context.Context, _ *mcp.CallToolRequest, in acceptSuggestionInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, toolErrf("id is required (from tt_list_alert_suggestions)")
	}
	if in.Severity != "" && in.Severity != "critical" && in.Severity != "warning" && in.Severity != "info" {
		return nil, nil, toolErrf("severity must be critical, warning or info")
	}
	body := map[string]any{}
	if in.Threshold != nil {
		body["threshold"] = *in.Threshold
	}
	if in.Severity != "" {
		body["severity"] = in.Severity
	}
	if in.Channels != nil {
		body["channels"] = in.Channels
	}
	path := "/api/v1/suggestions/" + url.PathEscape(id) + "/accept"
	if !in.Confirm {
		return dryRun(http.MethodPost, path, body)
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPost, path, nil, body, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"accepted": doc}, nil, "")
}

// ---- KPIs ----

type kpiBody struct {
	Name        string         `json:"name" jsonschema:"User-facing outcome, e.g. \"Checkout succeeds\""`
	Description string         `json:"description" jsonschema:"One sentence: what a good event is and why it matters"`
	Owner       string         `json:"owner" jsonschema:"Team or person, free text"`
	Target      kpiTarget      `json:"target"`
	Sli         string         `json:"sli" jsonschema:"latency (fast and not 5xx) | availability | success_rate (not 5xx)"`
	Good        kpiGood        `json:"good,omitempty" jsonschema:"Good-event rule; thresholdMs is required for latency"`
	Objective   float64        `json:"objective" jsonschema:"Share of good events, between 0 and 1 exclusive, e.g. 0.99"`
	Window      string         `json:"window" jsonschema:"7d or 28d"`
	Alerts      []kpiAlertRule `json:"alerts" jsonschema:"Usually two burn-rate rules: fast (longWindow 1h, shortWindow 5m, threshold 14.4, critical) and slow (6h / 30m, 6, warning)"`
	Source      string         `json:"source,omitempty" jsonschema:"Ignored: set to agent by the server"`
}

type kpiTarget struct {
	Kind      string `json:"kind" jsonschema:"endpoint | page | journey | app_start"`
	ServiceID string `json:"serviceId" jsonschema:"Service (endpoint) or app (page, app_start) name"`
	Ref       string `json:"ref" jsonschema:"endpoint: \"METHOD /route\" from tt_list_endpoints; page: the path, e.g. /checkout"`
}

type kpiGood struct {
	ThresholdMs float64 `json:"thresholdMs,omitempty" jsonschema:"latency SLI: a good event is at or under this many ms"`
	StatusBelow int     `json:"statusBelow,omitempty" jsonschema:"Good when the status is below this (default 500; 400 also counts 4xx as bad)"`
	NoError     bool    `json:"noError,omitempty" jsonschema:"page KPIs: also require no JS error"`
	Metric      string  `json:"metric,omitempty" jsonschema:"page KPIs: the vital, e.g. lcp"`
	Description string  `json:"description,omitempty" jsonschema:"Plain-language good-event rule, e.g. \"≤ 300 ms and status < 500\""`
}

type kpiAlertRule struct {
	Kind        string   `json:"kind" jsonschema:"burn_rate | sli_below"`
	LongWindow  string   `json:"longWindow" jsonschema:"e.g. 1h, 6h"`
	ShortWindow string   `json:"shortWindow,omitempty" jsonschema:"burn_rate: e.g. 5m, 30m"`
	Threshold   float64  `json:"threshold" jsonschema:"burn_rate: multiple of the sustainable rate (14.4 = budget gone in ~2 days of a 28d window); sli_below: the SLI fraction"`
	Severity    string   `json:"severity" jsonschema:"critical | warning | info"`
	Channels    []string `json:"channels" jsonschema:"Same targets as monitors"`
}

func (k *kpiBody) check() error {
	k.Name = strings.TrimSpace(k.Name)
	switch {
	case k.Name == "":
		return toolErrf("name is required")
	case k.Sli != "latency" && k.Sli != "availability" && k.Sli != "success_rate":
		return toolErrf("sli must be latency, availability or success_rate")
	case k.Objective <= 0 || k.Objective >= 1:
		return toolErrf("objective must be between 0 and 1 (exclusive), e.g. 0.99")
	case k.Window != "7d" && k.Window != "28d":
		return toolErrf("window must be 7d or 28d")
	case k.Target.ServiceID == "" || k.Target.Ref == "":
		return toolErrf("target.serviceId and target.ref are required")
	case k.Target.Kind == "endpoint" && !strings.Contains(k.Target.Ref, " "):
		return toolErrf("endpoint target.ref must be \"METHOD /route\" (from tt_list_endpoints), not a slug")
	case k.Sli == "latency" && k.Good.ThresholdMs <= 0:
		return toolErrf("latency KPIs need good.thresholdMs")
	}
	for i := range k.Alerts {
		if k.Alerts[i].Channels == nil {
			k.Alerts[i].Channels = []string{}
		}
	}
	if k.Alerts == nil {
		k.Alerts = []kpiAlertRule{}
	}
	k.Source = agentSource
	return nil
}

type createKpiInput struct {
	confirmField
	Kpi kpiBody `json:"kpi"`
}

func (s *Server) createKpi(ctx context.Context, _ *mcp.CallToolRequest, in createKpiInput) (*mcp.CallToolResult, any, error) {
	if err := in.Kpi.check(); err != nil {
		return nil, nil, err
	}
	if !in.Confirm {
		return dryRun(http.MethodPost, "/api/v1/kpis", in.Kpi)
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPost, "/api/v1/kpis", nil, in.Kpi, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"created": doc}, nil, "")
}

type updateKpiInput struct {
	confirmField
	ID  string  `json:"id" jsonschema:"KPI id from tt_list_kpis"`
	Kpi kpiBody `json:"kpi" jsonschema:"The full KPI (replaces it)"`
}

func (s *Server) updateKpi(ctx context.Context, _ *mcp.CallToolRequest, in updateKpiInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, toolErrf("id is required (from tt_list_kpis)")
	}
	if err := in.Kpi.check(); err != nil {
		return nil, nil, err
	}
	path := "/api/v1/kpis/" + url.PathEscape(id)
	src, err := s.sourceOf(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	if src == "code" {
		return nil, nil, toolErrf("KPI %s is managed in a repository: change it there", id)
	}
	in.Kpi.Source = src // keep who created it
	if !in.Confirm {
		return dryRun(http.MethodPut, path, in.Kpi)
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPut, path, nil, in.Kpi, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"updated": doc}, nil, "")
}

func ptr[T any](v T) *T { return &v }

// ---- Slack test ----

type slackTestInput struct {
	confirmField
	Channel string `json:"channel" jsonschema:"Slack channel id (C… / G…) or a slack:<id>#<name> target from tt_list_notify_channels"`
}

func (s *Server) sendSlackTest(ctx context.Context, _ *mcp.CallToolRequest, in slackTestInput) (*mcp.CallToolResult, any, error) {
	ch := strings.TrimSpace(in.Channel)
	ch = strings.TrimPrefix(ch, "slack:")
	if i := strings.IndexByte(ch, '#'); i > 0 {
		ch = ch[:i]
	}
	ch = strings.TrimPrefix(ch, "#")
	if ch == "" {
		return nil, nil, toolErrf("channel is required (an id from tt_list_notify_channels)")
	}
	body := map[string]string{"channel": ch}
	if !in.Confirm {
		return dryRun(http.MethodPost, "/api/v1/integrations/slack/test", body)
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPost, "/api/v1/integrations/slack/test", nil, body, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, nil, "")
}
