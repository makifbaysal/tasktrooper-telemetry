package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/symbolicate"
)

type adminFake struct {
	fakeStore
	patch   model.MonitorPatch
	preview []float64
	volume  []float64
}

func (f *adminFake) GetError(_ context.Context, _ store.Q, fp string) (*model.ErrorDetail, error) {
	if fp != "42" {
		return nil, store.ErrNotFound
	}
	return &model.ErrorDetail{ErrorIssue: model.ErrorIssue{Fingerprint: fp, Service: "checkout"}, Release: "1.2.3", Platform: "go",
		Stack: "raw stack", Frames: []model.Frame{}}, nil
}

func (f *adminFake) GetRumError(_ context.Context, _ store.Q, app, fp string) (*model.RumErrorDetail, error) {
	return &model.RumErrorDetail{RumError: model.RumError{Fingerprint: fp, AppVersion: "5.0"}, App: app, Platform: "web", Stack: "at a (app.min.js:1:200)"}, nil
}

func (f *adminFake) PatchMonitor(_ context.Context, id string, p model.MonitorPatch) (*model.Monitor, error) {
	f.patch = p
	return &model.Monitor{ID: id}, nil
}

func (f *adminFake) PreviewSeries(_ context.Context, q store.Q, kind string, _ model.MonitorTarget, _ json.RawMessage, _ model.MonitorCondition) ([]float64, []float64, error) {
	return f.preview, f.volume, nil
}

func adminServer(t *testing.T, f *adminFake, sym *symbolicate.Client, cloud CloudTester) http.Handler {
	f.t = t
	s := &Server{Store: f, Auth: auth.Static{"good": {OrgID: 7, ProjectID: 42}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return testNow }, Symbolicator: sym, CloudTest: cloud}
	return s.Handler()
}

func TestErrorDetailSymbolication(t *testing.T) {
	var gotAuth string
	var gotReq symbolicate.Request
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		if r.URL.Path != "/v1/symbolicate" {
			w.WriteHeader(404)
			return
		}
		if gotReq.App == "down" {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"stack": "at checkout (cart.ts:10:5)", "symbolicated": true,
			"frames": []model.Frame{{Fn: "checkout", File: "cart.ts", Line: 10, Col: 5, InApp: true}}})
	}))
	defer fake.Close()

	h := adminServer(t, &adminFake{}, symbolicate.New(fake.URL), nil)
	w := do(h, "GET", "/api/v1/errors/42?range=24h", "good", "")
	var d model.ErrorDetail
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || !d.Symbolicated || len(d.Frames) != 1 || d.Stack != "at checkout (cart.ts:10:5)" {
		t.Fatalf("detail %d %s", w.Code, w.Body)
	}
	if gotAuth != "Bearer good" || gotReq.App != "checkout" || gotReq.Release != "1.2.3" || gotReq.Stack != "raw stack" {
		t.Errorf("request auth=%q %+v", gotAuth, gotReq)
	}
	if w := do(h, "GET", "/api/v1/errors/7", "good", ""); w.Code != 404 {
		t.Errorf("missing: %d", w.Code)
	}

	// symbolicator error → raw stack
	w = do(h, "GET", "/api/v1/apps/down/errors/9", "good", "")
	var rd model.RumErrorDetail
	_ = json.Unmarshal(w.Body.Bytes(), &rd)
	if w.Code != 200 || rd.Symbolicated || rd.Stack != "at a (app.min.js:1:200)" || rd.Frames == nil {
		t.Errorf("fallback %d %s", w.Code, w.Body)
	}
	// no symbolicator configured → raw stack
	h = adminServer(t, &adminFake{}, nil, nil)
	w = do(h, "GET", "/api/v1/errors/42", "good", "")
	if !strings.Contains(w.Body.String(), `"stack":"raw stack"`) || !strings.Contains(w.Body.String(), `"frames":[]`) {
		t.Errorf("disabled: %s", w.Body)
	}
}

func TestSymbolicatorTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer slow.Close()
	c := symbolicate.New(slow.URL)
	c.HTTP.Timeout = 100 * time.Millisecond
	start := time.Now()
	stack, _, ok, err := c.Symbolicate(context.Background(), "", symbolicate.Request{Stack: "s"})
	if ok || err == nil || stack != "s" || time.Since(start) > 2*time.Second {
		t.Errorf("timeout: ok=%v err=%v stack=%q", ok, err, stack)
	}
}

