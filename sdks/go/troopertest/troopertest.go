// Package troopertest provides an in-memory OpenTelemetry setup with the trooper span processor
// and propagator for testing instrumented code and integrations.
package troopertest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

// Harness records spans in memory.
type Harness struct {
	Recorder *tracetest.SpanRecorder
	Provider *sdktrace.TracerProvider
}

// New installs a global TracerProvider (always-sample, trooper span processor with sample rate 1,
// in-memory recorder) and the trooper propagator for service. Globals are restored on cleanup.
func New(tb testing.TB, service string) *Harness {
	tb.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(trooper.NewSpanProcessor(1)),
		sdktrace.WithSpanProcessor(rec),
	)
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(trooper.NewPropagator(service))
	tb.Cleanup(func() {
		_ = tp.Shutdown(tb.Context())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return &Harness{Recorder: rec, Provider: tp}
}

// Spans returns the ended spans.
func (h *Harness) Spans() []sdktrace.ReadOnlySpan { return h.Recorder.Ended() }

// SpanByKind returns the first ended span of the given kind, or nil.
func (h *Harness) SpanByKind(kind trace.SpanKind) sdktrace.ReadOnlySpan {
	for _, s := range h.Spans() {
		if s.SpanKind() == kind {
			return s
		}
	}
	return nil
}

// SpanByName returns the first ended span with the given name, or nil.
func (h *Harness) SpanByName(name string) sdktrace.ReadOnlySpan {
	for _, s := range h.Spans() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// Attr returns the value of attribute key on span s.
func Attr(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	if s == nil {
		return attribute.Value{}, false
	}
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// AttrString returns attribute key as a string ("" if absent).
func AttrString(s sdktrace.ReadOnlySpan, key attribute.Key) string {
	v, _ := Attr(s, key)
	return v.Emit()
}

// HasException reports whether span s has an "exception" event.
func HasException(s sdktrace.ReadOnlySpan) bool {
	if s == nil {
		return false
	}
	for _, e := range s.Events() {
		if e.Name == "exception" {
			return true
		}
	}
	return false
}

// CallerHeaders returns HTTP-style headers carrying baggage tt.svc=<caller> as an upstream
// trooper service would send them, plus a sampled traceparent.
func CallerHeaders(caller string) propagation.MapCarrier {
	c := propagation.MapCarrier{}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)
	trooper.NewPropagator(caller).Inject(ctx, c)
	return c
}
