package trooper

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tasktrooper/telemetry-go/internal/sdkerr"
)

// Internal self-metrics (meter "TaskTrooper.Telemetry", .ai/contracts.md §4): every swallowed error is
// counted in trooper.sdk.errors{component}; telemetry items lost with a failed export batch are counted
// in trooper.sdk.dropped{component}. Components: exporter.traces|metrics|logs|profiles, processor, capture, resolver, profiler,
// profiler.cpu (CPU profiling already active), llm.
type selfMetrics struct {
	failedBatches atomic.Int64
	droppedItems  atomic.Int64
	errors        atomic.Int64

	mu      sync.Mutex
	failed  metric.Int64Counter // trooper.sdk.errors
	dropped metric.Int64Counter
}

var self = &selfMetrics{}

// DroppedBatches returns the number of export batches dropped since process start.
func DroppedBatches() int64 { return self.failedBatches.Load() }

func (s *selfMetrics) register(m metric.Meter) {
	f, err1 := m.Int64Counter("trooper.sdk.errors", metric.WithDescription("Errors swallowed by the trooper SDK"), metric.WithUnit("{error}"))
	d, err2 := m.Int64Counter("trooper.sdk.dropped", metric.WithDescription("Telemetry items dropped by the trooper SDK"), metric.WithUnit("{item}"))
	if err1 != nil || err2 != nil {
		return
	}
	s.mu.Lock()
	s.failed, s.dropped = f, d
	s.mu.Unlock()
}

func (s *selfMetrics) record(signal string, items int) {
	s.failedBatches.Add(1)
	s.droppedItems.Add(int64(items))
	s.errors.Add(1)
	s.mu.Lock()
	f, d := s.failed, s.dropped
	s.mu.Unlock()
	if f == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("component", "exporter."+signal))
	// Use a fresh context: the export context may already be cancelled.
	f.Add(context.Background(), 1, attrs)
	d.Add(context.Background(), int64(items), attrs)
}

type countingSpanExporter struct{ sdktrace.SpanExporter }

func (e countingSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	if err != nil {
		self.record("traces", len(spans))
	}
	return err
}

type countingLogExporter struct{ sdklog.Exporter }

func (e countingLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	err := e.Exporter.Export(ctx, records)
	if err != nil {
		self.record("logs", len(records))
	}
	return err
}

type countingMetricExporter struct{ sdkmetric.Exporter }

func (e countingMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err != nil {
		n := 0
		for _, sm := range rm.ScopeMetrics {
			n += len(sm.Metrics)
		}
		self.record("metrics", n)
	}
	return err
}

// exportTransport makes every 5xx retryable (the OTLP exporters only retry 429/502/503/504);
// contract: retry on 429/5xx, drop on other 4xx.
type exportTransport struct{ base http.RoundTripper }

func (t exportTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented &&
		resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode != http.StatusGatewayTimeout {
		resp.StatusCode = http.StatusServiceUnavailable
		resp.Status = "503 Service Unavailable"
	}
	return resp, err
}

// errorHandler swallows OTel errors; logs every error in debug mode, otherwise at most one per minute.
type errorHandler struct {
	debug bool
	last  atomic.Int64
}

func (h *errorHandler) Handle(err error) {
	if err == nil {
		return
	}
	now := time.Now().UnixNano()
	if !h.debug {
		prev := h.last.Load()
		if now-prev < int64(time.Minute) || !h.last.CompareAndSwap(prev, now) {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "trooper: %v\n", err)
}

// MeterName is the instrumentation scope of the SDK's internal metrics.
const MeterName = "TaskTrooper.Telemetry"

// SDKErrors returns the number of errors swallowed by the SDK since process start.
func SDKErrors() int64 { return self.errors.Load() }

// countDropped records telemetry items dropped by component (e.g. a profile that could not be sent).
func (s *selfMetrics) countDropped(component string, n int) {
	s.droppedItems.Add(int64(n))
	s.mu.Lock()
	d := s.dropped
	s.mu.Unlock()
	if d != nil {
		d.Add(context.Background(), int64(n), metric.WithAttributes(attribute.String("component", component)))
	}
}

// countError records a swallowed internal error (e.g. a recovered panic) for component.
func (s *selfMetrics) countError(component string) {
	s.errors.Add(1)
	s.mu.Lock()
	f := s.failed
	s.mu.Unlock()
	if f != nil {
		f.Add(context.Background(), 1, metric.WithAttributes(attribute.String("component", component)))
	}
}

func init() { sdkerr.SetCounter(self.countError) }
