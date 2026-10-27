package flatten

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func named(s *tracepb.Span, name string) *tracepb.Span { s.Name = name; return s }

func TestLLMSpans(t *testing.T) {
	inMsgs := `[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`
	outMsgs := `[{"role":"assistant","parts":[{"type":"text","content":"hello"}],"finish_reason":"stop"}]`
	tests := []struct {
		name      string
		spans     []*tracepb.Span
		idx       int // span under test
		wantDep   string
		wantKind  string
		wantAttrs map[string]string // "" value = must be absent
		wantCont  *LLMContentRow    // nil = no content row for this span
	}{
		{
			name: "otel genai client span",
			spans: []*tracepb.Span{span(1, 0, client, attrs("gen_ai.operation.name", "chat", "gen_ai.provider.name", "openai",
				"gen_ai.request.model", "gpt-4o", "gen_ai.response.model", "gpt-4o-2024-08-06", "gen_ai.usage.input_tokens", 120,
				"gen_ai.usage.output_tokens", 30, "gen_ai.usage.cache_read.input_tokens", 100, "trooper.llm.ttft_ms", 210.5,
				"http.request.method", "POST", "server.address", "api.openai.com",
				"gen_ai.input.messages", inMsgs, "gen_ai.output.messages", outMsgs, "gen_ai.system_instructions", `[{"type":"text","content":"be nice"}]`))},
			wantDep: "openai · gpt-4o-2024-08-06", wantKind: "ai",
			wantAttrs: map[string]string{"gen_ai.usage.input_tokens": "120", "gen_ai.usage.output_tokens": "30", "gen_ai.usage.cache_read.input_tokens": "100",
				"gen_ai.provider.name": "openai", "gen_ai.operation.name": "chat", "trooper.llm.ttft_ms": "210.5",
				"gen_ai.input.messages": "", "gen_ai.output.messages": "", "gen_ai.system_instructions": ""},
			wantCont: &LLMContentRow{System: `[{"type":"text","content":"be nice"}]`, Input: inMsgs, Output: outMsgs},
		},
		{
			name: "internal span, legacy provider gen_ai.system, request model",
			spans: []*tracepb.Span{span(1, 0, internal, attrs("gen_ai.operation.name", "chat", "gen_ai.system", "Anthropic",
				"gen_ai.request.model", "claude-sonnet-4-5"))},
			wantDep: "anthropic · claude-sonnet-4-5", wantKind: "ai", wantAttrs: map[string]string{"gen_ai.provider.name": "anthropic"},
		},
		{
			name:     "embeddings",
			spans:    []*tracepb.Span{span(1, 0, client, attrs("gen_ai.operation.name", "embeddings", "gen_ai.provider.name", "openai", "gen_ai.request.model", "text-embedding-3-small"))},
			wantDep:  "openai · text-embedding-3-small",
			wantKind: "ai",
		},
		{
			name:      "execute_tool is not a dependency",
			spans:     []*tracepb.Span{span(1, 0, internal, attrs("gen_ai.operation.name", "execute_tool", "gen_ai.tool.name", "search"))},
			wantAttrs: map[string]string{"gen_ai.tool.name": "search", "gen_ai.operation.name": "execute_tool"},
		},
		{
			name: "http client under an LLM span is skipped",
			spans: []*tracepb.Span{
				span(1, 0, internal, attrs("gen_ai.operation.name", "chat", "gen_ai.provider.name", "openai", "gen_ai.request.model", "gpt-4o")),
				span(2, 1, client, attrs("http.request.method", "POST", "server.address", "api.openai.com")),
			},
			idx: 1,
		},
		{
			name: "http client under a tool span stays a dependency",
			spans: []*tracepb.Span{
				span(1, 0, internal, attrs("gen_ai.operation.name", "execute_tool")),
				span(2, 1, client, attrs("http.request.method", "GET", "server.address", "search.example.com")),
			},
			idx: 1, wantDep: "HTTP · search.example.com", wantKind: "http",
		},
		{
			name: "openllmetry legacy names",
			spans: []*tracepb.Span{span(1, 0, client, attrs("llm.request.type", "chat", "gen_ai.system", "OpenAI", "gen_ai.request.model", "gpt-4o-mini",
				"llm.usage.prompt_tokens", 11, "llm.usage.completion_tokens", 7, "llm.usage.total_tokens", 18,
				"gen_ai.prompt.0.role", "system", "gen_ai.prompt.0.content", "be brief",
				"gen_ai.prompt.1.role", "user", "gen_ai.prompt.1.content", "2+2?",
				"gen_ai.completion.0.role", "assistant", "gen_ai.completion.0.content", "4", "gen_ai.completion.0.finish_reason", "stop",
				"gen_ai.completion.0.tool_calls.0.name", "calc", "gen_ai.completion.0.tool_calls.0.arguments", `{"x":2}`))},
			wantDep: "openai · gpt-4o-mini", wantKind: "ai",
			wantAttrs: map[string]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai", "gen_ai.usage.input_tokens": "11",
				"gen_ai.usage.output_tokens": "7", "llm.usage.prompt_tokens": "", "llm.usage.total_tokens": "", "gen_ai.prompt.0.content": ""},
			wantCont: &LLMContentRow{
				Input:  `[{"role":"system","parts":[{"type":"text","content":"be brief"}]},{"role":"user","parts":[{"type":"text","content":"2+2?"}]}]`,
				Output: `[{"role":"assistant","parts":[{"type":"text","content":"4"},{"type":"tool_call","name":"calc","arguments":{"x":2}}],"finish_reason":"stop"}]`,
			},
		},
		{
			name: "vercel ai sdk doGenerate",
			spans: []*tracepb.Span{named(span(1, 0, client, attrs("ai.operationId", "ai.generateText.doGenerate", "gen_ai.system", "anthropic.messages",
				"ai.model.id", "claude-haiku-4-5", "ai.model.provider", "anthropic.messages", "ai.usage.promptTokens", 50, "ai.usage.completionTokens", 9,
				"ai.response.msToFirstChunk", 321.0, "ai.prompt.messages", `[{"role":"user","content":"hey"}]`, "ai.response.text", "yo")), "ai.generateText.doGenerate")},
			wantDep: "anthropic · claude-haiku-4-5", wantKind: "ai",
			wantAttrs: map[string]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "anthropic", "gen_ai.request.model": "claude-haiku-4-5",
				"gen_ai.usage.input_tokens": "50", "gen_ai.usage.output_tokens": "9", "trooper.llm.ttft_ms": "321",
				"ai.usage.promptTokens": "", "ai.prompt.messages": "", "ai.response.text": ""},
			wantCont: &LLMContentRow{Input: `[{"role":"user","content":"hey"}]`, Output: `[{"role":"assistant","parts":[{"type":"text","content":"yo"}]}]`},
		},
		{
			name: "vercel google provider via span name",
			spans: []*tracepb.Span{named(span(1, 0, internal, attrs("gen_ai.system", "google.generative-ai", "gen_ai.request.model", "gemini-2.5-flash")),
				"ai.streamText.doStream")},
			wantDep: "gcp.gemini · gemini-2.5-flash", wantKind: "ai", wantAttrs: map[string]string{"gen_ai.operation.name": "chat"},
		},
		{
			name: "vercel outer wrapper is not an LLM span",
			spans: []*tracepb.Span{named(span(1, 0, internal, attrs("ai.operationId", "ai.generateText", "gen_ai.system", "openai.chat",
				"ai.model.id", "gpt-4o", "ai.prompt", `{"prompt":"x"}`)), "ai.generateText")},
			wantAttrs: map[string]string{"gen_ai.operation.name": "", "ai.prompt": ""},
			wantCont:  &LLMContentRow{Input: `{"prompt":"x"}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, content := SpansWithContent(tn, traces("chat-api", tc.spans...), nil, nil)
			r := rows[tc.idx]
			if r.Dep != tc.wantDep || r.DepKind != tc.wantKind {
				t.Errorf("dep = %q/%q, want %q/%q", r.Dep, r.DepKind, tc.wantDep, tc.wantKind)
			}
			for k, v := range tc.wantAttrs {
				got, ok := r.Attrs[k]
				if v == "" && ok {
					t.Errorf("attr %s must be absent, got %q", k, got)
				} else if v != "" && got != v {
					t.Errorf("attr %s = %q, want %q", k, got, v)
				}
			}
			var c *LLMContentRow
			for i := range content {
				if string(content[i].SpanID) == string(r.SpanID) {
					c = &content[i]
				}
			}
			if (c == nil) != (tc.wantCont == nil) {
				t.Fatalf("content row = %+v, want %+v", c, tc.wantCont)
			}
			if c != nil {
				if c.System != tc.wantCont.System || c.Input != tc.wantCont.Input || c.Output != tc.wantCont.Output {
					t.Errorf("content = %+v\nwant %+v", *c, *tc.wantCont)
				}
				if c.OrgID != 1 || c.ProjectID != 2 || c.Service != "chat-api" || len(c.TraceID) != 16 || c.TS.IsZero() {
					t.Errorf("content identity: %+v", *c)
				}
				for _, s := range []string{c.Input, c.Output} {
					if s != "" && strings.HasPrefix(s, "[") && !json.Valid([]byte(s)) {
						t.Errorf("content is not valid JSON: %s", s)
					}
				}
			}
		})
	}
}

func TestLLMContentTruncationAndCaps(t *testing.T) {
	kvs := attrs("gen_ai.operation.name", "chat", "gen_ai.provider.name", "openai", "gen_ai.request.model", "gpt-4o",
		"gen_ai.input.messages", strings.Repeat("é", 40<<10))
	for i := range 100 {
		kvs = append(kvs, kv(fmt.Sprintf("custom.%03d", i), "v"))
	}
	kvs = append(kvs, kv("gen_ai.usage.input_tokens", 5), kv("gen_ai.usage.output_tokens", 6))
	rows, content := SpansWithContent(tn, traces("svc", span(1, 0, client, kvs)), nil, nil)
	r := rows[0]
	if len(r.Attrs) > MaxAttrKeys {
		t.Fatalf("attrs = %d keys", len(r.Attrs))
	}
	if r.Attrs["gen_ai.usage.input_tokens"] != "5" || r.Attrs["gen_ai.usage.output_tokens"] != "6" {
		t.Fatalf("normalized tokens must survive the cap: %v", r.Attrs)
	}
	in := content[0].Input
	if len(in) > MaxLLMContentLen || !strings.HasSuffix(in, "…[truncated]") || !strings.HasPrefix(in, "é") {
		t.Fatalf("input len %d suffix %q", len(in), in[len(in)-20:])
	}
	if strings.Contains(in[:len(in)-len("…[truncated]")], "�") {
		t.Fatal("truncation split a rune")
	}
}

func TestNonLLMSpansUnchanged(t *testing.T) {
	rows, content := SpansWithContent(tn, traces("svc", span(1, 0, client, attrs("db.system", "postgresql", "db.namespace", "orders"))), nil, nil)
	if rows[0].Dep != "postgresql · orders" || rows[0].DepKind != "db" || len(content) != 0 {
		t.Fatalf("%+v %v", rows[0], content)
	}
}

func TestLLMDetailsLogEvent(t *testing.T) {
	ts := uint64(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixNano())
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: traces("svc").ResourceSpans[0].Resource,
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{TimeUnixNano: ts, EventName: LLMDetailsEvent, TraceId: traceID, SpanId: sid(5),
				Attributes: attrs("gen_ai.input.messages", `[{"role":"user"}]`, "gen_ai.output.messages", `[{"role":"assistant"}]`, "gen_ai.provider.name", "openai")},
			{TimeUnixNano: ts, TraceId: traceID, SpanId: sid(6), Attributes: attrs("event.name", LLMDetailsEvent),
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: attrs("gen_ai.system_instructions", "sys")}}}},
			{TimeUnixNano: ts, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "plain"}}, Attributes: attrs("gen_ai.input.messages", "kept")},
		}}},
	}}}
	logs, content := LogsWithContent(tn, req, time.Now(), nil, nil)
	if len(logs) != 3 || len(content) != 2 {
		t.Fatalf("logs %d content %d", len(logs), len(content))
	}
	if content[0].Input != `[{"role":"user"}]` || content[0].Output != `[{"role":"assistant"}]` || string(content[0].SpanID) != string(sid(5)) {
		t.Fatalf("content[0] %+v", content[0])
	}
	if _, ok := logs[0].Attrs["gen_ai.input.messages"]; ok || logs[0].Attrs["gen_ai.provider.name"] != "openai" {
		t.Fatalf("log attrs %v", logs[0].Attrs)
	}
	if content[1].System != "sys" || logs[1].Body != LLMDetailsEvent {
		t.Fatalf("kvlist body: %+v / %q", content[1], logs[1].Body)
	}
	if logs[2].Attrs["gen_ai.input.messages"] != "kept" {
		t.Fatal("other logs are untouched")
	}
}

func BenchmarkFlattenLLMSpans(b *testing.B) {
	var spans []*tracepb.Span
	for i := range 50 {
		spans = append(spans, span(byte(2*i+1), 0, client, attrs("gen_ai.operation.name", "chat", "gen_ai.provider.name", "openai",
			"gen_ai.request.model", "gpt-4o", "gen_ai.usage.input_tokens", 1000, "gen_ai.usage.output_tokens", 200,
			"gen_ai.input.messages", strings.Repeat("x", 2000), "gen_ai.output.messages", strings.Repeat("y", 500))))
		spans = append(spans, span(byte(2*i+2), byte(2*i+1), client, attrs("http.request.method", "POST", "server.address", "api.openai.com")))
	}
	req := traces("svc", spans...)
	b.ReportAllocs()
	var rows []SpanRow
	var content []LLMContentRow
	for b.Loop() {
		rows, content = SpansWithContent(tn, req, rows[:0], content[:0])
	}
}
