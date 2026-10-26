package flatten

import (
	"math"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

var tn = tenant.Tenant{OrgID: 1, ProjectID: 2}

func kv(k string, v any) *commonpb.KeyValue {
	var av *commonpb.AnyValue
	switch x := v.(type) {
	case string:
		av = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: x}}
	case int:
		av = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(x)}}
	case float64:
		av = &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: x}}
	case bool:
		av = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: x}}
	}
	return &commonpb.KeyValue{Key: k, Value: av}
}

func attrs(pairs ...any) []*commonpb.KeyValue {
	var out []*commonpb.KeyValue
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, kv(pairs[i].(string), pairs[i+1]))
	}
	return out
}

func sid(b byte) []byte { return []byte{0, 0, 0, 0, 0, 0, 0, b} }

var traceID = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

func span(id, parent byte, kind tracepb.Span_SpanKind, a []*commonpb.KeyValue) *tracepb.Span {
	s := &tracepb.Span{TraceId: traceID, SpanId: sid(id), Kind: kind, Name: "op", Attributes: a,
		StartTimeUnixNano: 1_700_000_000_000_000_000, EndTimeUnixNano: 1_700_000_000_250_000_000}
	if parent != 0 {
		s.ParentSpanId = sid(parent)
	}
	return s
}

func traces(service string, spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: attrs("service.name", service, "deployment.environment.name", "prod", "service.version", "1.4.0")},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
}

const (
	server   = tracepb.Span_SPAN_KIND_SERVER
	client   = tracepb.Span_SPAN_KIND_CLIENT
	consumer = tracepb.Span_SPAN_KIND_CONSUMER
	producer = tracepb.Span_SPAN_KIND_PRODUCER
	internal = tracepb.Span_SPAN_KIND_INTERNAL
)

func TestEntryDetection(t *testing.T) {
	remote := uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK | tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK)
	local := uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK)
	withFlags := func(s *tracepb.Span, f uint32) *tracepb.Span { s.Flags = f; return s }
	tests := []struct {
		name  string
		spans []*tracepb.Span
		want  []uint8 // is_entry per span
	}{
		{"root server", []*tracepb.Span{span(1, 0, server, nil)}, []uint8{1}},
		{"server with remote parent (no flags)", []*tracepb.Span{span(1, 9, server, nil)}, []uint8{1}},
		{"server with remote flag", []*tracepb.Span{withFlags(span(1, 9, server, nil), remote)}, []uint8{1}},
		{"server with local flag", []*tracepb.Span{withFlags(span(1, 9, server, nil), local)}, []uint8{0}},
		{"nested server under server in same batch", []*tracepb.Span{span(1, 0, server, nil), span(2, 1, server, nil)}, []uint8{1, 0}},
		{"server under own client (self call)", []*tracepb.Span{span(1, 0, internal, nil), span(2, 1, client, nil), span(3, 2, server, nil)}, []uint8{0, 0, 1}},
		{"consumer root", []*tracepb.Span{span(1, 0, consumer, nil)}, []uint8{1}},
		{"client never entry", []*tracepb.Span{span(1, 0, client, nil)}, []uint8{0}},
	}
	for _, tt := range tests {
		rows := Spans(tn, traces("api", tt.spans...), nil)
		for i, r := range rows {
			if r.IsEntry != tt.want[i] {
				t.Errorf("%s: span %d is_entry=%d want %d", tt.name, i, r.IsEntry, tt.want[i])
			}
		}
	}
}