func TestPatchMonitorMutedUntil(t *testing.T) {
	for _, tc := range []struct {
		body  string
		set   bool
		value int64
	}{
		{`{"mutedUntil": 1761400000000}`, true, 1761400000000},
		{`{"mutedUntil": null}`, true, 0},
		{`{"name": "x"}`, false, 0},
	} {
		f := &adminFake{}
		w := do(adminServer(t, f, nil, nil), "PATCH", "/api/v1/monitors/3", "good", tc.body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body)
		}
		var got int64
		if f.patch.MutedUntil != nil {
			got = *f.patch.MutedUntil
		}
		if f.patch.MutedUntilSet != tc.set || got != tc.value {
			t.Errorf("%s: set=%v value=%d", tc.body, f.patch.MutedUntilSet, got)
		}
	}
}

func TestIntegrationTestEndpoint(t *testing.T) {
	var gotSecret string
	cloud := func(_ context.Context, provider string, _ json.RawMessage, secret string) model.IntegrationTestResult {
		gotSecret = secret
		return model.IntegrationTestResult{OK: provider == "vercel", Message: "fake " + provider}
	}
	h := adminServer(t, &adminFake{}, nil, cloud)
	w := do(h, "POST", "/api/v1/integrations/test", "good", `{"provider":"vercel","config":{},"secret":"tok"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) || gotSecret != "tok" || strings.Contains(w.Body.String(), "tok\"") {
		t.Errorf("test: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/v1/integrations/test", "good", `{"provider":"azure"}`); w.Code != 400 {
		t.Errorf("unknown provider: %d", w.Code)
	}
	if w := do(adminServer(t, &adminFake{}, nil, nil), "POST", "/api/v1/integrations/test", "good", `{"provider":"neon"}`); w.Code != 501 {
		t.Errorf("unconfigured: %d", w.Code)
	}
}

func TestPreviewLogAndRumKinds(t *testing.T) {
	data := make([]float64, 60)
	vol := make([]float64, 60)
	for i := range data {
		data[i], vol[i] = 0.2, 100
	}
	vol[59] = 1 // last bucket below minRequests → not firing now, value still shown
	f := &adminFake{preview: data, volume: vol}
	body := `{"kind":"crash_rate","target":{"service":"shop-ios"},"condition":{"op":">","threshold":0.1,"forMin":5,"minRequests":10}}`
	w := do(adminServer(t, f, nil, nil), "POST", "/api/v1/monitors/preview?range=1h", "good", body)
	var p model.Preview
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != 200 || p.Episodes != 1 || p.FiringNow || p.Now != 0.2 || p.Th != 0.1 {
		t.Errorf("preview %d %+v", w.Code, p)
	}
	if nonBreaching(">=", 5) >= 5 || nonBreaching("<=", 5) <= 5 || nonBreaching(">", 5) != 5 {
		t.Error("nonBreaching")
	}
}

func TestOriginAllowed(t *testing.T) {
	dev := []string{"http://localhost:*", "http://127.0.0.1:*"}
	for o, want := range map[string]bool{
		"http://localhost:5173": true, "http://localhost:3000": true, "http://localhost": true, "http://127.0.0.1:8080": true,
		"http://localhost.evil.com": false, "http://localhost:80@evil.com": false, "https://localhost:5173": false, "http://evil.com": false,
	} {
		if got := OriginAllowed(dev, o); got != want {
			t.Errorf("%s: got %v", o, got)
		}
	}
	if !OriginAllowed([]string{"https://app.example.com"}, "https://app.example.com") || OriginAllowed([]string{"https://app.example.com"}, "https://x.example.com") {
		t.Error("exact match")
	}
}

func TestConnTestCloudQueues(t *testing.T) {
	h := adminServer(t, &adminFake{}, nil, nil)
	for _, k := range []string{"sqs", "pubsub"} {
		w := do(h, "POST", "/api/v1/infra/clusters/test-connection", "good", `{"kind":"`+k+`","host":"x"}`)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "integration") {
			t.Errorf("%s: %d %s", k, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/suggestions", "/repos", "/inventory"} {
		if w := do(h, "GET", "/api/v1"+p, "good", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
}
