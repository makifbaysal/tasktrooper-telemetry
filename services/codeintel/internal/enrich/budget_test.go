package enrich

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// fakeBudget allows `allow` reservations, then denies with reason.
type fakeBudget struct {
	allow    int
	reason   string
	reserves int
	estIn    []int64
	estOut   []int64
	recorded []Tokens
}

func (b *fakeBudget) Reserve(_ context.Context, model string, in, out int64) (bool, string) {
	b.reserves++
	b.estIn, b.estOut = append(b.estIn, in), append(b.estOut, out)
	if b.reserves > b.allow {
		return false, b.reason
	}
	return true, ""
}

func (b *fakeBudget) Record(_ context.Context, model string, u Tokens) float64 {
	b.recorded = append(b.recorded, u)
	return 1.5
}

// llmServer answers every request with fixed usage (1050 prompt tokens of which 50 cached, 200 completion);
// body controls the text.
func llmServer(t *testing.T, calls *atomic.Int32, body func(n int) string) *httptest.Server {
	t.Helper()
	srv, _ := chatServer(t, func(n int, _ chatReq) chatAnswer {
		calls.Add(1)
		return chatAnswer{content: body(n), prompt: 1050, cached: 50, completion: 200}
	})
	return srv
}

func okBody(int) string {
	return `{"items":[{"id":0,"criticality":"high","business_domain":"x","summary":"s","failure_modes":[],"suggested_monitors":[],"loadtest_hint":{"payload_example":"","idempotent":true,"notes":""}}]}`
}

func TestEnrichBudget(t *testing.T) {
	tests := []struct {
		name        string
		budget      *fakeBudget
		body        func(int) string
		wantCalls   int32
		wantStatus  string
		wantReason  string
		wantSkipped int
		wantLLM     int
		wantErr     bool
	}{
		{"unlimited", &fakeBudget{allow: 99}, okBody, 3, model.LLMOK, "", 0, 3, false},
		{"denied before first batch", &fakeBudget{allow: 0, reason: "org_limit"}, okBody, 0, model.LLMQuotaExceeded, "org_limit", 3, 0, false},
		{"denied mid-way", &fakeBudget{allow: 1, reason: "user_limit"}, okBody, 1, model.LLMQuotaExceeded, "user_limit", 2, 1, false},
		{"unusable answer is still recorded", &fakeBudget{allow: 99}, func(int) string { return "not json" }, 3, model.LLMOK, "", 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := llmServer(t, &calls, tt.body)
			l := newTestLLM(t, srv, "model-x")
			l.BatchSize = 1
			var items []*model.Item
			for i := range 3 {
				items = append(items, &model.Item{Kind: "endpoint", Key: fmt.Sprintf("POST /x/%d", i), Snippet: "code", Criticality: model.Medium})
			}
			out, err := l.Enrich(context.Background(), RepoSummary{Name: "acme/shop"}, items, tt.budget)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err %v", err)
			}
			if calls.Load() != tt.wantCalls || out.Status != tt.wantStatus || out.Reason != tt.wantReason || out.Skipped != tt.wantSkipped {
				t.Fatalf("calls %d out %+v", calls.Load(), out)
			}
			n := 0
			for _, it := range items {
				if it.LLM != nil {
					n++
				}
			}
			if n != tt.wantLLM {
				t.Fatalf("llm items %d", n)
			}
			if len(tt.budget.recorded) != int(tt.wantCalls) || out.Credits != 1.5*float64(tt.wantCalls) {
				t.Fatalf("recorded %v credits %v", tt.budget.recorded, out.Credits)
			}
			if tt.wantCalls > 0 {
				if got := tt.budget.recorded[0]; got != (Tokens{Input: 1000, CacheRead: 50, Output: 200}) {
					t.Fatalf("usage %+v", got)
				}
				if tt.budget.estIn[0] <= 0 || tt.budget.estOut[0] != OutputPerItem {
					t.Fatalf("estimate %v %v", tt.budget.estIn, tt.budget.estOut)
				}
			}
		})
	}
}
