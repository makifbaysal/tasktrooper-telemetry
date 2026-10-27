package gw

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

const testInternalToken = "s3cret-internal"

func newInternalServer(sink Sink, enabled bool) *Server {
	s := newTestServer(sink, denyLimiter{}, 1<<20, 900<<10)
	if enabled {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		s.SetInternal(auth.NewInternal(testInternalToken, auth.StaticProjects{9601: 42}, log))
	}
	return s
}

func TestInternalCredentialHTTP(t *testing.T) {
	pb, _ := proto.Marshal(traceReq(1))
	rumJSON := []byte(`{"resource":{"app":"x"},"events":[{"type":"VIEW_START","ts_ms":1700000000000}]}`)
	tests := []struct {
		name       string
		enabled    bool
		path       string
		ct         string
		hdr        map[string]string
		body       []byte
		wantStatus int
		wantTopic  string
	}{
		{"otlp traces", true, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": testInternalToken, "x-trooper-project": "9601"}, pb, 200, topics.Traces},
		{"rum", true, "/v1/rum", "application/json", map[string]string{"x-trooper-internal": testInternalToken, "x-trooper-project": "9601"}, rumJSON, 200, topics.RUM},
		{"internal wins over bad key", true, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": testInternalToken, "x-trooper-project": "9601", "x-trooper-key": "nope"}, pb, 200, topics.Traces},
		{"wrong token", true, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": "guess", "x-trooper-project": "9601"}, pb, 401, ""},
		{"wrong token with valid key", true, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": "guess", "x-trooper-project": "9601", "x-trooper-key": "good"}, pb, 401, ""},
		{"unknown project", true, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": testInternalToken, "x-trooper-project": "9999"}, pb, 401, ""},
		{"missing project", true, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": testInternalToken}, pb, 401, ""},
		{"non-numeric project", true, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": testInternalToken, "x-trooper-project": "9601x"}, pb, 401, ""},
		{"browser origin rejected", true, "/v1/rum", "application/json", map[string]string{"x-trooper-internal": testInternalToken, "x-trooper-project": "9601", "Origin": "https://shop.example.com"}, rumJSON, 403, ""},
		{"feature off", false, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-internal": testInternalToken, "x-trooper-project": "9601"}, pb, 401, ""},
		{"feature off key still works", false, "/v1/traces", "application/x-protobuf", map[string]string{"x-trooper-key": "good"}, pb, 200, topics.Traces},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &memSink{}
			h := newInternalServer(sink, tt.enabled).Handler()
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.ct)
			for k, v := range tt.hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantTopic == "" {
				if len(sink.recs) != 0 {
					t.Fatalf("unexpected records: %d", len(sink.recs))
				}
				return
			}
			if len(sink.recs) != 1 || sink.recs[0].Topic != tt.wantTopic {
				t.Fatalf("records: %+v", sink.recs)
			}
			if tt.hdr["x-trooper-key"] == "good" {
				return
			}
			got := map[string]string{}
			for _, h := range sink.recs[0].Headers {
				got[h.Key] = string(h.Value)
			}
			if got[topics.HeaderOrgID] != "42" || got[topics.HeaderProjectID] != "9601" {
				t.Fatalf("tenant headers: %v", got)
			}
		})
	}
}

func TestInternalCredentialGRPC(t *testing.T) {
	sink := &memSink{}
	s := newInternalServer(sink, true)
	md := func(kv ...string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs(kv...))
	}
	if err := s.grpcExport(md("x-trooper-internal", testInternalToken, "x-trooper-project", "9601"), sigTraces, traceReq(1)); err != nil {
		t.Fatal(err)
	}
	if len(sink.recs) != 1 || string(sink.recs[0].Key) != "9601" {
		t.Fatalf("records: %+v", sink.recs)
	}
	if err := s.grpcExport(md("x-trooper-internal", "bad", "x-trooper-project", "9601"), sigTraces, traceReq(1)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: %v", err)
	}
	if err := s.grpcExport(md("x-trooper-internal", testInternalToken, "x-trooper-project", "9601", "origin", "https://x.io"), sigTraces, traceReq(1)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("origin: %v", err)
	}
	// Key auth over gRPC keeps ignoring forwarded origins.
	if err := s.grpcExport(md("x-trooper-key", "browser", "origin", "https://evil.com"), sigTraces, traceReq(1)); err != nil {
		t.Fatalf("key with origin: %v", err)
	}
	off := newInternalServer(sink, false)
	if err := off.grpcExport(md("x-trooper-internal", testInternalToken, "x-trooper-project", "9601"), sigTraces, traceReq(1)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("feature off: %v", err)
	}
}

func TestInternalPreflightDoesNotAllowHeader(t *testing.T) {
	h := newInternalServer(&memSink{}, true).Handler()
	req := httptest.NewRequest(http.MethodOptions, "/v1/rum", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	req.Header.Set("Access-Control-Request-Headers", "x-trooper-internal")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if bytes.Contains(bytes.ToLower([]byte(rec.Header().Get("Access-Control-Allow-Headers"))), []byte("x-trooper-internal")) {
		t.Fatal("preflight must not allow x-trooper-internal")
	}
}
