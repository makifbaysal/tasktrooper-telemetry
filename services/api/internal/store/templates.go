package store

// MonitorTemplate is a ready-made monitor (contracts §20, §21, §25): kind + query + condition as stored in monitors.
// Create one with POST /monitors (source = 'template').
type MonitorTemplate struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Kind        string         `json:"kind"`
	Severity    string         `json:"severity"`
	Query       map[string]any `json:"query"`
	Condition   map[string]any `json:"condition"`
}

// MonitorTemplates is a copy of services/alerting/internal/eval/templates.go (eval.Templates; the alerting module is
// not a dependency of the api). TestMonitorTemplatesInSync fails when the ids drift apart.
var MonitorTemplates = []MonitorTemplate{
	{ID: "k8s-pod-crashloop", Name: "Pod restarting (CrashLoop)", Description: "A pod restarted more than 3 times in 10 minutes",
		Kind: "infra", Severity: "critical",
		Query:     map[string]any{"entity": "pod", "metric": "restarts", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 3, "window": "10m", "forMin": 0}},
	{ID: "k8s-node-not-ready", Name: "Node NotReady", Description: "At least one Kubernetes node is NotReady for 5 minutes",
		Kind: "infra", Severity: "critical",
		Query:     map[string]any{"entity": "node", "metric": "not_ready", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0, "window": "5m", "forMin": 5}},
	{ID: "host-cpu-high", Name: "Host CPU > 90%", Description: "A host's CPU is above 90 % for 10 minutes",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "host", "metric": "cpu", "agg": "avg", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.9, "window": "5m", "forMin": 10}},
	{ID: "host-disk-full", Name: "Host disk > 90%", Description: "A host's fullest filesystem is above 90 %",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "host", "metric": "disk", "agg": "max", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.9, "window": "5m", "forMin": 5}},
	{ID: "k8s-workload-unavailable", Name: "Workload has unavailable replicas", Description: "desired − available > 0 for 10 minutes",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "workload", "metric": "unavailable", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0, "window": "5m", "forMin": 10}},
	{ID: "faas-error-rate", Name: "Function error rate > 5%", Description: "A function's errors / invocations is above 5 % for 5 minutes",
		Kind: "faas", Severity: "critical",
		Query:     map[string]any{"metric": "error_rate", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.05, "window": "5m", "forMin": 5, "minRequests": 20}},
	{ID: "faas-throttles", Name: "Function throttled", Description: "A function was throttled in the last 5 minutes",
		Kind: "faas", Severity: "warning",
		Query:     map[string]any{"metric": "throttles", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0, "window": "5m", "forMin": 5}},
	{ID: "faas-near-timeout", Name: "Function p95 near timeout", Description: "p95 duration above 80 % of the function's timeout",
		Kind: "faas", Severity: "warning",
		Query:     map[string]any{"metric": "duration_p95", "relativeToTimeout": true, "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.8, "window": "5m", "forMin": 5}},
	{ID: "llm-cost-per-hour", Name: "LLM cost above $20/hour", Description: "Estimated LLM spend (priced models) above $20 over the last hour",
		Kind: "llm", Severity: "warning",
		Query:     map[string]any{"metric": "cost_usd_per_hour", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 20, "window": "1h", "forMin": 0}},
	{ID: "llm-error-rate", Name: "LLM error rate > 5%", Description: "More than 5 % of LLM calls failed over 15 minutes (at least 20 calls)",
		Kind: "llm", Severity: "critical",
		Query:     map[string]any{"metric": "error_rate", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.05, "window": "15m", "forMin": 0, "minRequests": 20}},
	{ID: "llm-p95-slow", Name: "LLM p95 > 20 s", Description: "p95 LLM call duration above 20 seconds over 15 minutes",
		Kind: "llm", Severity: "warning",
		Query:     map[string]any{"metric": "p95_ms", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 20000, "window": "15m", "forMin": 0, "minRequests": 20}},
	{ID: "llm-token-spike", Name: "Token spike", Description: "More than 5M tokens (input + output) in an hour",
		Kind: "llm", Severity: "warning",
		Query:     map[string]any{"metric": "tokens", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 5000000, "window": "1h", "forMin": 0}},
}
