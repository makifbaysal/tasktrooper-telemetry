// Package selfobs is the self-instrumentation of ingest-gw and ingest-writer with the TaskTrooper Go SDK
// (ADR-027, contracts §27).
//
// Everything here is opt-in: without TROOPER_DSN / OTEL_EXPORTER_OTLP_ENDPOINT, Start installs nothing and the services
// keep a nil *Tel, so the hot path does no telemetry work at all.
//
// Feedback-loop guard: ingest is where self telemetry arrives, so work done for the self project
// (TT_SELF_PROJECT_ID) must not create spans or OTel logs — otherwise each export would trigger new telemetry about
// itself. The guard marks such contexts with a fixed, unsampled span context (Suppress): the SDK's parent-based sampler
// drops every child span (pgx, go-redis, producer, ClickHouse), and the slog bridge (LogHandler) keeps those records
// local only. Prometheus metrics are unaffected.
package selfobs

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
)

// Configured reports whether an export destination is set (TROOPER_DSN or OTEL_EXPORTER_OTLP_ENDPOINT) and the SDK is
// not switched off with TROOPER_ENABLED=false. Without it the SDK would still record spans in memory (no exporter),
// so the services skip it entirely.
func Configured() bool {
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("TROOPER_ENABLED"))); err == nil && !v {
		return false
	}
	return strings.TrimSpace(os.Getenv("TROOPER_DSN")) != "" || strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

// Start configures the SDK for service (overridable with OTEL_SERVICE_NAME / TROOPER_SERVICE). TROOPER_ENV defaults to
// TT_ENV, else "development"; TROOPER_VERSION overrides version (the -ldflags build version). It returns enabled=false
// and a no-op shutdown when nothing is configured or the SDK failed to start (the service keeps running unchanged).
func Start(ctx context.Context, service, version string) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	if !Configured() {
		return noop, false, nil
	}
	env := first(os.Getenv("TROOPER_ENV"), os.Getenv("TT_ENV"), "development")
	sd, err := trooper.Start(ctx, trooper.Config{
		Service:     first(os.Getenv("OTEL_SERVICE_NAME"), os.Getenv("TROOPER_SERVICE"), service),
		Environment: env,
		Version:     first(os.Getenv("TROOPER_VERSION"), version),
	})
	if err != nil {
		return noop, false, err
	}
	return sd, true, nil
}

