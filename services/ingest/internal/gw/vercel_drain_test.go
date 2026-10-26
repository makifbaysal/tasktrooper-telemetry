package gw

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/vercel"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

type staticIntegrations map[int64]*vercel.Integration

func (s staticIntegrations) Vercel(_ context.Context, id int64) (*vercel.Integration, error) {
	if in, ok := s[id]; ok {
		return in, nil
	}
	return nil, ErrNoIntegration
}

var drainIntegrations = staticIntegrations{
	4: {ID: 4, ProjectID: 9, Secret: []byte("drain-secret"), Verify: "verify-me", Projects: map[string]string{"my-app": "shop-web"}},
	5: {ID: 5, ProjectID: 9}, // no secret configured
	6: {ID: 6, ProjectID: 1, Secret: []byte("x")},
}

func drainServer(sink Sink) http.Handler {
	s := New(Config{MaxBodyBytes: 1 << 20, MaxRecordBytes: 900 << 10, ServeMetrics: true}, testKeys, denyLimiter{}, sink,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetIntegrations(drainIntegrations)
	return s.Handler()
}

func drainReq(method, query string, body []byte, hdr ...string) *http.Request {
	r := httptest.NewRequest(method, "/v1/drains/vercel?"+query, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return r
}

func TestVercelDrain(t *testing.T) {
	logs := []byte(`[{"id":"1","deploymentId":"dpl_1","source":"lambda","host":"h","timestamp":1700000000000,"projectId":"p","projectName":"my-app","level":"error","message":"boom","path":"/api/[id]","statusCode":500}]`)
	speed := []byte(`{"schema":"vercel.speed_insights.v1","timestamp":"2023-09-14T15:30:00.000Z","projectId":"p","deviceId":1,"metricType":"LCP","value":1200,"path":"/a","country":"TR"}` + "\n" +
		`{"schema":"vercel.analytics.v2","eventType":"pageview","timestamp":1694723400000,"projectId":"p","deviceId":1,"path":"/a"}` + "\n")
	tr, _ := proto.Marshal(traceReq(2))
	sign := func(b []byte) string { return vercel.Sign([]byte("drain-secret"), b) }

	cases := []struct {
		name      string
		req       *http.Request
		code      int
		topic     string // expected topic of the first record ("" = none)
		verifyHdr bool
	}{
		{"verification GET", drainReq("GET", "key=good&integration=4", nil), 200, "", true},
		{"verification POST empty body", drainReq("POST", "key=good&integration=4", nil, "x-vercel-verify", "1"), 200, "", true},
		{"bad key", drainReq("POST", "key=nope&integration=4", logs, "x-vercel-signature", sign(logs)), 401, "", false},
		{"missing integration", drainReq("POST", "key=good", logs, "x-vercel-signature", sign(logs)), 400, "", false},
		{"unknown integration", drainReq("POST", "key=good&integration=77", logs, "x-vercel-signature", sign(logs)), 404, "", false},
		{"other project's integration", drainReq("POST", "key=good&integration=6", logs, "x-vercel-signature", sign(logs)), 403, "", false},
		{"no secret configured", drainReq("POST", "key=good&integration=5", logs, "x-vercel-signature", sign(logs)), 401, "", false},
		{"bad signature", drainReq("POST", "key=good&integration=4", logs, "x-vercel-signature", sign([]byte("x"))), 403, "", true},
		{"unsigned data", drainReq("POST", "key=good&integration=4", logs), 403, "", true},
		{"log drain (JSON array)", drainReq("POST", "key=good&integration=4", logs, "x-vercel-signature", sign(logs), "Content-Type", "application/json"), 200, topics.Logs, true},
		{"gzip, signature over decompressed body", drainReq("POST", "key=good&integration=4", gz(logs), "x-vercel-signature", sign(logs), "Content-Encoding", "gzip"), 200, topics.Logs, true},
		{"gzip, signature over wire bytes", func() *http.Request {
			z := gz(logs)
			return drainReq("POST", "key=good&integration=4", z, "x-vercel-signature", sign(z), "Content-Encoding", "gzip")
		}(), 200, topics.Logs, true},
		{"speed insights + analytics (NDJSON)", drainReq("POST", "key=good&integration=4", speed, "x-vercel-signature", sign(speed), "Content-Type", "application/x-ndjson"), 200, topics.RUM, true},
		{"trace drain protobuf", drainReq("POST", "key=good&integration=4", tr, "x-vercel-signature", sign(tr), "Content-Type", "application/x-protobuf"), 200, topics.Traces, true},
		{"broken JSON", drainReq("POST", "key=good&integration=4", []byte("[{"), "x-vercel-signature", sign([]byte("[{"))), 400, "", true},
		{"wrong method", drainReq("PUT", "key=good&integration=4", nil), 405, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &memSink{}
			rr := httptest.NewRecorder()
			drainServer(sink).ServeHTTP(rr, c.req)
			if rr.Code != c.code {
				t.Fatalf("code=%d want %d (%s)", rr.Code, c.code, rr.Body.String())
			}
			if got := rr.Header().Get("x-vercel-verify") == "verify-me"; got != c.verifyHdr {
				t.Errorf("x-vercel-verify present=%v want %v", got, c.verifyHdr)
			}
			if c.topic == "" {
				if len(sink.recs) != 0 {
					t.Errorf("unexpected records: %d", len(sink.recs))
				}
				return
			}
			if len(sink.recs) == 0 || sink.recs[0].Topic != c.topic {
				t.Fatalf("records=%d", len(sink.recs))
			}
			rec := sink.recs[0]
			hdr := map[string]string{}
			for _, h := range rec.Headers {
				hdr[h.Key] = string(h.Value)
			}
			if hdr[topics.HeaderProjectID] != "9" || hdr[topics.HeaderClientIP] != "" || hdr[topics.HeaderUserAgent] != "" {
				t.Errorf("headers: %v", hdr)
			}
			switch c.topic {
			case topics.Logs:
				var req collogspb.ExportLogsServiceRequest
				if err := proto.Unmarshal(rec.Value, &req); err != nil {
					t.Fatal(err)
				}
				if req.ResourceLogs[0].Resource.Attributes[0].GetValue().GetStringValue() != "shop-web" ||
					req.ResourceLogs[0].ScopeLogs[0].LogRecords[0].GetBody().GetStringValue() != "boom" {
					t.Errorf("logs: %v", &req)
				}
			case topics.RUM:
				if len(sink.recs) != 1 { // same (empty) client resource → one batch
					t.Errorf("rum records=%d", len(sink.recs))
				}
				var b rumpb.RumBatch
				if err := proto.Unmarshal(rec.Value, &b); err != nil {
					t.Fatal(err)
				}
				if len(b.Events) != 2 || b.Events[0].Type != rumpb.EventType_VITAL || b.Events[1].Type != rumpb.EventType_VIEW_START {
					t.Errorf("rum: %v", &b)
				}
			case topics.Traces:
				var req coltracepb.ExportTraceServiceRequest
				if err := proto.Unmarshal(rec.Value, &req); err != nil {
					t.Fatal(err)
				}
				if len(req.ResourceSpans[0].ScopeSpans[0].Spans) != 2 {
					t.Errorf("trace: %v", &req)
				}
			}
		})
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := drainServer(&memSink{})
	logs := []byte(`[{"source":"lambda","projectName":"my-app","message":"m","timestamp":1}]`)
	h.ServeHTTP(httptest.NewRecorder(), drainReq("POST", "key=good&integration=4", logs, "x-vercel-signature", "00"))
	h.ServeHTTP(httptest.NewRecorder(), drainReq("POST", "key=good&integration=4", logs, "x-vercel-signature", vercel.Sign([]byte("drain-secret"), logs)))
	r := httptest.NewRequest("POST", "/v1/logs", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), r) // no key → 401

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	out := rr.Body.String()
	for _, want := range []string{
		`tt_ingest_gw_requests_total{endpoint="/v1/drains/vercel",status="200"}`,
		`tt_ingest_gw_requests_total{endpoint="/v1/drains/vercel",status="403"}`,
		`tt_ingest_gw_auth_failures_total{endpoint="/v1/drains/vercel",reason="signature"}`,
		`tt_ingest_gw_auth_failures_total{endpoint="/v1/logs",reason="invalid_key"}`,
		`tt_ingest_gw_drain_events_total{kind="log",provider="vercel"}`,
		`tt_ingest_gw_request_bytes_total{endpoint="/v1/drains/vercel"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}
