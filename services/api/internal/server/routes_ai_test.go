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
	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

type failingSummarizer struct{ calls int }

func (f *failingSummarizer) Model() string { return "" }

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

// fakeSummarizer is a real-model summarizer double.
type fakeSummarizer struct {
	calls int
	err   error
}

func (f *fakeSummarizer) Model() string { return "test-model" }
func (f *fakeSummarizer) Summarize(context.Context, ai.Input) (ai.Result, error) {
	f.calls++
	return ai.Result{Summary: "s", Provider: "meta", Usage: ai.Usage{InputTokens: 30, OutputTokens: 6}}, f.err
}

// quotaFake answers reservations with allow and keeps the recorded usage.
type quotaFake struct {
	*fakeStore
	allow    bool
	reserves int
	usage    []aiquota.Usage
}

func (q *quotaFake) AssistantEstimate(string, int, int64) float64  { return 0.1 }
func (q *quotaFake) AssistantCredits(string, int64, int64) float64 { return 0.2 }
func (q *quotaFake) AssistantReserve(context.Context, int64, int64, int64, float64) (aiquota.Decision, error) {
	q.reserves++
	if !q.allow {
		return aiquota.Decision{Reason: aiquota.ReasonOrgLimit}, nil
	}
	return aiquota.Decision{Allowed: true}, nil
}
func (q *quotaFake) AssistantRecord(_ context.Context, u aiquota.Usage) error {
	q.usage = append(q.usage, u)
	return nil
}

func TestIncidentSummaryQuota(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allow       bool
		providerErr error
		wantStatus  int
		wantCalls   int
		wantRecords int
	}{
		{"allowed and recorded", true, nil, http.StatusOK, 1, 1},
		{"denied before the model call", false, nil, http.StatusTooManyRequests, 0, 0},
		{"failed answer is still recorded", true, errors.New("boom"), http.StatusBadGateway, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &quotaFake{fakeStore: &fakeStore{}, allow: tc.allow}
			f := &fakeSummarizer{err: tc.providerErr}
			s := &Server{Store: q, AI: f, Auth: auth.Static{"good": {OrgID: 7, ProjectID: 42}},
				Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			got := do(s.Handler(), http.MethodPost, "/api/v1/ai/incident-summary", "good", `{"title":"Checkout errors","evidence":["5xx rate rose"]}`)
			if got.Code != tc.wantStatus || f.calls != tc.wantCalls || q.reserves != 1 || len(q.usage) != tc.wantRecords {
				t.Fatalf("status=%d calls=%d reserves=%d usage=%+v body=%s", got.Code, f.calls, q.reserves, q.usage, got.Body.String())
			}
			if tc.wantRecords > 0 {
				u := q.usage[0]
				if u.Feature != aiquota.FeatureRCA || u.Model != "test-model" || u.InputTokens != 30 || u.OutputTokens != 6 ||
					u.Credits != 0.2 || u.OrgID != 7 || u.ProjectID != 42 || u.UserID != nil {
					t.Fatalf("usage %+v", u)
				}
			}
		})
	}
}
