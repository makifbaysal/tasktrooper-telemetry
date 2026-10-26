package flatten

import (
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
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
	for _, rl := range req.GetResourceLogs() {
		res := newResource(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				out = append(out, logRow(t, &res, lr, received))
			}
		}
	}
	return out
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
	r.Attrs = attrMap(lr.GetAttributes(), 0)
	return r
}

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
