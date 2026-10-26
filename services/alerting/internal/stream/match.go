// Package stream runs log_match / error_new monitors with condition.stream = true from the ingest topics
// (tt.otlp.logs, tt.otlp.traces) for sub-10-second alerting. It never resolves anything: it only escalates
// (pending / firing) through the engine's state machine; recovery stays with the periodic evaluation.
package stream

import (
	"slices"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// LogRec is the part of a log record a stream rule looks at.
type LogRec struct {
	Service, Env, Severity, Body string
}

// Severity maps an OTLP severity number (or text) to the logs.severity enum (same as ingest-writer).
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

// LogMatch reports whether rec counts for a log_match monitor. exact is false when the monitor filters on a
// Drain patternId, which only ingest-writer computes: such records only trigger a ClickHouse re-evaluation.
func LogMatch(m *model.Monitor, rec LogRec) (match, exact bool) {
	svc, _, _, env := m.Scope()
	if svc != "" && svc != rec.Service {
		return false, true
	}
	if env != "" && env != rec.Env {
		return false, true
	}
	if sev := eval.Severities(m.Query); len(sev) > 0 && !slices.Contains(sev, rec.Severity) {
		return false, true
	}
	if c := m.Query.Contains; c != "" && !strings.Contains(strings.ToLower(rec.Body), strings.ToLower(c)) {
		return false, true
	}
	return true, m.Query.PatternID == 0
}

// SpanErr is an error span seen on the stream.
type SpanErr struct{ Service, Env string }

// IsErrorSpan applies ingest-writer's rule: status ERROR, or HTTP status ≥ 500 on a non-internal span.
func IsErrorSpan(sp *tracepb.Span) bool {
	if sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		return true
	}
	if sp.GetKind() == tracepb.Span_SPAN_KIND_INTERNAL || sp.GetKind() == tracepb.Span_SPAN_KIND_UNSPECIFIED {
		return false
	}
	for _, kv := range sp.GetAttributes() {
		switch kv.GetKey() {
		case "http.response.status_code", "http.status_code":
			v := kv.GetValue()
			code := v.GetIntValue()
			if s := v.GetStringValue(); s != "" {
				for _, c := range s {
					if c < '0' || c > '9' {
						code = 0
						break
					}
					code = code*10 + int64(c-'0')
				}
			}
			if code >= 500 {
				return true
			}
		}
	}
	return false
}

// ErrorNewMatch reports whether an error span may create a new issue for an error_new monitor.
func ErrorNewMatch(m *model.Monitor, e SpanErr) bool {
	svc, _, _, env := m.Scope()
	return (svc == "" || svc == e.Service) && (env == "" || env == e.Env)
}

// Resource returns service.name (default unknown_service) and the deployment environment.
func Resource(kvs []*commonpb.KeyValue) (service, env string) {
	for _, kv := range kvs {
		switch kv.GetKey() {
		case "service.name":
			service = kv.GetValue().GetStringValue()
		case "deployment.environment.name":
			env = kv.GetValue().GetStringValue()
		case "deployment.environment":
			if env == "" {
				env = kv.GetValue().GetStringValue()
			}
		}
	}
	if service == "" {
		service = "unknown_service"
	}
	return service, env
}

// Window counts events in a sliding window with one-second buckets.
type Window struct {
	secs   []int64
	counts []float64
}

// Add records n events at t.
func (w *Window) Add(t time.Time, n float64) {
	s := t.Unix()
	if k := len(w.secs); k > 0 && w.secs[k-1] == s {
		w.counts[k-1] += n
		return
	}
	w.secs = append(w.secs, s)
	w.counts = append(w.counts, n)
}

// Sum returns the events in (now − d, now] and drops older buckets.
func (w *Window) Sum(now time.Time, d time.Duration) float64 {
	cut := now.Add(-d).Unix()
	i := 0
	for i < len(w.secs) && w.secs[i] <= cut {
		i++
	}
	w.secs, w.counts = w.secs[i:], w.counts[i:]
	s := 0.0
	for _, c := range w.counts {
		s += c
	}
	return s
}
