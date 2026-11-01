package selftel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tasktrooper/telemetry-go/troopertest"
	"go.opentelemetry.io/otel/trace"
)

func TestHandlerSkipsHealthAndMetrics(t *testing.T) {
	cases := []struct {
		method, path string
		traced       bool
		route        string
	}{
		{"GET", "/healthz", false, ""},
		{"GET", "/metrics", false, ""},
		{"POST", "/v1/webhooks/github", true, "/v1/webhooks/github"},
		{"POST", "/v1/webhooks/gitlab", true, "/v1/webhooks/gitlab"},
	}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /metrics", ok)
	mux.HandleFunc("POST /v1/webhooks/github", ok)
	mux.HandleFunc("POST /v1/webhooks/{provider}", ok)
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			h := troopertest.New(t, "tt-codeintel")
			rec := httptest.NewRecorder()
			Handler(mux).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			s := h.SpanByKind(trace.SpanKindServer)
			if (s != nil) != tc.traced {
				t.Fatalf("traced=%v, want %v", s != nil, tc.traced)
			}
			if s != nil && tc.route == "/v1/webhooks/github" && troopertest.AttrString(s, "trooper.route") != tc.route {
				t.Fatalf("route %q", troopertest.AttrString(s, "trooper.route"))
			}
		})
	}
}

func TestConfigEnv(t *testing.T) {
	cases := []struct {
		name                 string
		env                  map[string]string
		wantService, wantEnv string
	}{
		{"defaults", nil, "tt-codeintel", "development"},
		{"tt env", map[string]string{"TT_ENV": "production"}, "tt-codeintel", "production"},
		{"overrides", map[string]string{"TROOPER_SERVICE": "ci-x", "TROOPER_ENV": "staging", "TT_ENV": "production"}, "ci-x", "staging"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"OTEL_SERVICE_NAME", "TROOPER_SERVICE", "TROOPER_ENV", "TT_ENV", "TROOPER_VERSION"} {
				t.Setenv(k, tc.env[k])
			}
			c := Config("tt-codeintel", "dev")
			if c.Service != tc.wantService || c.Environment != tc.wantEnv || c.Version != "dev" {
				t.Fatalf("%+v", c)
			}
		})
	}
}
