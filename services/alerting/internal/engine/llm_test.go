package engine

import (
	"reflect"
	"testing"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
)

func TestLLMCause(t *testing.T) {
	cases := []struct {
		name   string
		detail map[string]any
		want   *rca.Result
	}{
		{"no summary", map[string]any{"metric": "calls"}, nil},
		{"cost", map[string]any{"metric": "cost_usd_per_hour", "summary": "LLM cost $34.20/h above $20/h — claude-sonnet-4-5 in storefront-bff 81%",
			"topShare": 0.81, "contributors": []map[string]any{{"service": "storefront-bff", "provider": "anthropic", "model": "claude-sonnet-4-5", "value": 27.7}}},
			&rca.Result{Unit: rca.UnitRatio, Confidence: 0.81, Summary: "LLM cost $34.20/h above $20/h — claude-sonnet-4-5 in storefront-bff 81%",
				Root: &rca.Node{ID: "llm:anthropic/claude-sonnet-4-5", Kind: "external", Label: "anthropic · claude-sonnet-4-5", Service: "storefront-bff", Current: 27.7}}},
		{"latency without contributors", map[string]any{"metric": "p95_ms", "summary": "LLM p95 24 s above 20 s"},
			&rca.Result{Unit: rca.UnitMs, Summary: "LLM p95 24 s above 20 s", Root: &rca.Node{ID: "llm", Kind: "external", Label: "LLM"}}},
	}
	for _, tc := range cases {
		got := llmCause(eval.Result{Detail: tc.detail})
		if (got == nil) != (tc.want == nil) {
			t.Fatalf("%s: got %+v", tc.name, got)
		}
		if got == nil {
			continue
		}
		if got.Unit != tc.want.Unit || got.Summary != tc.want.Summary || got.Confidence != tc.want.Confidence || !reflect.DeepEqual(got.Root, tc.want.Root) || got.Path == nil {
			t.Errorf("%s: got %+v root %+v", tc.name, got, got.Root)
		}
	}
}