func TestRouteResolution(t *testing.T) {
	tests := []struct {
		name string
		kind tracepb.Span_SpanKind
		a    []*commonpb.KeyValue
		want string
	}{
		{"trooper.route wins", server, attrs("trooper.route", "/v1/orders/:id", "http.route", "/v1/orders/{id}", "url.path", "/v1/orders/9"), "/v1/orders/:id"},
		{"http.route", server, attrs("http.route", "/v1/orders/{id}", "url.path", "/v1/orders/9"), "/v1/orders/{id}"},
		{"url.path normalized", server, attrs("url.path", "/v1/orders/9/lines/550e8400-e29b-41d4-a716-446655440000"), "/v1/orders/:id/lines/:id"},
		{"old http.target", server, attrs("http.target", "/a/12?x=1"), "/a/:id"},
		{"grpc", server, attrs("rpc.service", "shop.Orders", "rpc.method", "Get"), "/shop.Orders/Get"},
		{"consumer process", consumer, attrs("trooper.process", "send-email", "messaging.consumer.group.name", "g1"), "send-email"},
		{"consumer group fallback", consumer, attrs("messaging.consumer.group.name", "g1"), "g1"},
	}
	for _, tt := range tests {
		r := Spans(tn, traces("api", span(1, 0, tt.kind, tt.a)), nil)[0]
		if r.Route != tt.want {
			t.Errorf("%s: route=%q want %q", tt.name, r.Route, tt.want)
		}
		if r.EntryRoute != tt.want {
			t.Errorf("%s: entry spans must carry their own route as entry_route, got %q", tt.name, r.EntryRoute)
		}
	}
}

func TestEntryRouteInheritance(t *testing.T) {
	rows := Spans(tn, traces("api",
		span(1, 0, server, attrs("http.route", "/checkout", "http.request.method", "post")),
		span(2, 1, internal, nil),
		span(3, 2, client, attrs("db.system", "postgresql", "db.namespace", "orders")),
		span(4, 0, client, attrs("trooper.entry.route", "/sdk-route", "trooper.entry.method", "GET", "http.request.method", "GET", "server.address", "pay")),
	), nil)
	if rows[2].EntryRoute != "/checkout" || rows[2].EntryMethod != "POST" || rows[2].Route != "/checkout" {
		t.Errorf("db span: entry=%q %q route=%q", rows[2].EntryRoute, rows[2].EntryMethod, rows[2].Route)
	}
	if rows[3].EntryRoute != "/sdk-route" || rows[3].EntryMethod != "GET" {
		t.Errorf("sdk tagged span: %q %q", rows[3].EntryRoute, rows[3].EntryMethod)
	}
}

func TestDepNaming(t *testing.T) {
	tests := []struct {
		name                string
		kind                tracepb.Span_SpanKind
		a                   []*commonpb.KeyValue
		dep, depKind, depSv string
	}{
		{"postgres namespace", client, attrs("db.system", "postgresql", "db.namespace", "orders-db", "server.address", "10.0.0.1"), "postgresql · orders-db", "db", ""},
		{"db address fallback", client, attrs("db.system.name", "mysql", "server.address", "mysql-1"), "mysql · mysql-1", "db", ""},
		{"redis is cache", client, attrs("db.system", "redis", "server.address", "cache-1", "db.namespace", "0"), "redis · cache-1", "cache", ""},
		{"memcached is cache", client, attrs("db.system", "memcached", "net.peer.name", "mc"), "memcached · mc", "cache", ""},
		{"http peer.service", client, attrs("http.request.method", "GET", "peer.service", "payments-api", "server.address", "10.1.1.1"), "HTTP · payments-api", "http", "payments-api"},
		{"http server.address", client, attrs("http.request.method", "GET", "server.address", "api.stripe.com"), "HTTP · api.stripe.com", "http", ""},
		{"http url.full", client, attrs("http.method", "GET", "url.full", "https://user@maps.example.com:443/geo?q=1"), "HTTP · maps.example.com", "http", ""},
		{"grpc", client, attrs("rpc.system", "grpc", "rpc.service", "inventory.Stock"), "gRPC · inventory.Stock", "http", ""},
		{"kafka producer", producer, attrs("messaging.system", "kafka", "messaging.destination.name", "orders.created"), "kafka · orders.created", "queue", ""},
		{"plain client", client, attrs("foo", "bar"), "", "", ""},
		{"server has no dep", server, attrs("db.system", "postgresql"), "", "", ""},
	}
	for _, tt := range tests {
		r := Spans(tn, traces("api", span(1, 0, tt.kind, tt.a)), nil)[0]
		if r.Dep != tt.dep || r.DepKind != tt.depKind || r.DepService != tt.depSv {
			t.Errorf("%s: got %q/%q/%q want %q/%q/%q", tt.name, r.Dep, r.DepKind, r.DepService, tt.dep, tt.depKind, tt.depSv)
		}
	}
}

