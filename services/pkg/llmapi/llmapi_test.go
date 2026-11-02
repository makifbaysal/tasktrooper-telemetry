package llmapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatCompletionsURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", DefaultURL + "/chat/completions"},
		{"https://api.meta.ai/v1", "https://api.meta.ai/v1/chat/completions"},
		{"https://api.meta.ai/v1/", "https://api.meta.ai/v1/chat/completions"},
		{"https://gw.example/v1/chat/completions", "https://gw.example/v1/chat/completions"},
		{"  http://localhost:8000/v1  ", "http://localhost:8000/v1/chat/completions"},
	} {
		if got := ChatCompletionsURL(tc.in); got != tc.want {
			t.Errorf("ChatCompletionsURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, url, key, model string
		want                  Config
		enabled               bool
	}{
		{"unset", "", "", "", Config{URL: DefaultURL + "/chat/completions"}, false},
		{"key only", "", "k", "", Config{URL: DefaultURL + "/chat/completions", Key: "k"}, false},
		{"model only", "", "", "m", Config{URL: DefaultURL + "/chat/completions", Model: "m"}, false},
		{"key and model", "https://gw.example/v1/", " k ", " m ", Config{URL: "https://gw.example/v1/chat/completions", Key: "k", Model: "m"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvURL, tc.url)
			t.Setenv(EnvKey, tc.key)
			t.Setenv(EnvModel, tc.model)
			got := FromEnv()
			if got != tc.want || got.Enabled() != tc.enabled {
				t.Fatalf("FromEnv() = %+v enabled=%v", got, got.Enabled())
			}
		})
	}
}

func TestProvider(t *testing.T) {
	for _, tc := range []struct{ url, want, host string }{
		{"https://api.meta.ai/v1/chat/completions", "meta", "api.meta.ai"},
		{"https://inference.baseten.co/v1/chat/completions", "baseten", "inference.baseten.co"},
		{"https://openrouter.ai/api/v1/chat/completions", "openrouter", "openrouter.ai"},
		{"https://api.openai.com/v1/chat/completions", "openai", "api.openai.com"},
		{"http://127.0.0.1:9000/v1/chat/completions", "openai", "127.0.0.1:9000"},
	} {
		c := Config{URL: tc.url}
		if got := c.Provider(); got != tc.want {
			t.Errorf("Provider(%q) = %q, want %q", tc.url, got, tc.want)
		}
		if got := c.Host(); got != tc.host {
			t.Errorf("Host(%q) = %q, want %q", tc.url, got, tc.host)
		}
	}
}

func TestComplete(t *testing.T) {
	ok := `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10}}}`
	for _, tc := range []struct {
		name      string
		statuses  []int // per attempt; last one repeats
		body      string
		retries   int
		wantCalls int32
		wantErr   string
		want      Response
	}{
		{"ok", []int{200}, ok, 0, 1, "", Response{Content: "hi", FinishReason: "stop", Usage: Usage{Prompt: 40, Completion: 5, Cached: 10}}},
		{"retry 429 then ok", []int{429, 200}, ok, 2, 2, "", Response{Content: "hi", FinishReason: "stop", Usage: Usage{Prompt: 40, Completion: 5, Cached: 10}}},
		{"5xx exhausts retries", []int{503}, ok, 1, 2, "HTTP 503", Response{}},
		{"400 is not retried", []int{400}, ok, 3, 1, "HTTP 400", Response{}},
		{"invalid JSON is not retried", []int{200}, `{`, 3, 1, "decode response", Response{}},
		{"no choices keeps usage", []int{200}, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":0}}`, 3, 1, "no choices", Response{Usage: Usage{Prompt: 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1)) - 1
				var req Request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "m" || req.Stream ||
					r.Header.Get("Authorization") != "Bearer k" || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("request %s %+v err=%v", r.URL.Path, req, err)
				}
				w.WriteHeader(tc.statuses[min(n, len(tc.statuses)-1)])
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := &Client{Config: New(srv.URL+"/v1", "k", "m"), HTTP: srv.Client(), Retries: tc.retries, Backoff: time.Millisecond}
			got, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err %v, want %q", err, tc.wantErr)
			}
			if got != tc.want || calls.Load() != tc.wantCalls {
				t.Fatalf("got %+v calls %d", got, calls.Load())
			}
		})
	}
}

func TestCompleteHonorsContext(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{Config: New(srv.URL, "k", "m"), HTTP: srv.Client(), Retries: 3}
	if _, err := c.Complete(ctx, Request{}); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("err %v calls %d", err, calls.Load())
	}
}
