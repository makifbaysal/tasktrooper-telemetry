package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/llmapi"
)

// The summarizer reads the shared endpoint (TT_ASSISTANT_*): key and model set → real provider, else the mock.
func TestNewFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, key, model string
		wantModel        string
	}{
		{"unset is mock", "", "", ""},
		{"key alone is mock", "secret", "", ""},
		{"model alone is mock", "", "muse", ""},
		{"key and model is real", "secret", "muse", "muse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(llmapi.EnvURL, "")
			t.Setenv(llmapi.EnvKey, tc.key)
			t.Setenv(llmapi.EnvModel, tc.model)
			p := NewFromEnv()
			if p.Model() != tc.wantModel {
				t.Fatalf("model %q, want %q", p.Model(), tc.wantModel)
			}
			if tc.wantModel == "" {
				got, err := p.Summarize(context.Background(), Input{Title: "Checkout errors"})
				if err != nil || got.Provider != "mock" || got.Usage != (Usage{}) {
					t.Fatalf("mock result=%+v err=%v", got, err)
				}
			}
		})
	}
}

func newRemote(t *testing.T, url string, hc *http.Client) Summarizer {
	t.Helper()
	p := New(llmapi.New(url, "test-key", "test-model"), hc)
	if p.Model() != "test-model" {
		t.Fatalf("not a remote provider: %T", p)
	}
	return p
}

func TestRemoteProviderFailures(t *testing.T) {
	for _, tc := range []struct {
		name, response, want string
		status               int
		wantUsage            Usage
	}{
		{"rate limited", `{"error":"rate limited"}`, "HTTP 429", http.StatusTooManyRequests, Usage{}},
		{"server error", `{"error":"unavailable"}`, "HTTP 503", http.StatusServiceUnavailable, Usage{}},
		{"invalid JSON", `{`, "decode response", http.StatusOK, Usage{}},
		{"empty choices", `{"choices":[]}`, "no choices", http.StatusOK, Usage{}},
		{"blank summary is still billed", `{"choices":[{"message":{"content":"  "}}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`, "no summary", http.StatusOK, Usage{7, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer ts.Close()
			got, err := newRemote(t, ts.URL, ts.Client()).Summarize(context.Background(), Input{Title: "Incident"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || got != (Result{Usage: tc.wantUsage}) || requests != 1 {
				t.Fatalf("result=%+v err=%v requests=%d; want %q", got, err, requests, tc.want)
			}
		})
	}
}

func TestRemoteHonorsContextFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
		{"deadline exceeded", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()
			ctx, cancel := tc.ctx()
			defer cancel()
			got, err := newRemote(t, ts.URL, ts.Client()).Summarize(ctx, Input{Title: "Incident"})
			if !errors.Is(err, tc.want) || got != (Result{}) || requests != 0 {
				t.Fatalf("result=%+v err=%v requests=%d", got, err, requests)
			}
		})
	}
}

func TestRemoteRequestAndUsage(t *testing.T) {
	for _, tc := range []struct {
		name, base, wantPath string
	}{
		{"base URL", "/v1", "/v1/chat/completions"},
		{"full URL", "/v1/chat/completions", "/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.wantPath || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("unexpected request %s %s", r.URL.Path, r.Header.Get("Authorization"))
				}
				var req llmapi.Request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.Model != "test-model" || req.MaxTokens != IncidentSummaryMaxOutputTokens || len(req.Messages) != 2 ||
					req.Messages[0].Role != "system" || !strings.Contains(req.Messages[1].Content, "5xx rate") {
					t.Errorf("unexpected payload: %+v", req)
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Likely impact on checkout; cause unknown."}}],"usage":{"prompt_tokens":42,"completion_tokens":9}}`))
			}))
			defer ts.Close()
			got, err := newRemote(t, ts.URL+tc.base, ts.Client()).Summarize(context.Background(),
				Input{Title: "Checkout errors", Evidence: []string{"5xx rate rose"}})
			// httptest hosts are unknown → OpenAI-compatible "openai"
			if err != nil || got.Provider != "openai" || got.Summary == "" || got.Usage != (Usage{42, 9}) {
				t.Fatalf("result=%+v err=%v", got, err)
			}
		})
	}
}

func TestRemoteRejectsOversizedInputBeforeNetwork(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	got, err := newRemote(t, ts.URL, ts.Client()).Summarize(context.Background(),
		Input{Title: "Incident", Evidence: []string{strings.Repeat("x", IncidentSummaryMaxInputBytes)}})
	if !errors.Is(err, ErrInputTooLarge) || got != (Result{}) || requests != 0 {
		t.Fatalf("result=%+v err=%v requests=%d", got, err, requests)
	}
}