func TestCaller(t *testing.T) {
	tests := []struct {
		name         string
		a            []*commonpb.KeyValue
		caller, kind string
	}{
		{"sdk caller", attrs("trooper.caller", "checkout-svc", "user_agent.original", "okhttp/4"), "checkout-svc", "service"},
		{"browser", attrs("user_agent.original", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/128.0 Safari/537.36"), "web", "web"},
		{"old attr mobile", attrs("http.user_agent", "okhttp/4.9"), "mobile", "mobile"},
		{"probe", attrs("user_agent.original", "kube-probe/1.29"), "probe", "probe"},
		{"webhook", attrs("user_agent.original", "Stripe/1.0 (+https://stripe.com/docs/webhooks)"), "Stripe", "webhook"},
		{"none", nil, "unknown", "unknown"},
	}
	for _, tt := range tests {
		r := Spans(tn, traces("api", span(1, 0, server, tt.a)), nil)[0]
		if r.Caller != tt.caller || r.CallerKind != tt.kind {
			t.Errorf("%s: got %q/%q want %q/%q", tt.name, r.Caller, r.CallerKind, tt.caller, tt.kind)
		}
	}
	// Non-entry spans get no caller.
	if r := Spans(tn, traces("api", span(1, 0, client, attrs("user_agent.original", "okhttp/4"))), nil)[0]; r.Caller != "" {
		t.Errorf("client span caller=%q", r.Caller)
	}
}

func TestSpanFieldsAndErrors(t *testing.T) {
	s := span(1, 0, server, attrs("http.route", "/pay", "http.request.method", "post", "http.response.status_code", 502,
		"trooper.sample_rate", 0.25, "db.statement", "SELECT 1 FROM t WHERE card = '4111'"))
	s.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "upstream failed"}
	s.Events = []*tracepb.Span_Event{{Name: "exception", TimeUnixNano: 1_700_000_000_100_000_000, Attributes: attrs(
		"exception.type", "UpstreamError", "exception.message", "gateway 7 timed out", "exception.stacktrace", "at pay (pay.js:10:2)")}}
	r := Spans(tn, traces("payments", s), nil)[0]
	if r.OrgID != 1 || r.ProjectID != 2 || r.Service != "payments" || r.Env != "prod" || r.Version != "1.4.0" {
		t.Errorf("identity: %+v", r)
	}
	if r.Kind != "server" || r.DurationNs != 250_000_000 || r.HTTPMethod != "POST" || r.StatusCode != 502 {
		t.Errorf("basic fields: kind=%s dur=%d method=%s status=%d", r.Kind, r.DurationNs, r.HTTPMethod, r.StatusCode)
	}
	if r.SampleWeight != 4 {
		t.Errorf("sample_weight=%v want 4", r.SampleWeight)
	}
	if r.IsError != 1 || r.ErrorType != "UpstreamError" || r.ErrorFingerprint == 0 {
		t.Errorf("error: %d %q %d", r.IsError, r.ErrorType, r.ErrorFingerprint)
	}
	if r.Attrs["exception.message"] != "gateway 7 timed out" {
		t.Errorf("exception.message must be copied to attrs for errors_1h, got %q", r.Attrs["exception.message"])
	}
	if r.Attrs["db.query.text"] != "SELECT ? FROM t WHERE card = ?" || r.DBQueryHash == 0 {
		t.Errorf("query not normalized: %q", r.Attrs["db.query.text"])
	}
	if _, ok := r.Attrs["db.statement"]; ok {
		t.Error("raw db.statement must be dropped")
	}
	if len(r.EventTS) != 1 || r.EventName[0] != "exception" || r.EventAttrs[0]["exception.type"] != "UpstreamError" {
		t.Errorf("events: %+v %+v", r.EventName, r.EventAttrs)
	}
	if len(r.TraceID) != 16 || len(r.SpanID) != 8 || len(r.ParentID) != 8 {
		t.Error("ids must be fixed length")
	}

	// Bare 5xx without exception still gets a fingerprint per route.
	a := Spans(tn, traces("api", span(1, 0, server, attrs("http.route", "/a", "http.response.status_code", 500))), nil)[0]
	b := Spans(tn, traces("api", span(1, 0, server, attrs("http.route", "/b", "http.response.status_code", 500))), nil)[0]
	if a.IsError != 1 || a.ErrorType != "HTTP 500" || a.ErrorFingerprint == 0 || a.ErrorFingerprint == b.ErrorFingerprint {
		t.Errorf("bare 5xx: %d %q %d %d", a.IsError, a.ErrorType, a.ErrorFingerprint, b.ErrorFingerprint)
	}
	// Client 4xx is not an error.
	if c := Spans(tn, traces("api", span(1, 0, client, attrs("http.response.status_code", 404))), nil)[0]; c.IsError != 0 {
		t.Error("client 404 must not be an error")
	}
}

func TestMessagingFields(t *testing.T) {
	r := Spans(tn, traces("worker", span(1, 0, consumer, attrs("messaging.system", "kafka", "messaging.destination.name", "orders",
		"messaging.consumer.group.name", "billing", "trooper.consumer.attempt", 3, "trooper.consumer.dlq", true))), nil)[0]
	if r.Topic != "orders" || r.ConsumerGroup != "billing" || r.Attempt != 3 || r.DLQ != 1 || r.Route != "billing" || r.IsEntry != 1 {
		t.Errorf("%+v", r)
	}
}

func TestAttrCaps(t *testing.T) {
	var a []*commonpb.KeyValue
	big := make([]byte, 5000)
	for i := range big {
		big[i] = 'x'
	}
	for i := range 100 {
		a = append(a, kv("k"+string(rune('a'+i%26))+string(rune('a'+i/26)), string(big)))
	}
	r := Spans(tn, traces("api", span(1, 0, internal, a)), nil)[0]
	if len(r.Attrs) > MaxAttrKeys {
		t.Errorf("attrs keys=%d", len(r.Attrs))
	}
	for k, v := range r.Attrs {
		if len(v) > MaxAttrValueLen {
			t.Fatalf("attr %s len=%d", k, len(v))
		}
	}
}

func TestMetricRouting(t *testing.T) {
	gauge := func(name string, v float64, a []*commonpb.KeyValue) *metricspb.Metric {
		return &metricspb.Metric{Name: name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			TimeUnixNano: 1_700_000_030_000_000_000, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}, Attributes: a}}}}}
	}
	dbA := attrs("trooper.db.id", "db-1", "trooper.db.node", "primary", "db.system", "postgresql", "trooper.db.role", "primary")
	lagA := attrs("trooper.broker.cluster", "k1", "messaging.system", "kafka", "messaging.destination.name", "orders", "messaging.consumer.group.name", "billing", "messaging.destination.partition.id", "2")
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: attrs("service.name", "trooper-agent")},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			gauge("trooper.db.cpu", 0.5, dbA),
			gauge("trooper.db.hit_ratio", 0.97, dbA),
			gauge("trooper.db.connections", 40, dbA),
			gauge("trooper.db.query.calls", 10, attrs("trooper.db.id", "db-1", "db.query.text", "SELECT * FROM t WHERE id = 5")),
			gauge("trooper.db.query.total_ms", 55, attrs("trooper.db.id", "db-1", "db.query.text", "SELECT * FROM t WHERE id = 9")),
			gauge("trooper.broker.lag", 1200, lagA),
			gauge("trooper.broker.end_offset", 99999, lagA),
			gauge("trooper.broker.group.members", 3, attrs("trooper.broker.cluster", "k1", "messaging.consumer.group.name", "billing", "trooper.broker.group.state", "Stable")),
			gauge("trooper.broker.node.cpu", 0.3, attrs("trooper.broker.cluster", "k1", "trooper.broker.node", "b1")),
			gauge("http.server.active_requests", 7, attrs("route", "/a")),
			gauge("trooper.db.unknown_thing", 1, dbA), // unknown collector name → custom metrics
			{Name: "latency", Unit: "ms", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{
				TimeUnixNano: 1_700_000_030_000_000_000, Count: 3, Sum: ptr(30.0), ExplicitBounds: []float64{10, 100}, BucketCounts: []uint64{1, 2, 0}}}}}},
		}}},
	}}}
	b := NewMetricsBatch()
	b.Add(tn, req)

	if len(b.DB) != 1 {
		t.Fatalf("db rows=%d want 1 (merged per entity-minute)", len(b.DB))
	}
	for k, r := range b.DB {
		if k.DatabaseID != "db-1" || k.Node != "primary" || r.Engine != "postgresql" || r.Role != "primary" || k.Minute != 1_700_000_000/60*60 {
			t.Errorf("db key: %+v %+v", k, r)
		}
		if r.V[0] != 0.5 || r.V[3] != 40 || r.V[dbHitRatio] != 0.97 {
			t.Errorf("db values: %v", r.V)
		}
		if !math.IsInf(r.V[1], -1) {
			t.Errorf("unreported max column must be -Inf, got %v", r.V[1])
		}
	}
	if len(b.DBQueries) != 1 {
		t.Fatalf("db query rows=%d want 1 (same normalized query)", len(b.DBQueries))
	}
	for _, r := range b.DBQueries {
		if r.Calls != 10 || r.TotalMs != 55 || r.Query != "SELECT * FROM t WHERE id = ?" || r.Key.QueryHash == 0 {
			t.Errorf("db query: %+v", r)
		}
	}
	if len(b.BrokerLag) != 1 {
		t.Fatalf("lag rows=%d", len(b.BrokerLag))
	}
	for k, r := range b.BrokerLag {
		if k.Partition != 2 || k.Topic != "orders" || k.Group != "billing" || r.V[0] != 1200 || r.V[1] != 99999 || !math.IsInf(r.V[5], -1) {
			t.Errorf("lag: %+v %v", k, r.V)
		}
	}
	for _, r := range b.BrokerGroups {
		if r.State != "Stable" || r.Members != 3 {
			t.Errorf("group: %+v", r)
		}
	}
	if len(b.BrokerNodes) != 1 {
		t.Errorf("nodes=%d", len(b.BrokerNodes))
	}
	if len(b.Metrics) != 3 {
		t.Fatalf("custom metrics=%d want 3", len(b.Metrics))
	}
	h := b.Metrics[2]
	if h.Type != "histogram" || h.Value != 30 || h.Count != 3 || len(h.BucketBounds) != 2 || len(h.BucketCounts) != 3 || h.Service != "trooper-agent" {
		t.Errorf("histogram: %+v", h)
	}
	if b.Metrics[0].Type != "gauge" || b.Metrics[0].AttrsHash == 0 {
		t.Errorf("gauge: %+v", b.Metrics[0])
	}
	if b.Len() != 1+1+1+1+1+3 {
		t.Errorf("Len=%d", b.Len())
	}
}

