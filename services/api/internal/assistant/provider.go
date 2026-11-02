// Package assistant is the dashboard's AI chat agent (ADR-033, .ai/assistant.md): a tool loop over the MCP tool
// surface (services/mcp/toolset) with an OpenAI-compatible chat completions model, or an offline mock that spends nothing.
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// ToolDef is a tool offered to the model.
type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any // JSON schema of the input (type object)
}

// ImageLoader returns an uploaded image.
type ImageLoader func(ctx context.Context, id string) (mediaType string, data []byte, err error)

// StepRequest is one model request.
type StepRequest struct {
	System    string
	Messages  []model.TMsg
	Tools     []ToolDef
	MaxTokens int64
	Image     ImageLoader
}

// StepResult is the model's reply: text and tool_use blocks.
type StepResult struct {
	Blocks     []model.TBlock
	StopReason string
	Usage      model.AssistantUsage
}

// Provider runs one model request, streaming text through onText.
type Provider interface {
	Name() string  // openai | mock
	Model() string // model id ("" for mock)
	Step(ctx context.Context, req StepRequest, onText func(string)) (StepResult, error)
}

// ---------------------------------------------------------------- offline mock

type mock struct{}

// NewMock returns the offline provider: no network, no cost. It calls one read tool for the newest user message
// (the mentioned item when there is one), renders a widget and explains how to enable a model.
func NewMock() Provider { return mock{} }

func (mock) Name() string  { return "mock" }
func (mock) Model() string { return "" }

func (mock) Step(_ context.Context, req StepRequest, onText func(string)) (StepResult, error) {
	last := req.Messages[len(req.Messages)-1]
	var results []model.TBlock
	for _, b := range last.Blocks {
		if b.Type == "tool_result" {
			results = append(results, b)
		}
	}
	say := func(s string) StepResult {
		onText(s)
		return StepResult{Blocks: []model.TBlock{{Type: "text", Text: s}}, StopReason: "end_turn"}
	}
	has := func(name string) bool {
		for _, t := range req.Tools {
			if t.Name == name {
				return true
			}
		}
		return false
	}
	if len(results) == 0 {
		text := ""
		for _, b := range last.Blocks {
			if b.Type == "text" {
				text += b.Text
			}
		}
		call, widget := mockPlan(text)
		if !has(call.Name) {
			return say("[Offline mock] I cannot reach the telemetry tools right now."), nil
		}
		intro := "[Offline mock] Looking this up with `" + call.Name + "`."
		onText(intro)
		in, _ := json.Marshal(call.Input)
		blocks := []model.TBlock{{Type: "text", Text: intro}, {Type: "tool_use", ID: "mock_1", Name: call.Name, Input: in}}
		if widget != nil && has(WidgetTool) {
			w, _ := json.Marshal(widget)
			blocks = append(blocks, model.TBlock{Type: "tool_use", ID: "mock_2", Name: WidgetTool, Input: w})
		}
		return StepResult{Blocks: blocks, StopReason: "tool_use"}, nil
	}
	r := results[0]
	if r.IsError {
		return say("\n\nThe tool returned an error: " + truncate(r.Content, 300)), nil
	}
	return say("\n\nThe tool answered with " + fmt.Sprint(len(r.Content)) + " bytes of data. " +
		"This is the offline mock: set `TT_ASSISTANT_API_KEY` and `TT_ASSISTANT_MODEL` on the api for real answers."), nil
}

type mockCall struct {
	Name  string
	Input map[string]any
}

// mockPlan picks a read tool from the message: a mention in the referenced_items preamble wins, then keywords.
func mockPlan(text string) (mockCall, map[string]any) {
	ref := func(kind string) (string, bool) {
		for _, line := range strings.Split(text, "\n") {
			if f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "- ")); len(f) >= 2 && f[0] == kind {
				return strings.TrimPrefix(f[1], "id="), true
			}
		}
		return "", false
	}
	if id, ok := ref("incident"); ok {
		return mockCall{"tt_get_incident", map[string]any{"id": id}}, map[string]any{"kind": "incident", "id": id}
	}
	if id, ok := ref("service"); ok {
		return mockCall{"tt_list_endpoints", map[string]any{"service": id}}, map[string]any{"kind": "service", "service": id}
	}
	if id, ok := ref("loadtest_run"); ok {
		return mockCall{"tt_get_loadtest_run", map[string]any{"id": id}}, map[string]any{"kind": "loadtest_run", "id": id}
	}
	l := strings.ToLower(text)
	switch {
	case strings.Contains(l, "incident") || strings.Contains(l, "alert"):
		return mockCall{"tt_list_incidents", map[string]any{}}, nil
	case strings.Contains(l, "load test") || strings.Contains(l, "loadtest") || strings.Contains(l, "yük"):
		return mockCall{"tt_list_loadtest_runs", map[string]any{}}, nil
	case strings.Contains(l, "log"):
		return mockCall{"tt_search_logs", map[string]any{"level": "error"}}, nil
	}
	return mockCall{"tt_list_services", map[string]any{}}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
