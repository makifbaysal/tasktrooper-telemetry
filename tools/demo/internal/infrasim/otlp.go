package infrasim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// KV is one attribute (string or int64 value).
type KV struct {
	K string
	V any
}

func kvs(attrs []KV) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		var v *commonpb.AnyValue
		switch x := a.V.(type) {
		case int:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(x)}}
		case int64:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: x}}
		case float64:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: x}}
		default:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprint(x)}}
		}
		out = append(out, &commonpb.KeyValue{Key: a.K, Value: v})
	}
	return out
}

// Batch collects gauges for one resource (what one trooper-agent / trooper-cloud export carries).
type Batch struct {
	Resource []KV
	ts       uint64
	metrics  map[string]*metricspb.Metric
}

// NewBatch starts a batch at ts.
func NewBatch(resource []KV, ts time.Time) *Batch {
	return &Batch{Resource: resource, ts: uint64(ts.UnixNano()), metrics: map[string]*metricspb.Metric{}}
}

// Gauge adds a gauge data point.
func (b *Batch) Gauge(name string, v float64, attrs ...KV) {
	m, ok := b.metrics[name]
	if !ok {
		m = &metricspb.Metric{Name: name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{}}}
		b.metrics[name] = m
	}
	g := m.Data.(*metricspb.Metric_Gauge).Gauge
	g.DataPoints = append(g.DataPoints, &metricspb.NumberDataPoint{
		TimeUnixNano: b.ts, Attributes: kvs(attrs), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
	})
}

func (b *Batch) proto() *metricspb.ResourceMetrics {
	names := make([]string, 0, len(b.metrics))
	for n := range b.metrics {
		names = append(names, n)
	}
	sort.Strings(names)
	ms := make([]*metricspb.Metric, 0, len(names))
	for _, n := range names {
		ms = append(ms, b.metrics[n])
	}
	return &metricspb.ResourceMetrics{
		Resource:     &resourcepb.Resource{Attributes: kvs(b.Resource)},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "tasktrooper-demo/infrasim"}, Metrics: ms}},
	}
}

// Log is one log record.
type Log struct {
	Time  time.Time
	Body  string
	Attrs []KV
}

// Sender posts OTLP/HTTP protobuf to ingest-gw with the ingest key.
type Sender struct {
	Base, Key string
	HC        *http.Client
}

// Metrics sends batches (one ResourceMetrics each).
func (s *Sender) Metrics(ctx context.Context, batches ...*Batch) error {
	req := &colmetricspb.ExportMetricsServiceRequest{}
	for _, b := range batches {
		if len(b.metrics) > 0 {
			req.ResourceMetrics = append(req.ResourceMetrics, b.proto())
		}
	}
	if len(req.ResourceMetrics) == 0 {
		return nil
	}
	return s.post(ctx, "/v1/metrics", req)
}

// Logs sends warn-level logs for one resource.
func (s *Sender) Logs(ctx context.Context, resource []KV, logs []Log) error {
	if len(logs) == 0 {
		return nil
	}
	recs := make([]*logspb.LogRecord, 0, len(logs))
	for _, l := range logs {
		recs = append(recs, &logspb.LogRecord{
			TimeUnixNano: uint64(l.Time.UnixNano()), ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
			SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_WARN, SeverityText: "warn",
			Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: l.Body}},
			Attributes: kvs(l.Attrs),
		})
	}
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: kvs(resource)},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "tasktrooper-demo/infrasim"}, LogRecords: recs}},
	}}}
	return s.post(ctx, "/v1/logs", req)
}

func (s *Sender) post(ctx context.Context, path string, msg proto.Message) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("x-trooper-key", s.Key)
	hc := s.HC
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("POST %s: %d %s", path, resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}