func ptr[T any](v T) *T { return &v }

func TestAttrsHashStable(t *testing.T) {
	a := AttrsHash(map[string]string{"a": "1", "b": "2"})
	b := AttrsHash(map[string]string{"b": "2", "a": "1"})
	c := AttrsHash(map[string]string{"a": "1", "b": "3"})
	if a != b || a == c {
		t.Errorf("a=%d b=%d c=%d", a, b, c)
	}
}

func TestLogs(t *testing.T) {
	recv := time.Unix(1_700_000_100, 0).UTC()
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: attrs("service.name", "api", "deployment.environment", "staging")},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{TimeUnixNano: 1_700_000_000_000_000_000, SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "order 42 failed"}}, TraceId: traceID, SpanId: sid(3),
				Attributes: attrs("trooper.entry.route", "/checkout")},
			{SeverityText: "WARNING", Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "order 77 failed"}}},
		}}},
	}}}
	rows := Logs(tn, req, recv, nil)
	if len(rows) != 2 {
		t.Fatal(len(rows))
	}
	if rows[0].Severity != "error" || rows[0].Route != "/checkout" || rows[0].Env != "staging" || rows[0].SpanID[7] != 3 {
		t.Errorf("%+v", rows[0])
	}
	if rows[1].Severity != "warn" || !rows[1].TS.Equal(recv) || len(rows[1].TraceID) != 16 {
		t.Errorf("%+v", rows[1])
	}
	if rows[0].PatternID != rows[1].PatternID || rows[0].PatternID == 0 {
		t.Error("same template must share pattern_id")
	}
}

