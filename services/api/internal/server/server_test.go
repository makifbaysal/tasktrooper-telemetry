package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/rules"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// fakeStore implements only what the tests call; anything else panics through the nil embedded interface.
type fakeStore struct {
	Store
	t       *testing.T
	gotQ    store.Q
	gotTen  tenant.Tenant
	svcErr  error
	lag     rules.LagInputs
	monitor *model.Monitor
	audit   []store.AuditRecord
}

func (f *fakeStore) WriteAudit(_ context.Context, a store.AuditRecord) error {
	f.audit = append(f.audit, a)
	return nil
}

func (f *fakeStore) ListServices(ctx context.Context, q store.Q) ([]model.Service, error) {
	f.gotQ = q
	f.gotTen, _ = tenant.From(ctx)
	if f.svcErr != nil {
		return nil, f.svcErr
	}
	return []model.Service{{ID: "checkout", Name: "checkout"}}, nil
}

func (f *fakeStore) GetService(context.Context, store.Q, string) (*model.Service, error) {
	panic("boom")
}

func (f *fakeStore) LagInputs(_ context.Context, w timerange.Window, _ model.QueueRule) (rules.LagInputs, error) {
	if f.lag.Lag == nil {
		f.lag.Lag = make([]float64, w.Points)
		for i := range f.lag.Lag {
			f.lag.Lag[i] = 5000 // constantly above threshold
		}
	}
	return f.lag, nil
}

func (f *fakeStore) GetMonitor(_ context.Context, id string) (*model.Monitor, error) {
	if f.monitor == nil || f.monitor.ID != id {
		return nil, store.ErrNotFound
	}
	return f.monitor, nil
}

func (f *fakeStore) TransitionIncident(_ context.Context, _, action string) (*model.Incident, error) {
	if action == "ack" {
		return nil, store.ErrConflict
	}
	return nil, store.Invalid("unknown action")
}

func (f *fakeStore) CollectorConfig(context.Context) (model.CollectorConfig, error) {
	return model.CollectorConfig{Interval: 15, Databases: []model.CollectorDatabase{}, BrokerClusters: []model.CollectorCluster{}}, nil
}

var testNow = time.Date(2026, 10, 25, 12, 0, 30, 0, time.UTC)

func newTestServer(t *testing.T, fs *fakeStore) http.Handler {
	fs.t = t
	s := &Server{
		Store:   fs,
		AI:      ai.NewMock(),
		Auth:    auth.Static{"good": {OrgID: 7, ProjectID: 42}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Origins: []string{"http://localhost:5173"},
		Now:     func() time.Time { return testNow },
		Test: func(_ context.Context, in model.ConnTestInput) model.ConnTestResult {
			return model.ConnTestResult{OK: in.Kind == "redis", Message: "fake " + in.Kind}
		},
	}
	return s.Handler()
}

func do(h http.Handler, method, path, key, body string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHandlerStatuses(t *testing.T) {
	tests := []struct {
		name   string
		store  fakeStore
		method string
		path   string
		key    string
		body   string
		want   int
	}{
		{name: "healthz is public", method: "GET", path: "/healthz", want: 200},
		{name: "missing key", method: "GET", path: "/api/v1/services", want: 401},
		{name: "unknown key", method: "GET", path: "/api/v1/services", key: "bad", want: 401},
		{name: "ok", method: "GET", path: "/api/v1/services?range=6h", key: "good", want: 200},
		{name: "bad range", method: "GET", path: "/api/v1/services?range=2y", key: "good", want: 400},
		{name: "not found", store: fakeStore{svcErr: store.ErrNotFound}, method: "GET", path: "/api/v1/services", key: "good", want: 404},
		{name: "invalid", store: fakeStore{svcErr: store.Invalid("nope")}, method: "GET", path: "/api/v1/services", key: "good", want: 400},
		{name: "internal", store: fakeStore{svcErr: errors.New("db down")}, method: "GET", path: "/api/v1/services", key: "good", want: 500},
		{name: "panic recovered", method: "GET", path: "/api/v1/services/x", key: "good", want: 500},
		{name: "unknown route", method: "GET", path: "/api/v1/nope", key: "good", want: 404},
		{name: "conflict", method: "POST", path: "/api/v1/incidents/1/ack", key: "good", want: 409},
		{name: "bad json", method: "POST", path: "/api/v1/monitors", key: "good", body: "{", want: 400},
		{name: "missing monitor", method: "GET", path: "/api/v1/monitors/9/preview", key: "good", want: 404},
		{name: "unsupported preview", method: "POST", path: "/api/v1/monitors/preview", key: "good", body: `{"kind":"anomaly"}`, want: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := tt.store
			w := do(newTestServer(t, &fs), tt.method, tt.path, tt.key, tt.body)
			if w.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.want, w.Body)
			}
			if w.Code >= 400 && w.Code != 404 || strings.HasPrefix(tt.path, "/api/v1/monitors/9") {
				var e apiError
				if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error == "" {
					t.Errorf("error body %q", w.Body)
				}
			}
		})
	}
}

