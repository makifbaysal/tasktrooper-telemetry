package server

import (
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry-go/troopertest"
)

// Self telemetry (ADR-027): API routes get a server span with the chi route template and the tenant as span
// attributes; health checks produce no span.
func TestSelfTelemetrySpans(t *testing.T) {
	h := troopertest.New(t, "tt-api")
	handler := identityServer(t, newIdentityFake(), nil).Handler()
	tests := []struct {
		name, method, path string
		hdr                []string
		wantRoute          string // "" = no span
		wantProject        int64
	}{
		{name: "healthz untraced", method: "GET", path: "/healthz"},
		{name: "cors preflight untraced", method: "OPTIONS", path: "/api/v1/me",
			hdr: []string{"Origin", "http://localhost:5173", "Access-Control-Request-Method", "GET"}},
		{name: "route template", method: "DELETE", path: "/api/v1/api-keys/3", hdr: []string{"Authorization", "Bearer good"},
			wantRoute: "/api/v1/api-keys/:id", wantProject: 42},
		{name: "unauthenticated still traced", method: "GET", path: "/api/v1/me", wantRoute: "/api/v1/me"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.Recorder.Reset()
			do(handler, tc.method, tc.path, "", "", tc.hdr...)
			span := h.SpanByKind(trace.SpanKindServer)
			if tc.wantRoute == "" {
				if span != nil {
					t.Fatalf("unexpected span %q", span.Name())
				}
				return
			}
			if span == nil {
				t.Fatal("no server span")
			}
			if v, _ := troopertest.Attr(span, "trooper.route"); v.AsString() != tc.wantRoute {
				t.Errorf("trooper.route = %q, want %q", v.AsString(), tc.wantRoute)
			}
			v, ok := troopertest.Attr(span, "tt.project_id")
			if tc.wantProject == 0 && ok || tc.wantProject != 0 && v.AsInt64() != tc.wantProject {
				t.Errorf("tt.project_id = %v (%v), want %d", v.AsInt64(), ok, tc.wantProject)
			}
		})
	}
}