func TestSeverity(t *testing.T) {
	tests := []struct {
		n    logspb.SeverityNumber
		text string
		want string
	}{
		{1, "", "trace"}, {5, "", "debug"}, {9, "", "info"}, {12, "", "info"}, {13, "", "warn"}, {17, "", "error"}, {21, "", "fatal"},
		{0, "warning", "warn"}, {0, "CRITICAL", "fatal"}, {0, "err", "error"}, {0, "", "info"}, {0, "verbose", "debug"},
	}
	for _, tt := range tests {
		if got := Severity(tt.n, tt.text); got != tt.want {
			t.Errorf("Severity(%d,%q)=%q want %q", tt.n, tt.text, got, tt.want)
		}
	}
}

func TestRumMapping(t *testing.T) {
	recv := time.UnixMilli(1_700_000_000_000).UTC()
	ts := recv.Add(-2 * time.Second).UnixMilli()
	batch := &rumpb.RumBatch{
		Resource: &rumpb.Resource{App: "shop-web", Version: "3.1.0", Environment: "prod", Platform: rumpb.Platform_PLATFORM_WEB,
			Sdk: &rumpb.Sdk{Name: "@trooper/browser", Version: "0.1.0"}},
		Events: []*rumpb.RumEvent{
			{Type: rumpb.EventType_VIEW_START, TsMs: ts, SessionId: "s", ViewId: "v", AnonymousId: "a", User: &rumpb.User{Id: "u1", Traits: map[string]string{"plan": "pro"}},
				Payload: &rumpb.RumEvent_View{View: &rumpb.ViewPayload{Url: "https://shop.test/products/123?utm=x", LoadingTimeMs: 900, IsInitial: true}}},
			{Type: rumpb.EventType_VIEW_END, TsMs: ts, ViewName: "/products/:id", Payload: &rumpb.RumEvent_View{View: &rumpb.ViewPayload{TimeSpentMs: 15000, LoadingTimeMs: 900}}},
			{Type: rumpb.EventType_VITAL, TsMs: ts, Payload: &rumpb.RumEvent_Vital{Vital: &rumpb.VitalPayload{Name: "lcp", Value: 1800, Rating: "needs-improvement"}}},
			{Type: rumpb.EventType_VITAL, TsMs: ts, Attributes: map[string]string{"total_frames": "600"}, Payload: &rumpb.RumEvent_Vital{Vital: &rumpb.VitalPayload{Name: "SLOW_FRAMES", Value: 12}}},
			{Type: rumpb.EventType_ERROR, TsMs: ts, Payload: &rumpb.RumEvent_Error{Error: &rumpb.ErrorPayload{Type: "TypeError", Message: "x is undefined", Stack: "at f (a.js:1:2)", IsCrash: true}}},
			{Type: rumpb.EventType_RESOURCE, TsMs: ts, Payload: &rumpb.RumEvent_Resource{Resource: &rumpb.ResourcePayload{Url: "https://api.shop.test/v1/carts/991?x=1", Method: "post",
				StatusCode: 503, DurationMs: 120, TraceId: "0af7651916cd43dd8448eb211c80319c", SpanId: "b7ad6b7169203331"}}},
			{Type: rumpb.EventType_ACTION, TsMs: ts, Payload: &rumpb.RumEvent_Action{Action: &rumpb.ActionPayload{Name: "Buy", Target: "#buy", Rage: true}}},
			{Type: rumpb.EventType_APP_START, TsMs: ts, Attributes: map[string]string{"start_reference": "process_start"}, Payload: &rumpb.RumEvent_AppStart{AppStart: &rumpb.AppStartPayload{Kind: "cold", DurationMs: 1500}}},
			{Type: rumpb.EventType_CUSTOM, TsMs: ts, Payload: &rumpb.RumEvent_Custom{Custom: &rumpb.CustomPayload{Name: "checkout", Value: 99.5, Props: map[string]string{"step": "2"}}}},
		},
	}
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	rows := Rum(tn, batch, RumMeta{ReceivedAt: recv, UserAgent: ua}, nil)
	if len(rows) != 9 {
		t.Fatal(len(rows))
	}
	v := rows[0]
	if v.Type != "view_start" || v.DurationMs != 900 || v.ViewName != "/products/:id" || v.App != "shop-web" || v.Platform != "web" ||
		v.UserID != "u1" || v.Attrs["user.plan"] != "pro" || v.Attrs["view.url"] != "https://shop.test/products/123?utm" || v.Attrs["sdk.name"] != "@trooper/browser" {
		t.Errorf("view_start: %+v", v)
	}
	if v.Browser != "Chrome 128.0" || v.OS != "macOS 10.15" || v.DeviceType != "desktop" {
		t.Errorf("ua fallback: %q %q %q", v.Browser, v.OS, v.DeviceType)
	}
	if !v.TS.Equal(time.UnixMilli(ts)) || !v.ReceivedAt.Equal(recv) {
		t.Errorf("ts within skew must not shift: %v", v.TS)
	}
	if rows[1].Type != "view_end" || rows[1].DurationMs != 15000 {
		t.Errorf("view_end duration=%v", rows[1].DurationMs)
	}
	if rows[2].VitalName != "LCP" || rows[2].VitalValue != 1800 || rows[2].Attrs["vital.rating"] != "needs-improvement" {
		t.Errorf("vital: %+v", rows[2])
	}
	if rows[3].VitalName != "SLOW_FRAMES" || rows[3].VitalValue != 12 || rows[3].Attrs["total_frames"] != "600" {
		t.Errorf("slow frames: %+v", rows[3])
	}
	if e := rows[4]; e.ErrorType != "TypeError" || e.IsCrash != 1 || e.ErrorFingerprint == 0 || e.ErrorStack == "" {
		t.Errorf("error: %+v", e)
	}
	if r := rows[5]; r.ResourceURL != "https://api.shop.test/v1/carts/:id" || r.ResourceMethod != "POST" || r.StatusCode != 503 || r.DurationMs != 120 ||
		r.TraceID[0] != 0x0a || r.Attrs["resource.span_id"] != "b7ad6b7169203331" {
		t.Errorf("resource: %+v", r)
	}
	if a := rows[6]; a.ActionName != "Buy" || a.Rage != 1 || a.Dead != 0 {
		t.Errorf("action: %+v", a)
	}
	if s := rows[7]; s.Type != "app_start" || s.DurationMs != 1500 || s.Attrs["app_start.kind"] != "cold" || s.Attrs["start_reference"] != "process_start" {
		t.Errorf("app_start: %+v", s)
	}
	if c := rows[8]; c.Name != "checkout" || c.DurationMs != 99.5 || c.Attrs["step"] != "2" {
		t.Errorf("custom: %+v", c)
	}
}

