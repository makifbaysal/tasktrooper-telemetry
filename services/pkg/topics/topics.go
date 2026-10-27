// Package topics names the Redpanda topics and record headers shared by ingest-gw and ingest-writer.
package topics

const (
	Traces   = "tt.otlp.traces"
	Metrics  = "tt.otlp.metrics"
	Logs     = "tt.otlp.logs"
	RUM      = "tt.rum"
	Replay   = "tt.replay"   // gzip'ed replay segment JSON (contracts §23)
	Profiles = "tt.profiles" // raw profile body: pprof (maybe gzip'ed) or folded text (contracts §24)

	WriterGroup = "tt-ingest-writer"

	HeaderOrgID      = "org_id"
	HeaderProjectID  = "project_id"
	HeaderReceivedAt = "received_at" // unix ms
	HeaderClientIP   = "client_ip"
	HeaderUserAgent  = "user_agent"

	// tt.replay
	HeaderSessionID = "session_id"
	HeaderSegment   = "segment"

	// tt.profiles (values as received in the /v1/profiles query string)
	HeaderService = "service"
	HeaderEnv     = "env"
	HeaderVersion = "version"
	HeaderHost    = "host"
	HeaderRuntime = "runtime"
	HeaderStart   = "start" // unix ms
	HeaderEnd     = "end"   // unix ms
	HeaderFormat  = "format"
	HeaderType    = "type"
	HeaderUnit    = "unit"
)

// All lists every ingest topic.
var All = []string{Traces, Metrics, Logs, RUM, Replay, Profiles}

// MaxLargeRecordBytes is the record size limit of the blob topics (Replay, Profiles): their topics are created with
// max.message.bytes above this, the other topics keep the 1 MiB broker default.
const MaxLargeRecordBytes = 8 << 20
