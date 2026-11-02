package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

func TestNewOpenAINeedsKeyAndModel(t *testing.T) {
	for _, tc := range []struct{ key, model string }{{"", "m"}, {"k", ""}, {" ", " "}} {
		if NewOpenAI("", tc.key, tc.model, nil) != nil {
			t.Errorf("key %q model %q: want nil provider", tc.key, tc.model)
		}
	}
	if p := NewOpenAI("", "k", "muse", nil); p == nil || p.Name() != "openai" || p.Model() != "muse" {
		t.Fatalf("provider = %#v", p)
	}
}

const streamBody = `data: {"choices":[{"delta":{"content":"Checking "}}]}

data: {"choices":[{"delta":{"content":"services."}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"tt_list_services","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"range\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"1h\"}"}},{"index":1,"function":{"name":"show_widget","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30}}

data: [DONE]
`

func TestOpenAIStep(t *testing.T) {
	var got oaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamBody)
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL+"/v1", "secret", "muse-spark", srv.Client())
	var streamed strings.Builder
	img := func(context.Context, string) (string, []byte, error) { return "image/png", []byte{1, 2, 3}, nil }
	res, err := p.Step(context.Background(), StepRequest{
		System: "sys",
		Messages: []model.TMsg{
			{Role: "user", Blocks: []model.TBlock{{Type: "text", Text: "hi"}, {Type: "image", AttachmentID: "a1"}}},
			{Role: "assistant", Blocks: []model.TBlock{{Type: "text", Text: "ok"}, {Type: "tool_use", ID: "t1", Name: "tt_get_incident", Input: json.RawMessage(`{"id":"5"}`)}}},
			{Role: "user", Blocks: []model.TBlock{{Type: "tool_result", ToolUseID: "t1", Content: "boom", IsError: true}, {Type: "text", Text: "and now?"}}},
		},
		Tools:     []ToolDef{{Name: "tt_list_services", Description: "d", Schema: map[string]any{"type": "object"}}},
		MaxTokens: 512,
		Image:     img,
	}, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}

	// request shape
	if got.Model != "muse-spark" || !got.Stream || got.MaxTokens != 512 || len(got.Tools) != 1 {
		t.Fatalf("request = %+v", got)
	}
	if props, ok := got.Tools[0].Function.Parameters["properties"]; !ok || props == nil {
		t.Errorf("tool schema without properties: %v", got.Tools[0].Function.Parameters)
	}
	roles := []string{}
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles = %v", roles)
	}
	if parts, ok := got.Messages[1].Content.([]any); !ok || len(parts) != 2 ||
		!strings.HasPrefix(parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,") {
		t.Errorf("user content = %#v", got.Messages[1].Content)
	}
	if tc := got.Messages[2].ToolCalls; len(tc) != 1 || tc[0].ID != "t1" || tc[0].Function.Arguments != `{"id":"5"}` {
		t.Errorf("assistant tool calls = %+v", tc)
	}
	if m := got.Messages[3]; m.ToolCallID != "t1" || m.Content != "ERROR: boom" {
		t.Errorf("tool message = %+v", m)
	}

	// response
	if streamed.String() != "Checking services." {
		t.Errorf("streamed %q", streamed.String())
	}
	if res.StopReason != "tool_use" || res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 30 {
		t.Errorf("stop %q usage %+v", res.StopReason, res.Usage)
	}
	if len(res.Blocks) != 3 || res.Blocks[0].Text != "Checking services." ||
		res.Blocks[1].ID != "call_a" || res.Blocks[1].Name != "tt_list_services" || string(res.Blocks[1].Input) != `{"range":"1h"}` ||
		res.Blocks[2].Name != WidgetTool || res.Blocks[2].ID == "" {
		t.Fatalf("blocks = %+v", res.Blocks)
	}
}

func TestOpenAIErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantHits int32
	}{
		{"bad request is not retried", http.StatusBadRequest, 1},
		{"rate limit is retried", http.StatusTooManyRequests, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"prompt was: secret question"}`)
			}))
			defer srv.Close()
			p := NewOpenAI(srv.URL, "k", "m", srv.Client()).(*openAI)
			p.retries = 1
			_, err := p.Step(context.Background(), StepRequest{Messages: []model.TMsg{{Role: "user", Blocks: []model.TBlock{{Type: "text", Text: "q"}}}}}, func(string) {})
			if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "HTTP") {
				t.Fatalf("err = %v", err)
			}
			if hits.Load() != tc.wantHits {
				t.Errorf("hits = %d, want %d", hits.Load(), tc.wantHits)
			}
		})
	}
}

func TestReadStreamFinishReasons(t *testing.T) {
	for _, tc := range []struct{ finish, want string }{{"stop", "end_turn"}, {"length", "max_tokens"}, {"tool_calls", "tool_use"}} {
		body := `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"` + tc.finish + `"}]}` + "\n\ndata: [DONE]\n"
		res, err := readStream(strings.NewReader(body), func(string) {})
		if err != nil || res.StopReason != tc.want {
			t.Errorf("%s: stop %q err %v", tc.finish, res.StopReason, err)
		}
	}
}