func TestRumClockSkew(t *testing.T) {
	recv := time.UnixMilli(1_700_000_000_000).UTC()
	tests := []struct {
		name   string
		offset time.Duration // client clock - server clock
		shift  bool
	}{
		{"in sync", -3 * time.Second, false},
		{"4 min ahead", 4 * time.Minute, false},
		{"1 hour ahead", time.Hour, true},
		{"10 min behind (offline queue)", -10 * time.Minute, false},
		{"6 h behind (backfill)", -6 * time.Hour, false},
		{"23 h behind", -23 * time.Hour, false},
		{"2 days behind", -48 * time.Hour, true},
	}
	for _, tt := range tests {
		newest := recv.Add(tt.offset)
		b := &rumpb.RumBatch{Resource: &rumpb.Resource{App: "a"}, Events: []*rumpb.RumEvent{
			{Type: rumpb.EventType_ACTION, TsMs: newest.Add(-10 * time.Second).UnixMilli()},
			{Type: rumpb.EventType_ACTION, TsMs: newest.UnixMilli()},
		}}
		rows := Rum(tn, b, RumMeta{ReceivedAt: recv}, nil)
		gotShift := !rows[1].TS.Equal(newest)
		if gotShift != tt.shift {
			t.Errorf("%s: shifted=%v want %v", tt.name, gotShift, tt.shift)
		}
		if tt.shift && (!rows[1].TS.Equal(recv) || rows[1].TS.Sub(rows[0].TS) != 10*time.Second) {
			t.Errorf("%s: shift must preserve spacing and land newest on received_at: %v %v", tt.name, rows[0].TS, rows[1].TS)
		}
	}
}

