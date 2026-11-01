package server

import (
	"bytes"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"
)

// Self telemetry (ADR-027): routes get a server span with the mux route template and tt.project_id; /healthz none.
func TestSelfTelemetrySpans(t *testing.T) {
	h := troopertest.New(t, "tt-symbolicator")
	e := newEnv(t, 0, 0)
	tests := []struct {
		name, method, path, key string
		wantRoute               string // "" = no span
		wantProject             int64
	}{
		{name: "healthz untraced", method: "GET", path: "/healthz"},
		{name: "authed route", method: "POST", path: "/v1/symbolicate", key: "tt_dev_api_key", wantRoute: "/v1/symbolicate", wantProject: 1},
		{name: "rejected key still traced", method: "GET", path: "/v1/artifacts", key: "bad", wantRoute: "/v1/artifacts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.Recorder.Reset()
			req, _ := http.NewRequest(tc.method, e.srv.URL+tc.path, bytes.NewReader([]byte(`{}`)))
			if tc.key != "" {
				req.Header.Set("Authorization", "Bearer "+tc.key)
			}
			do(t, req)
			s := h.SpanByKind(trace.SpanKindServer)
			if tc.wantRoute == "" {
				if s != nil {
					t.Fatalf("unexpected span %q", s.Name())
				}
				return
			}
			if s == nil {
				t.Fatal("no server span")
			}
			if v, _ := troopertest.Attr(s, "trooper.route"); v.AsString() != tc.wantRoute {
				t.Errorf("trooper.route = %q, want %q", v.AsString(), tc.wantRoute)
			}
			if v, _ := troopertest.Attr(s, "tt.project_id"); v.AsInt64() != tc.wantProject {
				t.Errorf("tt.project_id = %d, want %d", v.AsInt64(), tc.wantProject)
			}
		})
	}
}
