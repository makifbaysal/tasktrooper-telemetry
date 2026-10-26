package trooper_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

// TestSampleRateTraceState: the root writes tracestate tt=r:<rate>; remote-parented entry spans
// take trooper.sample_rate from it, falling back to their own configured rate.
func TestSampleRateTraceState(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(trooper.NewSampler(1)), // own rate 1; upstream says 0.25
		sdktrace.WithSpanProcessor(trooper.NewSpanProcessor(1)),
		sdktrace.WithSpanProcessor(rec),
	)
	tr := tp.Tracer("t")

	_, root := tr.Start(context.Background(), "root", trace.WithSpanKind(trace.SpanKindServer))
	root.End()
	if got := root.SpanContext().TraceState().Get("tt"); got != "r:1" {
		t.Errorf("root tracestate tt = %q, want r:1", got)
	}

	ts, _ := trace.ParseTraceState("tt=r:0.25,other=x")
	tests := []struct {
		name string
		ts   trace.TraceState
		want float64
	}{
		{"upstream rate", ts, 0.25},
		{"no tracestate", trace.TraceState{}, 1},
	}
	for _, tt := range tests {
		parent := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{9}, SpanID: trace.SpanID{9}, TraceFlags: trace.FlagsSampled, TraceState: tt.ts, Remote: true,
		}))
		_, s := tr.Start(parent, tt.name, trace.WithSpanKind(trace.SpanKindConsumer))
		s.End()
	}
	for _, tt := range tests {
		var got float64
		for _, s := range rec.Ended() {
			if s.Name() == tt.name {
				v, _ := troopertest.Attr(s, trooper.AttrSampleRate)
				got = v.AsFloat64()
			}
		}
		if got != tt.want {
			t.Errorf("%s: sample_rate = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAttemptHeader(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		attempt int
		want    string
	}{
		{"header only", "3", 0, "3"},
		{"no header", "", 0, "1"},
		{"integration value larger", "2", 5, "5"},
		{"garbage header", "x", 0, "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			c := propagation.MapCarrier{}
			if tt.header != "" {
				c[trooper.AttemptHeader] = tt.header
			}
			_, s := trooper.StartConsumer(context.Background(), c, trooper.Message{System: "kafka", Destination: "orders.retry", Partition: -1, Attempt: tt.attempt})
			s.End()
			sp := h.SpanByKind(trace.SpanKindConsumer)
			if got := troopertest.AttrString(sp, trooper.AttrConsumerAttempt); got != tt.want {
				t.Errorf("attempt = %q, want %q", got, tt.want)
			}
			// process falls back to destination when there is no group (contract §4)
			if got := troopertest.AttrString(sp, trooper.AttrProcess); got != "orders.retry" {
				t.Errorf("process = %q", got)
			}
		})
	}
	c := propagation.MapCarrier{}
	trooper.SetAttempt(c, 4)
	if c[trooper.AttemptHeader] != "4" {
		t.Errorf("SetAttempt: %v", c)
	}
}

func TestTrooperEnabledFalse(t *testing.T) {
	t.Setenv("TROOPER_ENABLED", "false")
	prev := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prev)
	shutdown, err := trooper.Start(context.Background(), trooper.Config{DSN: "http://k@127.0.0.1:1/1"})
	if err != nil || shutdown == nil {
		t.Fatalf("err=%v", err)
	}
	if otel.GetTracerProvider() != prev {
		t.Error("disabled SDK must not install a TracerProvider")
	}
}