func TestTenantAndRangeReachStore(t *testing.T) {
	fs := &fakeStore{}
	w := do(newTestServer(t, fs), "GET", "/api/v1/services?range=24h&env=prod", "good", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if fs.gotTen != (tenant.Tenant{OrgID: 7, ProjectID: 42}) {
		t.Errorf("tenant %+v", fs.gotTen)
	}
	if fs.gotQ.Env != "prod" || fs.gotQ.W.Points != 96 || !fs.gotQ.Now.Equal(testNow) {
		t.Errorf("q %+v", fs.gotQ)
	}
	var svcs []model.Service
	if err := json.Unmarshal(w.Body.Bytes(), &svcs); err != nil || len(svcs) != 1 || svcs[0].ID != "checkout" {
		t.Errorf("body %s", w.Body)
	}
}

func TestCORS(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	w := do(h, "OPTIONS", "/api/v1/services", "", "", "Origin", "http://localhost:5173", "Access-Control-Request-Method", "GET")
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("preflight: %d %v", w.Code, w.Header())
	}
	w = do(h, "GET", "/api/v1/services", "good", "", "Origin", "http://evil.example")
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign origin allowed")
	}
}

func TestGzip(t *testing.T) {
	w := do(newTestServer(t, &fakeStore{}), "GET", "/api/v1/services", "good", "", "Accept-Encoding", "gzip")
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("no gzip: %v", w.Header())
	}
}

func TestPreviewConsumerLag(t *testing.T) {
	body := `{"name":"lag","kind":"consumer_lag","severity":"warning","queue":{"clusterId":"k","group":"g","metric":"lag","threshold":1000,"forMin":5}}`
	w := do(newTestServer(t, &fakeStore{}), "POST", "/api/v1/monitors/preview?range=1h", "good", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var p model.Preview
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Data) != 60 || len(p.Timestamps) != 60 || !p.FiringNow || p.Episodes != 1 || p.Th != 1000 {
		t.Errorf("preview %+v", p)
	}
}

func TestTestConnectionAndCollectorConfig(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	w := do(h, "POST", "/api/v1/infra/databases/test-connection", "good", `{"kind":"redis","host":"localhost"}`)
	var res model.ConnTestResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || !res.OK || res.Message != "fake redis" {
		t.Errorf("test-connection: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/api/v1/internal/collector-config", "good", "")
	var cfg model.CollectorConfig
	_ = json.Unmarshal(w.Body.Bytes(), &cfg)
	if w.Code != 200 || cfg.ProjectID != 42 || cfg.OrgID != 7 || cfg.Interval != 15 {
		t.Errorf("collector-config: %d %s", w.Code, w.Body)
	}
}
