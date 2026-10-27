package flatten

import (
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/tasktrooper/telemetry/services/ingest/internal/norm"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

const maxLogBody = 64 << 10

// LogRow is one row of telemetry.logs.
type LogRow struct {
	OrgID, ProjectID uint32
	TS               time.Time
	Service, Env     string
	Severity         string
	Body             string
	TraceID, SpanID  []byte
	Route            string
	PatternID        uint64
	Attrs            map[string]string
}

// LogColumns is the INSERT column list matching LogRow.Values.
const LogColumns = "org_id, project_id, ts, service, env, severity, body, trace_id, span_id, route, pattern_id, attrs"

// Values returns the row in LogColumns order.
func (r *LogRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.TS, r.Service, r.Env, r.Severity, r.Body, r.TraceID, r.SpanID, r.Route, r.PatternID, r.Attrs}
}

// Logs appends one LogRow per OTLP log record. received is used when a record carries no timestamp.
func Logs(t tenant.Tenant, req *collogspb.ExportLogsServiceRequest, received time.Time, out []LogRow) []LogRow {
	out, _ = LogsWithContent(t, req, received, out, nil)
	return out
}

// LogsWithContent is Logs that also turns gen_ai.client.inference.operation.details events into llm_content rows
// (contracts §25); their content is removed from the log row (attributes, and a kvlist body is replaced by the event name).
func LogsWithContent(t tenant.Tenant, req *collogspb.ExportLogsServiceRequest, received time.Time, out []LogRow, content []LLMContentRow) ([]LogRow, []LLMContentRow) {
	for _, rl := range req.GetResourceLogs() {
		res := newResource(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				row := logRow(t, &res, lr, received)
				if isLLMDetailsEvent(lr) {
					c := llmContentFromLog(lr.GetAttributes(), lr.GetBody(), row.Attrs)
					if _, kv := lr.GetBody().GetValue().(*commonpb.AnyValue_KvlistValue); kv {
						row.Body = LLMDetailsEvent
						row.PatternID = norm.PatternID(row.Body)
					}
					if !c.empty() {
						c.OrgID, c.ProjectID, c.TS, c.TraceID, c.SpanID, c.Service = row.OrgID, row.ProjectID, row.TS, row.TraceID, row.SpanID, row.Service
						content = append(content, c)
					}
				}
				out = append(out, row)
			}
		}
	}
	return out, content
}

func isLLMDetailsEvent(lr *logspb.LogRecord) bool {
	if lr.GetEventName() == LLMDetailsEvent {
		return true
	}
	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() == "event.name" {
			return kv.GetValue().GetStringValue() == LLMDetailsEvent
		}
	}
	return false
}

func logRow(t tenant.Tenant, res *resourceInfo, lr *logspb.LogRecord, received time.Time) LogRow {
	a := attrIndex(lr.GetAttributes())
	r := LogRow{
		OrgID: t.OrgID, ProjectID: t.ProjectID,
		Service: res.service, Env: res.env,
		Severity: Severity(lr.GetSeverityNumber(), lr.GetSeverityText()),
		Body:     capValue(anyString(lr.GetBody()), maxLogBody),
		TraceID:  fixedID(lr.GetTraceId(), 16),
		SpanID:   fixedID(lr.GetSpanId(), 8),
		Route:    a.str("trooper.entry.route"),
	}
	switch {
	case lr.GetTimeUnixNano() > 0:
		r.TS = time.Unix(0, int64(lr.GetTimeUnixNano())).UTC()
	case lr.GetObservedTimeUnixNano() > 0:
		r.TS = time.Unix(0, int64(lr.GetObservedTimeUnixNano())).UTC()
	default:
		r.TS = received
	}
	r.PatternID = norm.PatternID(r.Body)
	r.Attrs = attrMap(lr.GetAttributes(), len(logResourceKeys))
	// Kubernetes placement lives on the resource (trooper-agent Warning events: service.name=k8s + k8s.cluster.name;
	// apps with OTEL_RESOURCE_ATTRIBUTES k8s.*); logs keep no resource map, so copy it unless the record sets it.
	if len(res.attrs) > 0 {
		for _, k := range logResourceKeys {
			if _, ok := r.Attrs[k]; !ok {
				putAttr(r.Attrs, k, res.attrs[k], MaxAttrValueLen)
			}
		}
	}
	return r
}

// logResourceKeys are resource attributes copied into logs.attrs (contracts §20).
var logResourceKeys = []string{"k8s.cluster.name", "k8s.namespace.name", "k8s.pod.name", "k8s.node.name"}

// Severity maps OTLP severity number (or text when the number is unset) to the logs.severity enum.
func Severity(n logspb.SeverityNumber, text string) string {
	switch {
	case n >= 21:
		return "fatal"
	case n >= 17:
		return "error"
	case n >= 13:
		return "warn"
	case n >= 9:
		return "info"
	case n >= 5:
		return "debug"
	case n >= 1:
		return "trace"
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "trace", "finest", "finer":
		return "trace"
	case "debug", "fine", "verbose":
		return "debug"
	case "warn", "warning":
		return "warn"
	case "error", "err", "severe":
		return "error"
	case "fatal", "critical", "crit", "panic", "emergency", "emerg", "alert":
		return "fatal"
	}
	return "info"
}
