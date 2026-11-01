package gw

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	trooper "github.com/tasktrooper/telemetry-go"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs/selfobstest"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// tracingSink behaves like KafkaSink for telemetry: producer span on Send, ended at the (immediate) ack.
type tracingSink struct{ memSink }

func (s *tracingSink) Send(r *kgo.Record) error {
	traceRecord(r)
	if err := s.memSink.Send(r); err != nil {
		return err
	}
	endRecordSpan(r, nil)
	return nil
}

const selfProject = 2

var selfKeys = auth.Static{
	"good": {ID: 1, Tenant: tenant.Tenant{OrgID: 7, ProjectID: 9}},
	"self": {ID: 3, Tenant: tenant.Tenant{OrgID: 1, ProjectID: selfProject}},
}

func newSelfObsServer(sink Sink, self uint32) *Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{DefaultRPS: 100}, selfKeys, denyLimiter{}, sink, log)
	s.SetTelemetry(selfobs.New(nil, self))
	return s
}

func TestSelfTelemetryHTTP(t *testing.T) {
	pb, _ := proto.Marshal(traceReq(2))
	tests := []struct {
		name       string
		self       uint32
		key        string
		wantStatus int
		wantSpans  []string
		wantRecs   int
	}{
		{name: "customer request traced", self: selfProject, key: "good", wantStatus: 200,
			wantSpans: []string{"send tt.otlp.traces", "POST /v1/traces"}, wantRecs: 1},
		{name: "self project not traced", self: selfProject, key: "self", wantStatus: 200, wantRecs: 1},
		{name: "guard off traces self project", self: 0, key: "self", wantStatus: 200,
			wantSpans: []string{"send tt.otlp.traces", "POST /v1/traces"}, wantRecs: 1},
		{name: "unauthenticated not traced", self: selfProject, key: "nope", wantStatus: 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := selfobstest.Install(t)
			sink := &tracingSink{}
			h := newSelfObsServer(sink, tt.self).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(pb))
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("X-Trooper-Key", tt.key)
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d", rw.Code, tt.wantStatus)
			}
			if got := rec.SpanNames(); !slices.Equal(got, tt.wantSpans) {
				t.Fatalf("spans = %q, want %q", got, tt.wantSpans)
			}
			if len(sink.recs) != tt.wantRecs {
				t.Fatalf("records = %d, want %d", len(sink.recs), tt.wantRecs)
			}
			for _, r := range sink.recs {
				for _, h := range r.Headers {
					if h.Key == "traceparent" || h.Key == "baggage" {
						t.Fatalf("trace context injected into the customer record: %s", h.Key)
					}
				}
				if r.Context != nil && r.Context.Err() != nil {
					t.Fatal("record context must not be cancelled with the request")
				}
			}
			if len(tt.wantSpans) == 0 {
				return
			}
			spans := rec.Spans.Ended()
			server, producer := spans[1], spans[0]
			if producer.Parent().SpanID() != server.SpanContext().SpanID() {
				t.Fatal("producer span is not a child of the request span")
			}
			attrs := map[string]string{}
			for _, kv := range server.Attributes() {
				attrs[string(kv.Key)] = kv.Value.Emit()
			}
			if attrs["http.response.status_code"] != "200" || attrs["trooper.route"] != "/v1/traces" {
				t.Fatalf("server attrs %v", attrs)
			}
		})
	}
}

func TestSelfTelemetryGRPC(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		wantSpans []string
	}{
		{name: "customer", key: "good", wantSpans: []string{"send tt.otlp.traces", "opentelemetry.proto.collector.trace.v1.TraceService/Export"}},
		{name: "self", key: "self"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := selfobstest.Install(t)
			s := newSelfObsServer(&tracingSink{}, selfProject)
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-trooper-key", tt.key))
			ctx = grpc.NewContextWithServerTransportStream(ctx, fakeStream{"/opentelemetry.proto.collector.trace.v1.TraceService/Export"})
			if _, err := (traceSvc{s: s}).Export(ctx, traceReq(1)); err != nil {
				t.Fatal(err)
			}
			if got := rec.SpanNames(); !slices.Equal(got, tt.wantSpans) {
				t.Fatalf("spans = %q, want %q", got, tt.wantSpans)
			}
		})
	}
}

type fakeStream struct{ method string }

func (f fakeStream) Method() string             { return f.method }
func (fakeStream) SetHeader(metadata.MD) error  { return nil }
func (fakeStream) SendHeader(metadata.MD) error { return nil }
func (fakeStream) SetTrailer(metadata.MD) error { return nil }

// BenchmarkHTTPTracesTelemetry is BenchmarkHTTPTraces with self-telemetry on: customer project (spans recorded, also
// with head sampling 0.1) and the self project (suppressed by the loop guard).
func BenchmarkHTTPTracesTelemetry(b *testing.B) {
	for _, bc := range []struct {
		name, key string
		rate      float64
	}{{"customer", "good", 1}, {"customer-sampled-0.1", "good", 0.1}, {"self", "self", 1}} {
		b.Run(bc.name, func(b *testing.B) {
			installBenchProvider(b, bc.rate)
			srv := newSelfObsServer(&benchTracingSink{}, selfProject)
			h := srv.Handler()
			pbb, _ := proto.Marshal(traceReq(100))
			body := gz(pbb)
			b.SetBytes(int64(len(pbb)))
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/x-protobuf")
				req.Header.Set("Content-Encoding", "gzip")
				req.Header.Set("x-trooper-key", bc.key)
				rw := httptest.NewRecorder()
				h.ServeHTTP(rw, req)
				if rw.Code != 200 {
					b.Fatal(rw.Code)
				}
			}
		})
	}
}

type benchTracingSink struct{}

func (benchTracingSink) Send(r *kgo.Record) error {
	traceRecord(r)
	endRecordSpan(r, nil)
	return nil
}

// installBenchProvider mirrors production (trooper sampler + processor, batch processor) with a discarding exporter.
func installBenchProvider(b *testing.B, rate float64) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(trooper.NewSampler(rate)),
		sdktrace.WithSpanProcessor(trooper.NewSpanProcessor(rate)),
		sdktrace.WithBatcher(tracetest.NewNoopExporter()),
	)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	b.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })
}
