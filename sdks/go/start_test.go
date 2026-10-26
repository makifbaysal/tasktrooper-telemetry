package trooper_test

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	trooper "github.com/tasktrooper/telemetry-go"
)

func attrKey(s string) attribute.Key { return attribute.Key(s) }

type captured struct {
	path, key, encoding, contentType string
	body                             []byte
}

type fakeIngest struct {
	mu       sync.Mutex
	reqs     []captured
	statuses []int // status per request; last one repeats
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Header.Get("Content-Encoding") == "gzip" {
		if zr, err := gzip.NewReader(r.Body); err == nil {
			body, _ = io.ReadAll(zr)
		}
	} else {
		body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, captured{r.URL.Path, r.Header.Get("x-trooper-key"), r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type"), body})
	status := http.StatusOK
	if n := len(f.statuses); n > 0 {
		status = f.statuses[min(len(f.reqs)-1, n-1)]
	}
	f.mu.Unlock()
	w.WriteHeader(status)
}

func (f *fakeIngest) byPath(p string) []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []captured
	for _, r := range f.reqs {
		if r.path == p {
			out = append(out, r)
		}
	}
	return out
}

func startAgainst(t *testing.T, srv *httptest.Server, cfg trooper.Config) trooper.ShutdownFunc {
	t.Helper()
	prevTP, prevMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetMeterProvider(prevMP) })
	cfg.DSN = strings.Replace(srv.URL, "http://", "http://tt_test_key@", 1) + "/1"
	shutdown, err := trooper.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return shutdown
}

func TestStartExportsToIngest(t *testing.T) {
	ing := &fakeIngest{}
	srv := httptest.NewServer(ing)
	defer srv.Close()
	shutdown := startAgainst(t, srv, trooper.Config{Service: "orders-api", Environment: "test", Version: "abc123", DisableRuntimeMetrics: true})

	ctx, s := otel.Tracer("t").Start(context.Background(), "GET /orders/:id", trace.WithSpanKind(trace.SpanKindServer))
	trooper.CaptureError(ctx, io.ErrUnexpectedEOF)
	s.End()
	c, _ := otel.Meter("t").Int64Counter("orders.created")
	c.Add(ctx, 1)
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := shutdown(context.Background()); err != nil { // idempotent
		t.Fatalf("second shutdown: %v", err)
	}

	traces := ing.byPath("/v1/traces")
	if len(traces) == 0 {
		t.Fatal("no request to /v1/traces")
	}
	r := traces[0]
	if r.key != "tt_test_key" || r.encoding != "gzip" || r.contentType != "application/x-protobuf" {
		t.Fatalf("key=%q encoding=%q content-type=%q", r.key, r.encoding, r.contentType)
	}
	var req coltrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(r.body, &req); err != nil {
		t.Fatal(err)
	}
	rs := req.ResourceSpans[0]
	res := map[string]string{}
	for _, kv := range rs.Resource.Attributes {
		res[kv.Key] = kv.Value.GetStringValue()
	}
	for k, v := range map[string]string{"service.name": "orders-api", "service.version": "abc123", "deployment.environment.name": "test", "telemetry.distro.name": "trooper-go"} {
		if res[k] != v {
			t.Errorf("resource %s = %q, want %q", k, res[k], v)
		}
	}
	if _, ok := res["process.command_args"]; ok {
		t.Error("process.command_args must not be collected")
	}
	span := rs.ScopeSpans[0].Spans[0]
	if span.Name != "GET /orders/:id" {
		t.Errorf("span name %q", span.Name)
	}
	var rate float64
	for _, kv := range span.Attributes {
		if kv.Key == "trooper.sample_rate" {
			rate = kv.Value.GetDoubleValue()
		}
	}
	if rate != 1 {
		t.Errorf("trooper.sample_rate = %v", rate)
	}
	if len(ing.byPath("/v1/metrics")) == 0 {
		t.Error("no request to /v1/metrics")
	}
}

func TestExportRetryAndDrop(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		wantReqs int
		dropped  bool
	}{
		{name: "400 dropped without retry", statuses: []int{400}, wantReqs: 1, dropped: true},
		{name: "500 retried then ok", statuses: []int{500, 200}, wantReqs: 2},
		{name: "429 retried then ok", statuses: []int{429, 200}, wantReqs: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ing := &fakeIngest{statuses: tt.statuses}
			srv := httptest.NewServer(ing)
			defer srv.Close()
			before := trooper.DroppedBatches()
			shutdown := startAgainst(t, srv, trooper.Config{Service: "svc", DisableMetrics: true, DisableLogs: true, ExportTimeout: 10 * time.Second})
			_, s := otel.Tracer("t").Start(context.Background(), "op")
			s.End()
			_ = shutdown(context.Background())
			if got := len(ing.byPath("/v1/traces")); got != tt.wantReqs {
				t.Errorf("requests = %d, want %d", got, tt.wantReqs)
			}
			if got := trooper.DroppedBatches() > before; got != tt.dropped {
				t.Errorf("dropped = %v, want %v", got, tt.dropped)
			}
		})
	}
}

func TestStartNeverFailsHard(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("TROOPER_DSN", "")
	tests := []struct {
		name    string
		cfg     trooper.Config
		wantErr bool
	}{
		{name: "no dsn runs without export", cfg: trooper.Config{Service: "svc"}},
		{name: "bad dsn", cfg: trooper.Config{DSN: "not-a-dsn"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shutdown, err := trooper.Start(context.Background(), tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if shutdown == nil {
				t.Fatal("shutdown must never be nil")
			}
			_ = shutdown(context.Background())
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	ing := &fakeIngest{}
	srv := httptest.NewServer(ing)
	defer srv.Close()
	t.Setenv("TROOPER_DSN", strings.Replace(srv.URL, "http://", "http://envkey@", 1)+"/9")
	t.Setenv("OTEL_SERVICE_NAME", "from-otel")
	t.Setenv("TROOPER_SERVICE", "from-trooper") // explicit OTEL_* wins (contract §2)
	t.Setenv("TROOPER_ENV", "staging")
	t.Setenv("TROOPER_SAMPLE_RATE", "0.5")
	prev := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prev)
	shutdown, err := trooper.Start(context.Background(), trooper.Config{DisableMetrics: true, DisableLogs: true})
	if err != nil {
		t.Fatal(err)
	}
	// Force a sampled root by using a trace id below the 0.5 ratio threshold.
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0, 0, 0, 1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
	_, s := otel.Tracer("t").Start(ctx, "op", trace.WithSpanKind(trace.SpanKindServer))
	s.End()
	_ = shutdown(context.Background())
	reqs := ing.byPath("/v1/traces")
	if len(reqs) == 0 || reqs[0].key != "envkey" {
		t.Fatalf("reqs = %v", reqs)
	}
	var req coltrace.ExportTraceServiceRequest
	_ = proto.Unmarshal(reqs[0].body, &req)
	res := map[string]string{}
	for _, kv := range req.ResourceSpans[0].Resource.Attributes {
		res[kv.Key] = kv.Value.GetStringValue()
	}
	if res["service.name"] != "from-otel" || res["deployment.environment.name"] != "staging" {
		t.Errorf("resource = %v", res)
	}
	for _, kv := range req.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
		if kv.Key == "trooper.sample_rate" && kv.Value.GetDoubleValue() != 0.5 {
			t.Errorf("sample_rate = %v", kv.Value.GetDoubleValue())
		}
	}
}
