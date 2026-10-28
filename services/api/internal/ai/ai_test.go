package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderSelectionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, provider, allow, key string
		wantErr                    bool
	}{
		{"default mock", "", "", "", false},
		{"explicit mock ignores key", "mock", "true", "secret", false},
		{"meta needs opt in", "meta", "", "secret", true},
		{"meta needs key", "meta", "true", "", true},
		{"baseten needs opt in", "baseten", "", "secret", true},
		{"unknown provider", "unknown", "true", "secret", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TT_AI_PROVIDER", tc.provider)
			t.Setenv("TT_AI_ALLOW_PAID", tc.allow)
			t.Setenv("MODEL_API_KEY", tc.key)
			t.Setenv("BASETEN_API_KEY", tc.key)
			p, err := NewFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("provider=%T err=%v", p, err)
			}
			if !tc.wantErr {
				got, err := p.Summarize(context.Background(), Input{Title: "Checkout errors"})
				if err != nil || got.Provider != "mock" || got.Usage != (Usage{}) {
					t.Fatalf("mock result=%+v err=%v", got, err)
				}
			}
		})
	}
}

func TestRemoteRequestAndUsage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var req struct {
			Model     string    `json:"model"`
			Messages  []message `json:"messages"`
			MaxTokens int       `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "test-model" || req.MaxTokens != 300 || len(req.Messages) != 2 ||
			req.Messages[0].Role != "developer" || !strings.Contains(req.Messages[1].Content, "5xx rate") {
			t.Errorf("unexpected payload: %+v", req)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Likely impact on checkout; cause unknown."}}],"usage":{"prompt_tokens":42,"completion_tokens":9}}`))
	}))
	defer ts.Close()
	p := &remote{provider: "meta", endpoint: ts.URL + "/v1/chat/completions", model: "test-model", key: "test-key", client: ts.Client()}
	got, err := p.Summarize(context.Background(), Input{Title: "Checkout errors", Evidence: []string{"5xx rate rose"}})
	if err != nil || got.Provider != "meta" || got.Usage.InputTokens != 42 || got.Usage.OutputTokens != 9 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}