// BenchmarkFlattenSpans measures the hot writer path: OTLP request of 100 spans → rows.
func BenchmarkFlattenSpans(b *testing.B) {
	var spans []*tracepb.Span
	for i := range 20 {
		base := byte(i*5 + 1)
		spans = append(spans,
			span(base, 0, server, attrs("http.route", "/v1/orders/:id", "http.request.method", "GET", "http.response.status_code", 200,
				"url.path", "/v1/orders/123", "user_agent.original", "Mozilla/5.0 (Macintosh) Chrome/128.0 Safari/537.36", "trooper.sample_rate", 1.0)),
			span(base+1, base, internal, attrs("code.function", "handler")),
			span(base+2, base+1, client, attrs("db.system", "postgresql", "db.namespace", "orders", "db.query.text", "SELECT * FROM orders WHERE id = 123 AND status IN (1,2,3)")),
			span(base+3, base+1, client, attrs("db.system", "redis", "server.address", "cache-1", "db.query.text", "GET order:123")),
			span(base+4, base, client, attrs("http.request.method", "POST", "peer.service", "payments-api", "url.full", "https://payments/charge", "http.response.status_code", 200)),
		)
	}
	req := traces("orders-api", spans...)
	rows := make([]SpanRow, 0, 100)
	b.ReportAllocs()
	for b.Loop() {
		rows = Spans(tn, req, rows[:0])
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(spans)), "ns/span")
}

// Backfilled / offline-queued events spread over hours must keep their own minutes (not collapse onto received_at).
func TestRumBackfillKeepsTimestamps(t *testing.T) {
	recv := time.UnixMilli(1_700_000_000_000).UTC()
	var evs []*rumpb.RumEvent
	for i := range 6 {
		evs = append(evs, &rumpb.RumEvent{Type: rumpb.EventType_VIEW_START, TsMs: recv.Add(-time.Duration(i+1) * 30 * time.Minute).UnixMilli()})
	}
	rows := Rum(tn, &rumpb.RumBatch{Resource: &rumpb.Resource{App: "a"}, Events: evs}, RumMeta{ReceivedAt: recv}, nil)
	minutes := map[int64]bool{}
	for i, r := range rows {
		if r.TS.UnixMilli() != evs[i].TsMs {
			t.Errorf("event %d moved: %v → %v", i, time.UnixMilli(evs[i].TsMs).UTC(), r.TS)
		}
		minutes[r.TS.Unix()/60] = true
	}
	if len(minutes) != len(rows) {
		t.Errorf("events collapsed into %d minutes", len(minutes))
	}
}
