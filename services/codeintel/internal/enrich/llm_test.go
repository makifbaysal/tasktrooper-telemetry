package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/llmapi"
)

// chatReq is the part of a chat completions request the tests look at.
type chatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxTokens      int64          `json:"max_tokens"`
	ResponseFormat map[string]any `json:"response_format"`
}

// chatAnswer is one fake chat completions answer.
type chatAnswer struct {
	status                     int // 0 = 200
	content, finish            string
	prompt, completion, cached int64
}

func (a chatAnswer) write(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	if a.status != 0 && a.status != http.StatusOK {
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(`{"error":{"message":"rejected"}}`))
		return
	}
	if a.finish == "" {
		a.finish = "stop"
	}
	usage := map[string]any{"prompt_tokens": a.prompt, "completion_tokens": a.completion, "total_tokens": a.prompt + a.completion}
	if a.cached > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": a.cached}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "model": "m",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": a.content},
			"finish_reason": a.finish}},
		"usage": usage})
}

// chatServer is a fake OpenAI-compatible endpoint; answer decides per request (n counts from 0).
func chatServer(t *testing.T, answer func(n int, req chatReq) chatAnswer) (*httptest.Server, func() []chatReq) {
	t.Helper()
	var mu sync.Mutex
	var reqs []chatReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, req)
		mu.Unlock()
		answer(n, req).write(w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []chatReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]chatReq(nil), reqs...)
	}
}

func newTestLLM(t *testing.T, srv *httptest.Server, modelID string) *LLM {
	t.Helper()
	l := NewLLM(llmapi.New(srv.URL+"/v1", "test-key", modelID))
	if l == nil {
		t.Fatal("NewLLM returned nil")
	}
	return l
}

const twoItems = `{"items":[{"id":0,"criticality":"critical","business_domain":"payments","summary":"Captures a payment.","failure_modes":["provider timeout"],"suggested_monitors":[{"template":"latency","reason":"provider calls"}],"loadtest_hint":{"payload_example":"{\"amount\":100}","idempotent":false,"notes":""}},{"id":1,"criticality":"low","business_domain":"internal","summary":"Health.","failure_modes":[],"suggested_monitors":[],"loadtest_hint":{"payload_example":"","idempotent":true,"notes":""}}]}`

func TestNewLLMDisabledWithoutConfig(t *testing.T) {
	for _, tc := range []struct{ key, model string }{{"", "m"}, {"k", ""}, {" ", " "}} {
		if NewLLM(llmapi.New("", tc.key, tc.model)) != nil {
			t.Fatalf("key %q model %q: LLM must be disabled without key and model", tc.key, tc.model)
		}
	}
}

func TestLLMEnrichJSONOutput(t *testing.T) {
	tests := []struct {
		name    string
		answers []chatAnswer // per request; the last one repeats
		wantErr string
		wantLLM bool
		// wantSchema per request: whether response_format json_schema was sent
		wantSchema []bool
	}{
		{"structured output", []chatAnswer{{content: twoItems, prompt: 10, completion: 10}}, "", true, []bool{true}},
		{"fenced JSON is accepted", []chatAnswer{{content: "Here you go:\n```json\n" + twoItems + "\n```", prompt: 10, completion: 10}}, "", true, []bool{true}},
		{"json_schema rejected → prompt-only retry", []chatAnswer{{status: http.StatusBadRequest}, {content: twoItems, prompt: 10, completion: 10}}, "", true, []bool{true, false}},
		{"truncated output", []chatAnswer{{content: `{"items":[`, finish: "length", prompt: 10, completion: 16000}}, "truncated", false, []bool{true}},
		{"refused", []chatAnswer{{content: "", finish: "content_filter", prompt: 10}}, "refused", false, []bool{true}},
		{"not JSON", []chatAnswer{{content: "sorry", prompt: 10, completion: 1}}, "decode JSON output", false, []bool{true}},
		{"unauthorized is not retried", []chatAnswer{{status: http.StatusUnauthorized}}, "HTTP 401", false, []bool{true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, reqs := chatServer(t, func(n int, _ chatReq) chatAnswer { return tt.answers[min(n, len(tt.answers)-1)] })
			l := newTestLLM(t, srv, "test-model-from-config")
			items := []*model.Item{
				{Kind: "endpoint", Key: "POST /v1/payments/:id/capture", Snippet: "func capture() {}", Criticality: model.Medium},
				{Kind: "endpoint", Key: "GET /healthz", Criticality: model.Low},
			}
			_, err := l.Enrich(context.Background(), RepoSummary{Name: "acme/shop", Inventory: []string{"endpoint shop GET /healthz"}}, items, nil)
			if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err %v, want %q", err, tt.wantErr)
			}
			got := reqs()
			if len(got) != len(tt.wantSchema) {
				t.Fatalf("%d requests, want %d", len(got), len(tt.wantSchema))
			}
			for i, req := range got {
				if (req.ResponseFormat != nil) != tt.wantSchema[i] || (req.ResponseFormat != nil && req.ResponseFormat["type"] != "json_schema") {
					t.Fatalf("request %d response_format %v", i, req.ResponseFormat)
				}
				if req.Model != "test-model-from-config" || req.MaxTokens != maxTokens || len(req.Messages) != 2 ||
					req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
					t.Fatalf("request %d %+v", i, req)
				}
				if sys := req.Messages[0].Content; !strings.Contains(sys, "acme/shop") || !strings.Contains(sys, `"suggested_monitors"`) {
					t.Fatalf("system prompt lacks repo summary or schema: %q", sys)
				}
				if !strings.Contains(req.Messages[1].Content, "POST /v1/payments/:id/capture") {
					t.Fatalf("user message %q", req.Messages[1].Content)
				}
			}
			if !tt.wantLLM {
				if items[0].LLM != nil || items[0].Criticality != model.Medium {
					t.Fatalf("item0 changed on failure: %+v", items[0])
				}
				return
			}
			if items[0].Criticality != model.Critical || items[0].LLM == nil || items[0].Summary != "Captures a payment." ||
				string(items[0].LLM.LoadtestHint.PayloadExample) != `{"amount":100}` || items[0].LLM.Model != "test-model-from-config" {
				t.Fatalf("item0 %+v %+v", items[0], items[0].LLM)
			}
			if items[1].LLM == nil || !items[1].LLM.LoadtestHint.Idempotent {
				t.Fatalf("item1 %+v", items[1])
			}
		})
	}
}

// After the endpoint rejected json_schema once, later batches do not send it again.
func TestLLMRemembersSchemaRejection(t *testing.T) {
	srv, reqs := chatServer(t, func(_ int, req chatReq) chatAnswer {
		if req.ResponseFormat != nil {
			return chatAnswer{status: http.StatusBadRequest}
		}
		return chatAnswer{content: okBody(0), prompt: 5, completion: 5}
	})
	l := newTestLLM(t, srv, "m")
	l.BatchSize = 1
	items := []*model.Item{{Kind: "endpoint", Key: "POST /a"}, {Kind: "endpoint", Key: "POST /b"}, {Kind: "endpoint", Key: "POST /c"}}
	if _, err := l.Enrich(context.Background(), RepoSummary{Name: "r"}, items, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(reqs()); n != 4 { // 400 + retry, then one request per remaining batch
		t.Fatalf("%d requests, want 4", n)
	}
}

func TestJSONObject(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"Sure! {\"a\":{\"b\":2}} hope it helps", `{"a":{"b":2}}`},
		{"no json", "no json"},
	} {
		if got := jsonObject(tc.in); got != tc.want {
			t.Errorf("jsonObject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
