package eval

// Template is a ready-made monitor definition (contracts §20, §21, §25): kind + query + condition as stored in
// monitors.kind / query / condition. The UI / api create monitors from them with source = 'template'.
type Template struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Kind        string         `json:"kind"`
	Severity    string         `json:"severity"`
	Query       map[string]any `json:"query"`
	Condition   map[string]any `json:"condition"`
}

// Templates lists the infra, serverless and LLM monitor templates.
var Templates = []Template{
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
	{ID: "host-memory-high", Name: "Host memory > 90%", Description: "A host's memory is above 90 % for 10 minutes",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "host", "metric": "memory", "agg": "avg", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.9, "window": "5m", "forMin": 10}},
	{ID: "k8s-node-disk-full", Name: "Node disk > 85%", Description: "A Kubernetes node's fullest filesystem is above 85 %",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "node", "metric": "disk", "agg": "max", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.85, "window": "5m", "forMin": 5}},
	{ID: "k8s-cluster-disk-high", Name: "Cluster disk > 80%", Description: "Average disk use across a cluster's nodes is above 80 % for 15 minutes",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "cluster", "metric": "disk", "agg": "avg", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.8, "window": "15m", "forMin": 15}},
	{ID: "db-memory-high", Name: "Database memory > 90%", Description: "A database node's memory is above 90 % for 10 minutes",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "database", "metric": "memory", "agg": "avg", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.9, "window": "5m", "forMin": 10}},
	{ID: "db-disk-full", Name: "Database disk > 85%", Description: "A database node's disk is above 85 %",
		Kind: "infra", Severity: "critical",
		Query:     map[string]any{"entity": "database", "metric": "disk", "agg": "max", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.85, "window": "5m", "forMin": 5}},
	{ID: "db-connections-high", Name: "Database connections > 80% of max", Description: "Open connections above 80 % of max_connections for 5 minutes",
		Kind: "infra", Severity: "warning",
		Query:     map[string]any{"entity": "database", "metric": "connections_ratio", "agg": "max", "filter": map[string]any{}},
		Condition: map[string]any{"op": ">", "threshold": 0.8, "window": "5m", "forMin": 5}},
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
