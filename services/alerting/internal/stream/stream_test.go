package stream

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

func mon(id int64, kind, target, query, cond string) *model.Monitor {
	m := &model.Monitor{ID: id, ProjectID: 5, Kind: kind}
	_ = json.Unmarshal([]byte(target), &m.Target)
	_ = json.Unmarshal([]byte(query), &m.Query)
	_ = json.Unmarshal([]byte(cond), &m.Condition)
	return m
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func TestLogMatch(t *testing.T) {
	rec := LogRec{Service: "checkout-api", Env: "production", Severity: "error", Body: "Payment DECLINED by provider"}
	tests := []struct {
		name         string
		m            *model.Monitor
		match, exact bool
	}{
		{"contains case-insensitive", mon(1, "log_match", `{"service":"checkout-api"}`, `{"contains":"payment declined"}`, `{}`), true, true},
		{"other service", mon(1, "log_match", `{"service":"pay"}`, `{}`, `{}`), false, true},
		{"min severity", mon(1, "log_match", `{}`, `{"minSeverity":"warn"}`, `{}`), true, true},
		{"severity too low", mon(1, "log_match", `{}`, `{"minSeverity":"fatal"}`, `{}`), false, true},
		{"env mismatch", mon(1, "log_match", `{"env":"staging"}`, `{}`, `{}`), false, true},
		{"pattern id → trigger only", mon(1, "log_match", `{}`, `{"patternId":"777"}`, `{}`), true, false},
		{"text missing", mon(1, "log_match", `{}`, `{"contains":"timeout"}`, `{}`), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if m, e := LogMatch(tc.m, rec); m != tc.match || (m && e != tc.exact) {
				t.Fatalf("got %v %v", m, e)
			}
		})
	}
}

func TestIsErrorSpan(t *testing.T) {
	code := func(k string, v *commonpb.AnyValue) []*commonpb.KeyValue {
		return []*commonpb.KeyValue{{Key: k, Value: v}}
	}
	iv := func(i int64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}
	}
	sv := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	tests := []struct {
		name string
		sp   *tracepb.Span
		want bool
	}{
		{"status error", &tracepb.Span{Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}}, true},
		{"server 503", &tracepb.Span{Kind: tracepb.Span_SPAN_KIND_SERVER, Attributes: code("http.response.status_code", iv(503))}, true},
		{"client 500 as string", &tracepb.Span{Kind: tracepb.Span_SPAN_KIND_CLIENT, Attributes: code("http.status_code", sv("500"))}, true},
		{"server 404", &tracepb.Span{Kind: tracepb.Span_SPAN_KIND_SERVER, Attributes: code("http.response.status_code", iv(404))}, false},
		{"internal 500 ignored", &tracepb.Span{Kind: tracepb.Span_SPAN_KIND_INTERNAL, Attributes: code("http.response.status_code", iv(500))}, false},
		{"ok", &tracepb.Span{Kind: tracepb.Span_SPAN_KIND_SERVER}, false},
	}
	for _, tc := range tests {
		if got := IsErrorSpan(tc.sp); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

func TestWindow(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	var w Window
	w.Add(t0, 1)
	w.Add(t0, 2)
	w.Add(t0.Add(30*time.Second), 4)
	w.Add(t0.Add(70*time.Second), 8)
	tests := []struct {
		at   time.Duration
		d    time.Duration
		want float64
	}{
		{70 * time.Second, 5 * time.Minute, 15},
		{70 * time.Second, time.Minute, 12}, // t0 bucket left the window
		{70 * time.Second, 30 * time.Second, 8},
		{10 * time.Minute, time.Minute, 0},
	}
	for _, tc := range tests {
		if got := w.Sum(t0.Add(tc.at), tc.d); got != tc.want {
			t.Errorf("Sum(+%v, %v) = %v want %v", tc.at, tc.d, got, tc.want)
		}
	}
}

type fakeEngine struct {
	mu      sync.Mutex
	ms      []*model.Monitor
	applied map[int64]eval.Result
}

func (f *fakeEngine) StreamLeader() bool               { return true }
func (f *fakeEngine) StreamMonitors() []*model.Monitor { return f.ms }
func (f *fakeEngine) ApplyStream(_ context.Context, m *model.Monitor, r eval.Result) {
	f.mu.Lock()
	f.applied[m.ID] = r
	f.mu.Unlock()
}
func (f *fakeEngine) EvalNow(context.Context, *model.Monitor) {}

func logsRecord(t *testing.T, project string, svc string, n int, sev logspb.SeverityNumber, body string) *kgo.Record {
	var lrs []*logspb.LogRecord
	for i := 0; i < n; i++ {
		lrs = append(lrs, &logspb.LogRecord{SeverityNumber: sev, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}}})
	}
	b, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", svc), str("deployment.environment.name", "production")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: lrs}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return &kgo.Record{Topic: "tt.otlp.logs", Value: b, Headers: []kgo.RecordHeader{{Key: "project_id", Value: []byte(project)}}}
}