func first(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// SelfProjectFromEnv reads TT_SELF_PROJECT_ID (0 / unset / invalid = guard off).
func SelfProjectFromEnv() uint32 {
	n, err := strconv.ParseUint(strings.TrimSpace(os.Getenv("TT_SELF_PROJECT_ID")), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

// suppressSC is the marker parent of self-project work: valid (so the sampler treats it as a parent) but not sampled
// (so ParentBased drops every child). The ids are fixed; such spans are never exported.
var suppressSC = trace.NewSpanContext(trace.SpanContextConfig{
	TraceID: trace.TraceID{0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f},
	SpanID:  trace.SpanID{0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f, 0x5e, 0x1f},
})

// Suppress returns ctx marked as self-project work: no spans below it are sampled and LogHandler keeps its records
// local.
func Suppress(ctx context.Context) context.Context {
	if Suppressed(ctx) {
		return ctx
	}
	return trace.ContextWithSpanContext(ctx, suppressSC)
}

// Suppressed reports whether ctx was marked by Suppress.
func Suppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	sc := trace.SpanContextFromContext(ctx)
	return sc.TraceID() == suppressSC.TraceID() && sc.SpanID() == suppressSC.SpanID()
}

// ScopeName is the instrumentation scope of the spans created here.
const ScopeName = "github.com/tasktrooper/telemetry/services/ingest"

// Tel creates the ingest spans. A nil *Tel is valid and does nothing (telemetry off).
type Tel struct {
	tracer  trace.Tracer
	self    uint32
	selfHdr []byte // decimal self project id, compared with the tt-project-id record header
}

// New returns a Tel using tp (nil → the global provider). selfProject 0 disables the loop guard.
func New(tp trace.TracerProvider, selfProject uint32) *Tel {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	t := &Tel{tracer: tp.Tracer(ScopeName), self: selfProject}
	if selfProject != 0 {
		t.selfHdr = strconv.AppendUint(nil, uint64(selfProject), 10)
	}
	return t
}

// IsSelf reports whether project is the self project (always false when the guard is off).
func (t *Tel) IsSelf(project uint32) bool { return t != nil && t.self != 0 && project == t.self }

// IsSelfHeader reports whether a tt-project-id header value names the self project.
func (t *Tel) IsSelfHeader(v []byte) bool {
	return t != nil && t.self != 0 && string(v) == string(t.selfHdr)
}

// Request is the server span of one ingest request. The span starts only after authentication (Begin), because the
// project — and with it the loop-guard decision — is unknown before; its start time is the request's arrival.
// Unauthenticated requests get no span: a misconfigured self key would otherwise loop through 401s.
type Request struct {
	tel    *Tel
	start  time.Time
	method string
	route  string
	rpc    bool
	span   trace.Span
	entry  *trooper.Entry
}

type reqKey struct{}

// WithRequest stores a pending request span in ctx. Nil-safe: without telemetry ctx is returned unchanged and the
// Request is nil.
func (t *Tel) WithRequest(ctx context.Context, method, route string, rpc bool, start time.Time) (context.Context, *Request) {
	if t == nil {
		return ctx, nil
	}
	r := &Request{tel: t, start: start, method: method, route: route, rpc: rpc}
	return context.WithValue(ctx, reqKey{}, r), r
}

// Begin is called once the request's tenant is known. For the self project it returns a suppressed context;
// otherwise it starts the server (entry) span of the pending Request in ctx. Without a pending Request ctx is
// returned unchanged.
func Begin(ctx context.Context, org uint32, project uint32) context.Context {
	r, _ := ctx.Value(reqKey{}).(*Request)
	if r == nil || r.span != nil {
		return ctx
	}
	if r.tel.IsSelf(project) {
		return Suppress(ctx)
	}
	attrs := make([]attribute.KeyValue, 0, 6)
	name := r.method + " " + r.route
	if r.rpc {
		name = strings.TrimPrefix(r.route, "/")
		svc, method, _ := strings.Cut(name, "/")
		attrs = append(attrs, attribute.String("rpc.system", "grpc"), attribute.String("rpc.service", svc),
			attribute.String("rpc.method", method))
	} else {
		attrs = append(attrs, attribute.String("http.request.method", r.method))
	}
	attrs = append(attrs, attribute.Int64("tt.org_id", int64(org)), attribute.Int64("tt.project_id", int64(project)))
	// A new root: a customer's traceparent must not pull our span into a trace of another project.
	ctx, span := r.tel.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithNewRoot(),
		trace.WithTimestamp(r.start), trace.WithAttributes(attrs...))
	if !r.rpc {
		trooper.SetServerRoute(span, r.route)
	} else {
		span.SetAttributes(trooper.AttrRoute.String(r.route))
	}
	method := r.method
	if r.rpc {
		method = ""
	}
	ctx, r.entry = trooper.BeginEntry(ctx, method, r.route, nil)
	r.span = span
	return ctx
}

// End finishes the span (if Begin started one) with the HTTP status (gRPC: the mapped code) and error.
func (r *Request) End(status int, grpcCode int, err error) {
	if r == nil || r.span == nil {
		return
	}
	r.entry.Finish(r.route)
	if r.rpc {
		r.span.SetAttributes(attribute.Int("rpc.grpc.status_code", grpcCode))
	} else {
		r.span.SetAttributes(attribute.Int("http.response.status_code", status))
	}
	if status >= 500 {
		msg := strconv.Itoa(status)
		if err != nil {
			msg = err.Error()
		}
		r.span.SetStatus(codes.Error, msg)
		r.span.SetAttributes(attribute.String("error.type", strconv.Itoa(status)))
	}
	r.span.End()
	r.span = nil
}

// ProducerSpan starts a producer span for one record published while handling ctx. It returns ctx unchanged and a
// nil span unless ctx carries a recording span (so self-project and background records get none). Trace context is
// not injected into the record: customer records stay byte-for-byte what the gateway publishes.
func ProducerSpan(ctx context.Context, topic string) (context.Context, trace.Span) {
	if ctx == nil || !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, nil
	}
	return trooper.StartProducer(ctx, nil, trooper.Message{System: "kafka", Destination: topic, Partition: -1})
}

