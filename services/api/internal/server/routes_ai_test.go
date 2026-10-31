package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
)

type failingSummarizer struct{ calls int }

func (f *failingSummarizer) Summarize(context.Context, ai.Input) (ai.Result, error) {
	f.calls++
	return ai.Result{}, errors.New("provider secret and internal failure details")
}

func TestIncidentSummaryMock(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	path := "/api/v1/ai/incident-summary"
	body := `{"title":"Checkout errors","evidence":["5xx rate rose to 8%"]}`
	if got := do(h, http.MethodPost, path, "", body); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got.Code)
	}
	got := do(h, http.MethodPost, path, "good", body)
	if got.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
	}
	var result ai.Result
	if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil || result.Provider != "mock" || result.Usage != (ai.Usage{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := do(h, http.MethodPost, path, "good", `{"title":"","evidence":[]}`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d", got.Code)
	}
	items := make([]string, 9)
	for i := range items {
		items[i] = strings.Repeat("x", 500)
	}
	oversized, _ := json.Marshal(ai.Input{Title: "Incident", Evidence: items})
	if got := do(h, http.MethodPost, path, "good", string(oversized)); got.Code != http.StatusBadRequest ||
		!strings.Contains(got.Body.String(), "exceeds 4000 bytes") {
		t.Fatalf("oversized status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestIncidentSummaryProviderFailure(t *testing.T) {
	f := &failingSummarizer{}
	s := &Server{Store: &fakeStore{}, AI: f, Auth: auth.Static{"good": {OrgID: 7, ProjectID: 42}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	path := "/api/v1/ai/incident-summary"
	body := `{"title":"Checkout errors","evidence":["5xx rate rose"]}`
	if got := do(s.Handler(), http.MethodPost, path, "good", body); got.Code != http.StatusBadGateway ||
		got.Body.String() != "{\"error\":\"AI provider failed\"}\n" {
		t.Fatalf("status=%d body=%q", got.Code, got.Body.String())
	}
	if f.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", f.calls)
	}
}
