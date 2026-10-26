package gw

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

type memSink struct {
	mu   sync.Mutex
	recs []*kgo.Record
	full bool
}

func (m *memSink) Send(r *kgo.Record) error {
	if m.full {
		return ErrBufferFull
	}
	m.mu.Lock()
	m.recs = append(m.recs, r)
	m.mu.Unlock()
	return nil
}

type denyLimiter struct{ deny bool }

func (d denyLimiter) Allow(context.Context, uint32, int) (bool, time.Duration) {
	if d.deny {
		return false, 1500 * time.Millisecond
	}
	return true, 0
}

var testKeys = auth.Static{
	"good":    {ID: 1, Tenant: tenant.Tenant{OrgID: 7, ProjectID: 9}},
	"browser": {ID: 2, Tenant: tenant.Tenant{OrgID: 7, ProjectID: 9}, AllowedOrigins: []string{"https://shop.example.com", "*.example.org"}},
}

func newTestServer(sink Sink, lim RateLimiter, maxBody int64, maxRecord int) *Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Config{MaxBodyBytes: maxBody, MaxRecordBytes: maxRecord, DefaultRPS: 100}, testKeys, lim, sink, log)
}

func traceReq(nSpans int) *coltracepb.ExportTraceServiceRequest {
	var spans []*tracepb.Span
	for i := range nSpans {
		spans = append(spans, &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: []byte{0, 0, 0, 0, 0, 0, byte(i >> 8), byte(i)},
			Name: "GET /orders/:id", Kind: tracepb.Span_SPAN_KIND_SERVER, StartTimeUnixNano: 1, EndTimeUnixNano: 2,
			Attributes: []*commonpb.KeyValue{{Key: "http.route", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("x", 100)}}}}})
	}
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api"}}}}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func TestHTTP(t *testing.T) {
	pb, _ := proto.Marshal(traceReq(3))
	otlpJSON := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},"scopeSpans":[{"spans":[
		{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331","parentSpanId":"00f067aa0ba902b7","name":"x","kind":2,
		 "startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000100000000","unknownField":1}]}]}]}`
	rumJSON := `{"resource":{"app":"shop-web","platform":"PLATFORM_WEB"},"events":[{"type":"VIEW_START","ts_ms":1700000000000,"view":{"loading_time_ms":12}},{"type":"VITAL","ts_ms":1700000000000,"vital":{"name":"LCP","value":1}, "future_field": true}]}`
	bomb := gz(bytes.Repeat([]byte{0}, 2<<20)) // 2 MiB of zeros, tiny compressed

	tests := []struct {
		name       string
		path       string
		ct, enc    string
		hdr        map[string]string
		query      string
		body       []byte
		limiter    RateLimiter
		full       bool
		wantStatus int
		wantTopic  string
		wantRecs   int
	}{
		{name: "proto traces", path: "/v1/traces", ct: "application/x-protobuf", hdr: map[string]string{"x-trooper-key": "good"}, body: pb, wantStatus: 200, wantTopic: topics.Traces, wantRecs: 1},
		{name: "bearer auth", path: "/v1/traces", ct: "application/x-protobuf", hdr: map[string]string{"Authorization": "Bearer good"}, body: pb, wantStatus: 200, wantTopic: topics.Traces, wantRecs: 1},
		{name: "gzip proto", path: "/v1/traces", ct: "application/x-protobuf", enc: "gzip", hdr: map[string]string{"x-trooper-key": "good"}, body: gz(pb), wantStatus: 200, wantTopic: topics.Traces, wantRecs: 1},
		{name: "otlp json", path: "/v1/traces", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(otlpJSON), wantStatus: 200, wantTopic: topics.Traces, wantRecs: 1},
		{name: "rum json query key", path: "/v1/rum", ct: "text/plain;charset=UTF-8", query: "key=good", body: []byte(rumJSON), wantStatus: 200, wantTopic: topics.RUM, wantRecs: 1},
		{name: "query key only for rum", path: "/v1/traces", ct: "application/x-protobuf", query: "key=good", body: pb, wantStatus: 401},
		{name: "missing key", path: "/v1/logs", ct: "application/json", body: []byte(`{}`), wantStatus: 401},
		{name: "bad key", path: "/v1/logs", ct: "application/json", hdr: map[string]string{"x-trooper-key": "nope"}, body: []byte(`{}`), wantStatus: 401},
		{name: "bad proto", path: "/v1/traces", ct: "application/x-protobuf", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte{0xff, 0xff, 0xff}, wantStatus: 400},
		{name: "bad json", path: "/v1/metrics", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"resourceMetrics":`), wantStatus: 400},
		{name: "too large", path: "/v1/logs", ct: "application/x-protobuf", hdr: map[string]string{"x-trooper-key": "good"}, body: make([]byte, 1<<20+1), wantStatus: 413},
		{name: "zip bomb", path: "/v1/logs", ct: "application/x-protobuf", enc: "gzip", hdr: map[string]string{"x-trooper-key": "good"}, body: bomb, wantStatus: 413},
		{name: "bad encoding", path: "/v1/logs", ct: "application/x-protobuf", enc: "br", hdr: map[string]string{"x-trooper-key": "good"}, body: pb, wantStatus: 415},
		{name: "rate limited", path: "/v1/traces", ct: "application/x-protobuf", hdr: map[string]string{"x-trooper-key": "good"}, body: pb, limiter: denyLimiter{true}, wantStatus: 429},
		{name: "buffer full", path: "/v1/traces", ct: "application/x-protobuf", hdr: map[string]string{"x-trooper-key": "good"}, body: pb, full: true, wantStatus: 503},
		{name: "origin denied", path: "/v1/rum", ct: "application/json", query: "key=browser", hdr: map[string]string{"Origin": "https://evil.com"}, body: []byte(rumJSON), wantStatus: 403},
		{name: "origin allowed", path: "/v1/rum", ct: "application/json", query: "key=browser", hdr: map[string]string{"Origin": "https://shop.example.com"}, body: []byte(rumJSON), wantStatus: 200, wantTopic: topics.RUM, wantRecs: 1},
		{name: "origin wildcard", path: "/v1/rum", ct: "application/json", query: "key=browser", hdr: map[string]string{"Origin": "https://a.example.org"}, body: []byte(rumJSON), wantStatus: 200, wantTopic: topics.RUM, wantRecs: 1},
		{name: "empty request", path: "/v1/traces", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{}`), wantStatus: 200, wantRecs: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &memSink{full: tt.full}
			lim := tt.limiter
			if lim == nil {
				lim = denyLimiter{}
			}
			srv := newTestServer(sink, lim, 1<<20, 900<<10)
			url := tt.path
			if tt.query != "" {
				url += "?" + tt.query
			}
			req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.ct)
			if tt.enc != "" {
				req.Header.Set("Content-Encoding", tt.enc)
			}
			req.Header.Set("User-Agent", "test-ua")
			for k, v := range tt.hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			if acao := rec.Header().Get("Access-Control-Allow-Origin"); tt.hdr["Origin"] != "" && (acao != "") != (tt.wantStatus == 200) {
				t.Errorf("Access-Control-Allow-Origin=%q for status %d", acao, rec.Code)
			}
			if (tt.wantStatus == 429 || tt.wantStatus == 503) && rec.Header().Get("Retry-After") == "" {
				t.Error("missing Retry-After")
			}
			if len(sink.recs) != tt.wantRecs {
				t.Fatalf("records=%d want %d", len(sink.recs), tt.wantRecs)
			}
			for _, r := range sink.recs {
				if r.Topic != tt.wantTopic || string(r.Key) != "9" {
					t.Errorf("topic/key = %s/%s", r.Topic, r.Key)
				}
				h := map[string]string{}
				for _, x := range r.Headers {
					h[x.Key] = string(x.Value)
				}
				if h[topics.HeaderOrgID] != "7" || h[topics.HeaderProjectID] != "9" || h[topics.HeaderReceivedAt] == "" || h[topics.HeaderClientIP] == "" {
					t.Errorf("headers: %v", h)
				}
				if (tt.wantTopic == topics.RUM) != (h[topics.HeaderUserAgent] == "test-ua") {
					t.Errorf("user_agent header only for RUM: %v", h)
				}
			}
		})
	}
}

func TestOTLPJSONIDsBecomeBytes(t *testing.T) {
	sink := &memSink{}
	srv := newTestServer(sink, denyLimiter{}, 1<<20, 900<<10)
	body := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331","name":"x"}]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-trooper-key", "good")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "{}" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	var got coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(sink.recs[0].Value, &got); err != nil {
		t.Fatal(err)
	}
	sp := got.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if len(sp.TraceId) != 16 || sp.TraceId[0] != 0x0a || len(sp.SpanId) != 8 || sp.SpanId[0] != 0xb7 {
		t.Fatalf("ids: %x %x", sp.TraceId, sp.SpanId)
	}
}

func TestRUMProtobuf(t *testing.T) {
	sink := &memSink{}
	srv := newTestServer(sink, denyLimiter{}, 1<<20, 900<<10)
	b, _ := proto.Marshal(&rumpb.RumBatch{Resource: &rumpb.Resource{App: "ios"}, Events: []*rumpb.RumEvent{{Type: rumpb.EventType_APP_START}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/rum", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("x-trooper-key", "good")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || len(sink.recs) != 1 || !bytes.Equal(sink.recs[0].Value, b) {
		t.Fatalf("%d recs=%d", rec.Code, len(sink.recs))
	}
}

func TestCORSPreflight(t *testing.T) {
	srv := newTestServer(&memSink{}, denyLimiter{}, 1<<20, 900<<10)
	for _, tt := range []struct {
		path string
		want int
	}{{"/v1/rum", 204}, {"/v1/traces", 204}, {"/v1/metrics", 405}} {
		req := httptest.NewRequest(http.MethodOptions, tt.path, nil)
		req.Header.Set("Origin", "https://shop.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s: %d want %d", tt.path, rec.Code, tt.want)
		}
		if tt.want == 204 && (rec.Header().Get("Access-Control-Allow-Origin") != "https://shop.example.com" ||
			!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "X-Trooper-Key")) {
			t.Errorf("%s: cors headers %v", tt.path, rec.Header())
		}
	}
}

func TestSplitLargeRequest(t *testing.T) {
	sink := &memSink{}
	srv := newTestServer(sink, denyLimiter{}, 5<<20, 64<<10) // 64 KiB records
	req := traceReq(3000)                                    // ~400 KiB
	b, _ := proto.Marshal(req)
	hr := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(b))
	hr.Header.Set("Content-Type", "application/x-protobuf")
	hr.Header.Set("x-trooper-key", "good")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, hr)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if len(sink.recs) < 6 {
		t.Fatalf("expected split into several records, got %d", len(sink.recs))
	}
	total := 0
	for _, r := range sink.recs {
		if len(r.Value) > 64<<10 {
			t.Errorf("record %d bytes > limit", len(r.Value))
		}
		var part coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(r.Value, &part); err != nil {
			t.Fatal(err)
		}
		for _, rs := range part.ResourceSpans {
			if rs.Resource == nil || len(rs.Resource.Attributes) == 0 {
				t.Error("every part must keep the resource")
			}
			for _, ss := range rs.ScopeSpans {
				total += len(ss.Spans)
			}
		}
	}
	if total != 3000 {
		t.Fatalf("spans after split = %d, want 3000", total)
	}
}

// BenchmarkHTTPTraces measures the gateway request path (auth hit, rate limit, gunzip, validate, enqueue)
// for a gzip'd OTLP/HTTP protobuf request with 100 spans.
func BenchmarkHTTPTraces(b *testing.B) {
	srv := newTestServer(discardSink{}, denyLimiter{}, 5<<20, 900<<10)
	h := srv.Handler()
	pb, _ := proto.Marshal(traceReq(100))
	body := gz(pb)
	b.SetBytes(int64(len(pb)))
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("x-trooper-key", "good")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			b.Fatal(rec.Code)
		}
	}
}

// BenchmarkHTTPRumJSON measures the RUM JSON → protobuf path for a 20-event batch.
func BenchmarkHTTPRumJSON(b *testing.B) {
	srv := newTestServer(discardSink{}, denyLimiter{}, 5<<20, 900<<10)
	h := srv.Handler()
	ev := `{"type":"RESOURCE","ts_ms":1700000000000,"session_id":"s1","view_id":"v1","view_name":"/p/:id","resource":{"url":"https://api/x/1","method":"GET","status_code":200,"duration_ms":12}}`
	body := []byte(`{"resource":{"app":"web","platform":"PLATFORM_WEB"},"events":[` + strings.TrimSuffix(strings.Repeat(ev+",", 20), ",") + `]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/rum?key=good", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			b.Fatal(rec.Code)
		}
	}
}

type discardSink struct{}

func (discardSink) Send(*kgo.Record) error { return nil }
