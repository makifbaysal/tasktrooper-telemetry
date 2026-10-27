package store

import (
	"math"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/llmprice"
)

func TestFinishReason(t *testing.T) {
	for in, want := range map[string]string{
		`["stop"]`:          "stop",
		`["length","stop"]`: "length",
		"end_turn":          "end_turn",
		"stop,length":       "stop",
		`['tool_calls']`:    "tool_calls",
		"":                  "",
	} {
		if got := FinishReason(in); got != want {
			t.Errorf("FinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLLMFromAttrs(t *testing.T) {
	prices := llmprice.New(nil)
	tests := []struct {
		name     string
		attrs    map[string]string
		nilLLM   bool
		provider string
		model    string
		cost     float64 // -1 = unknown
		ttft     float64
	}{
		{"not llm", map[string]string{"http.method": "GET"}, true, "", "", 0, 0},
		{"openai priced", map[string]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai", "gen_ai.request.model": "gpt-4o",
			"gen_ai.response.model": "gpt-4o-2024-08-06", "gen_ai.usage.input_tokens": "1000000", "gen_ai.usage.output_tokens": "100000",
			"trooper.llm.ttft_ms": "120.5"}, false, "openai", "gpt-4o-2024-08-06", 2.5 + 1.0, 120.5},
		{"legacy system + cache", map[string]string{"gen_ai.operation.name": "chat", "gen_ai.system": "anthropic", "gen_ai.request.model": "claude-sonnet-4-5",
			"gen_ai.usage.input_tokens": "1000000", "gen_ai.usage.cache_read.input_tokens": "1000000"}, false, "anthropic", "claude-sonnet-4-5", 0.3, 0},
		{"unknown model", map[string]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "ollama", "gen_ai.request.model": "llama3"},
			false, "ollama", "llama3", -1, 0},
	}
	for _, tc := range tests {
		l := LLMFromAttrs(tc.attrs, prices)
		if (l == nil) != tc.nilLLM {
			t.Fatalf("%s: llm = %+v", tc.name, l)
		}
		if l == nil {
			continue
		}
		if l.Provider != tc.provider || l.Model != tc.model {
			t.Errorf("%s: %s/%s", tc.name, l.Provider, l.Model)
		}
		switch {
		case tc.cost < 0 && l.CostUsd != nil:
			t.Errorf("%s: cost %v, want unknown", tc.name, *l.CostUsd)
		case tc.cost >= 0 && (l.CostUsd == nil || math.Abs(*l.CostUsd-tc.cost) > 1e-9):
			t.Errorf("%s: cost %v, want %v", tc.name, l.CostUsd, tc.cost)
		}
		if (tc.ttft == 0) != (l.TtftMs == nil) || l.TtftMs != nil && *l.TtftMs != tc.ttft {
			t.Errorf("%s: ttft %v", tc.name, l.TtftMs)
		}
	}
}

func TestCostAcc(t *testing.T) {
	prices := llmprice.New(nil)
	var c costAcc
	if c.ptr() != nil {
		t.Error("empty acc has a cost")
	}
	c.add(prices, "ollama", "llama3", llmSums{calls: 3, in: 100})
	if c.ptr() != nil || !c.partial {
		t.Errorf("unpriced only: %+v", c)
	}
	c.add(prices, "openai", "gpt-4o", llmSums{calls: 1, in: 1e6})
	if c.ptr() == nil || *c.ptr() != 2.5 || !c.partial {
		t.Errorf("mixed: %+v", c)
	}
}

func TestProfileFilterValidate(t *testing.T) {
	now := time.Now()
	ok := ProfileFilter{Service: "api", Type: "cpu", From: now.Add(-time.Hour), To: now}
	tests := []struct {
		name    string
		f       ProfileFilter
		needTyp bool
		valid   bool
	}{
		{"ok", ok, true, true},
		{"no service", ProfileFilter{Type: "cpu", From: ok.From, To: ok.To}, true, false},
		{"bad type", ProfileFilter{Service: "api", Type: "heap", From: ok.From, To: ok.To}, true, false},
		{"types endpoint ignores type", ProfileFilter{Service: "api", From: ok.From, To: ok.To}, false, true},
		{"reversed", ProfileFilter{Service: "api", Type: "cpu", From: now, To: now.Add(-time.Hour)}, true, false},
		{"too long", ProfileFilter{Service: "api", Type: "cpu", From: now.Add(-30 * 24 * time.Hour), To: now}, true, false},
	}
	for _, tc := range tests {
		if err := tc.f.validate(tc.needTyp); (err == nil) != tc.valid {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

func TestIsLLMCallOp(t *testing.T) {
	cases := []struct {
		op   string
		want bool
	}{
		{"", false},
		{"execute_tool", false},
		{"invoke_agent", false},
		{"create_agent", false},
		{"chat", true},
		{"embeddings", true},
		{"generate_content", true},
		{"text_completion", true},
	}
	for _, c := range cases {
		if got := IsLLMCallOp(c.op); got != c.want {
			t.Errorf("IsLLMCallOp(%q) = %v, want %v", c.op, got, c.want)
		}
		if got := LLMFromAttrs(map[string]string{"gen_ai.operation.name": c.op}, nil) != nil; got != c.want {
			t.Errorf("LLMFromAttrs(op=%q) non-nil = %v, want %v", c.op, got, c.want)
		}
	}
}
