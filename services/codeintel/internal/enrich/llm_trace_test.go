package enrich

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/llm"
	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// The Anthropic client built by NewLLM goes through the trooper llm transport: one GenAI client span with provider,
// configured model and token usage, without prompt/completion content (capture off by default).
func TestLLMUsesTrooperTransport(t *testing.T) {
	cases := []struct {
		name                       string
		input, cacheRead, output   int64
		wantInputTokens, wantOuput int64
	}{
		{"plain", 120, 0, 40, 120, 40},
		{"cache read counts as input", 100, 900, 25, 1000, 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := troopertest.New(t, "tt-codeintel")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				var req map[string]any
				_ = json.Unmarshal(b, &req)
				out := `{"items":[{"id":0,"criticality":"high","business_domain":"orders","summary":"Creates an order.","failure_modes":[],"suggested_monitors":[],"loadtest_hint":{"payload_example":"","idempotent":false,"notes":""}}]}`
				w.Header().Set("content-type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": req["model"],
					"content": []any{map[string]any{"type": "text", "text": out}}, "stop_reason": "end_turn",
					"usage": map[string]any{"input_tokens": tc.input, "output_tokens": tc.output, "cache_read_input_tokens": tc.cacheRead}})
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			prev := llmTransportOptions
			llmTransportOptions = []llm.Option{llm.WithProvider(u.Host, "anthropic")}
			defer func() { llmTransportOptions = prev }()

			l := NewLLM("k", "model-from-config", option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
			items := []*model.Item{{Kind: "endpoint", Key: "POST /orders", Snippet: "secret prompt body", Criticality: model.Medium}}
			if _, err := l.Enrich(context.Background(), RepoSummary{Name: "acme/shop"}, items, nil); err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, s := range h.Spans() {
				if troopertest.AttrString(s, "gen_ai.provider.name") != "anthropic" {
					continue
				}
				found = true
				if got := troopertest.AttrString(s, "gen_ai.request.model"); got != "model-from-config" {
					t.Errorf("request model %q", got)
				}
				if v, _ := troopertest.Attr(s, attribute.Key("gen_ai.usage.input_tokens")); v.AsInt64() != tc.wantInputTokens {
					t.Errorf("input tokens %v, want %d", v.AsInt64(), tc.wantInputTokens)
				}
				if v, _ := troopertest.Attr(s, attribute.Key("gen_ai.usage.output_tokens")); v.AsInt64() != tc.wantOuput {
					t.Errorf("output tokens %v, want %d", v.AsInt64(), tc.wantOuput)
				}
				for _, k := range []attribute.Key{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions"} {
					if _, ok := troopertest.Attr(s, k); ok {
						t.Errorf("content attribute %s recorded with capture off", k)
					}
				}
			}
			if !found {
				t.Fatalf("no GenAI span among %d spans", len(h.Spans()))
			}
		})
	}
}