// FlushStats counts the records of one writer flush, in total and per topic.
type FlushStats struct {
	Records int // all records since the last flush
	Self    int // records of the self project
	// Topics are the per-topic counts (nil when nothing was added through Add).
	Topics map[string]TopicStats
}

// TopicStats counts the records of one topic within a flush.
type TopicStats struct {
	Records int
	Self    int
}

// Add counts one record of topic.
func (s *FlushStats) Add(topic string, self bool) {
	s.Records++
	if s.Topics == nil {
		s.Topics = map[string]TopicStats{}
	}
	ts := s.Topics[topic]
	ts.Records++
	if self {
		s.Self++
		ts.Self++
	}
	s.Topics[topic] = ts
}

// Flush holds the consumer spans of one writer flush (one per consumed topic). A nil *Flush is valid.
type Flush struct{ spans []trace.Span }

// End ends every span of the flush with err (nil-safe).
func (f *Flush) End(err error) {
	if f == nil {
		return
	}
	for _, s := range f.spans {
		EndSpan(s, err)
	}
}

// Spans returns the flush's consumer spans (the first one is the parent of the flush's work).
func (f *Flush) Spans() []trace.Span {
	if f == nil {
		return nil
	}
	return f.spans
}

// StartFlush starts the writer's consumer (entry) spans for one flush: one "process flush" span per consumed topic
// (messaging.destination.name = topic, so consumer_1m.topic links the gateway's producer topics to the writer), with
// that topic's record counts. The span of the topic with the most records is the parent of the flush's work (INSERT,
// replay uploads); the others link to it. Topics made only of self-project records get no span; a flush that contains
// only self-project records returns a suppressed context and a nil *Flush. Mixed flushes are traced (they happen anyway
// because of the other projects, so self data riding along does not create extra flushes).
func (t *Tel) StartFlush(ctx context.Context, group string, st FlushStats) (context.Context, *Flush) {
	if t == nil {
		return ctx, nil
	}
	if st.Records > 0 && st.Self >= st.Records {
		return Suppress(ctx), nil
	}
	type topicCount struct {
		topic string
		TopicStats
	}
	var list []topicCount
	for topic, ts := range st.Topics {
		if ts.Records > ts.Self {
			list = append(list, topicCount{topic, ts})
		}
	}
	if len(list) == 0 { // no per-topic counts: one span without a destination
		list = append(list, topicCount{TopicStats: TopicStats{Records: st.Records, Self: st.Self}})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Records != list[j].Records {
			return list[i].Records > list[j].Records
		}
		return list[i].topic < list[j].topic
	})
	f := &Flush{spans: make([]trace.Span, 0, len(list))}
	var parent context.Context
	var link trace.Link
	for i, tc := range list {
		attrs := []attribute.KeyValue{attribute.Int("messaging.batch.message_count", tc.Records), attribute.Int("tt.self_records", tc.Self)}
		sctx, span := trooper.StartConsumer(ctx, nil, trooper.Message{System: "kafka", Destination: tc.topic,
			ConsumerGroup: group, Process: "flush", Partition: -1, Attributes: attrs})
		span.SetName("process flush")
		if i == 0 {
			parent, link = sctx, trace.LinkFromContext(sctx)
		} else {
			span.AddLink(link)
		}
		f.spans = append(f.spans, span)
	}
	return parent, f
}

// InsertSpan starts the ClickHouse client span of one batch INSERT (never per row). Without a recording parent
// (telemetry off, self-only flush) it returns a nil span.
func InsertSpan(ctx context.Context, table string, rows int) (context.Context, trace.Span) {
	parent := trace.SpanFromContext(ctx)
	if !parent.IsRecording() {
		return ctx, nil
	}
	tr := parent.TracerProvider().Tracer(ScopeName)
	return tr.Start(ctx, "INSERT "+table, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("db.system", "clickhouse"),
		attribute.String("db.system.name", "clickhouse"),
		attribute.String("db.operation.name", "INSERT"),
		attribute.String("db.collection.name", table),
		attribute.String("db.query.text", trooper.SanitizeSQL("INSERT INTO "+table)),
		attribute.Int("db.operation.batch.size", rows),
	))
}

// EndSpan ends span (nil-safe) recording err as an exception event with a stack trace.
func EndSpan(span trace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
