package enrich

import (
	"context"
	"testing"

	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// The client built by NewLLM goes through the trooper llm transport with the configured endpoint host registered:
// one GenAI client span with provider (an unknown OpenAI-compatible host → "openai"), configured model and token
// usage, without prompt/completion content (capture off by default).
func TestLLMUsesTrooperTransport(t *testing.T) {
	cases := []struct {
		name                        string
		prompt, cached, completion  int64
		wantInputTokens, wantOutput int64
	}{
		{"plain", 120, 0, 40, 120, 40},
		{"cached prompt tokens count as input", 1000, 900, 25, 1000, 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := troopertest.New(t, "tt-codeintel")
			srv, _ := chatServer(t, func(int, chatReq) chatAnswer {
				return chatAnswer{content: `{"items":[{"id":0,"criticality":"high","business_domain":"orders","summary":"Creates an order.","failure_modes":[],"suggested_monitors":[],"loadtest_hint":{"payload_example":"","idempotent":false,"notes":""}}]}`,
					prompt: tc.prompt, cached: tc.cached, completion: tc.completion}
			})
			l := newTestLLM(t, srv, "model-from-config")
			items := []*model.Item{{Kind: "endpoint", Key: "POST /orders", Snippet: "secret prompt body", Criticality: model.Medium}}
			if _, err := l.Enrich(context.Background(), RepoSummary{Name: "acme/shop"}, items, nil); err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, s := range h.Spans() {
				if _, ok := troopertest.Attr(s, attribute.Key("gen_ai.request.model")); !ok {
					continue
				}
				found = true
				if got := troopertest.AttrString(s, "gen_ai.provider.name"); got != "openai" {
					t.Errorf("provider %q", got)
				}
				if got := troopertest.AttrString(s, "gen_ai.operation.name"); got != "chat" {
					t.Errorf("operation %q", got)
				}
				if got := troopertest.AttrString(s, "gen_ai.request.model"); got != "model-from-config" {
					t.Errorf("request model %q", got)
				}
				if v, _ := troopertest.Attr(s, attribute.Key("gen_ai.usage.input_tokens")); v.AsInt64() != tc.wantInputTokens {
					t.Errorf("input tokens %v, want %d", v.AsInt64(), tc.wantInputTokens)
				}
				if v, _ := troopertest.Attr(s, attribute.Key("gen_ai.usage.output_tokens")); v.AsInt64() != tc.wantOutput {
					t.Errorf("output tokens %v, want %d", v.AsInt64(), tc.wantOutput)
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