func TestProcessor(t *testing.T) {
	declined := mon(1, "log_match", `{"service":"checkout-api"}`, `{"contains":"declined","minSeverity":"error"}`, `{"threshold":5,"stream":true}`)
	pattern := mon(2, "log_match", `{"service":"checkout-api"}`, `{"patternId":"9"}`, `{"threshold":1,"stream":true}`)
	newErr := mon(3, "error_new", `{"service":"pay-api"}`, `{}`, `{"stream":true}`)
	eng := &fakeEngine{ms: []*model.Monitor{declined, pattern, newErr}, applied: map[int64]eval.Result{}}
	p := NewProcessor(eng, nil, nil)
	t0 := time.Unix(2_000_000, 0)
	p.Now = func() time.Time { return t0 }
	p.Refresh()
	ctx := context.Background()

	// other project / not matching: nothing
	p.Handle(ctx, logsRecord(t, "6", "checkout-api", 10, logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "declined"))
	p.Handle(ctx, logsRecord(t, "5", "checkout-api", 10, logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "declined"))
	if len(eng.applied) != 0 {
		t.Fatalf("unexpected %v", eng.applied)
	}
	// 4 matches: under the threshold → ClickHouse check scheduled (and the pattern monitor is trigger-only)
	p.Handle(ctx, logsRecord(t, "5", "checkout-api", 4, logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "card declined"))
	if len(eng.applied) != 0 || len(p.triggers[1]) != 2 || len(p.triggers[2]) != 2 {
		t.Fatalf("applied %v triggers %v", eng.applied, p.triggers)
	}
	// 2 more → 6 > 5 in the window: applied straight from the stream
	p.Handle(ctx, logsRecord(t, "5", "checkout-api", 2, logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, "Declined!"))
	if r, ok := eng.applied[1]; !ok || r.Value != 6 || !r.Breached {
		t.Fatalf("applied %v", eng.applied)
	}

	// error span of pay-api → error_new re-evaluation at +2s and +6s
	tb, _ := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "pay-api")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}}}}},
	}}})
	p.Handle(ctx, &kgo.Record{Topic: "tt.otlp.traces", Value: tb, Headers: []kgo.RecordHeader{{Key: "project_id", Value: []byte("5")}}})
	if got := p.Due(t0.Add(time.Second)); len(got) != 0 {
		t.Fatalf("early %v", got)
	}
	ids := func(ms []*model.Monitor) map[int64]bool {
		out := map[int64]bool{}
		for _, m := range ms {
			out[m.ID] = true
		}
		return out
	}
	if got := ids(p.Due(t0.Add(2 * time.Second))); !got[1] || !got[2] || !got[3] {
		t.Fatalf("due at +2s %v", got)
	}
	if got := ids(p.Due(t0.Add(6 * time.Second))); len(got) != 3 {
		t.Fatalf("due at +6s %v", got)
	}
	if len(p.triggers) != 0 || len(p.Due(t0.Add(time.Minute))) != 0 {
		t.Fatalf("triggers left %v", p.triggers)
	}
}
