// Package selfobstest installs in-memory span and log recorders with the production sampler (parent-based), so tests
// can check the self-telemetry loop guard.
package selfobstest

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
)

// Recorder holds what the global providers recorded.
type Recorder struct {
	Spans *tracetest.SpanRecorder
	Logs  *LogExporter
	TP    *sdktrace.TracerProvider
}

// Install sets global tracer and logger providers (trooper sampler + span processor, rate 1) recording in memory.
// Globals are restored on cleanup.
func Install(tb testing.TB) *Recorder {
	tb.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(trooper.NewSampler(1)),
		sdktrace.WithSpanProcessor(trooper.NewSpanProcessor(1)),
		sdktrace.WithSpanProcessor(spans),
	)
	logs := &LogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
	prevTP, prevLP := otel.GetTracerProvider(), global.GetLoggerProvider()
	otel.SetTracerProvider(tp)
	global.SetLoggerProvider(lp)
	tb.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		global.SetLoggerProvider(prevLP)
	})
	return &Recorder{Spans: spans, Logs: logs, TP: tp}
}

// SpanNames lists the ended spans' names.
func (r *Recorder) SpanNames() []string {
	var out []string
	for _, s := range r.Spans.Ended() {
		out = append(out, s.Name())
	}
	return out
}

// LogExporter keeps exported OTel log records.
type LogExporter struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

// Export implements sdklog.Exporter.
func (e *LogExporter) Export(_ context.Context, rs []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rs {
		e.recs = append(e.recs, r.Clone())
	}
	return nil
}

// Shutdown implements sdklog.Exporter.
func (e *LogExporter) Shutdown(context.Context) error { return nil }

// ForceFlush implements sdklog.Exporter.
func (e *LogExporter) ForceFlush(context.Context) error { return nil }

// Bodies lists the exported log bodies.
func (e *LogExporter) Bodies() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.recs))
	for _, r := range e.recs {
		out = append(out, r.Body().AsString())
	}
	return out
}
