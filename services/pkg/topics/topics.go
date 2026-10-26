// Package topics names the Redpanda topics and record headers shared by ingest-gw and ingest-writer.
package topics

const (
	Traces  = "tt.otlp.traces"
	Metrics = "tt.otlp.metrics"
	Logs    = "tt.otlp.logs"
	RUM     = "tt.rum"

	WriterGroup = "tt-ingest-writer"

	HeaderOrgID      = "org_id"
	HeaderProjectID  = "project_id"
	HeaderReceivedAt = "received_at" // unix ms
	HeaderClientIP   = "client_ip"
	HeaderUserAgent  = "user_agent"
)

// All lists every ingest topic.
var All = []string{Traces, Metrics, Logs, RUM}
