// Package assistant is the dashboard's AI chat agent (ADR-033, .ai/assistant.md): a tool loop over the MCP tool
// surface (services/mcp/toolset) with Claude, or an offline mock that spends nothing.
package assistant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

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
	Name() string  // claude | mock
	Model() string // model id ("" for mock)
	Step(ctx context.Context, req StepRequest, onText func(string)) (StepResult, error)
}

// ---------------------------------------------------------------- Claude

type claude struct {
	client anthropic.Client
	model  string
}

// NewClaude returns the Claude provider, or nil when the key or the model is empty (model ids are never
// hard-coded: TT_ASSISTANT_MODEL). hc carries the GenAI client spans (ADR-027); nil = default client.
func NewClaude(apiKey, modelID string, hc *http.Client, opts ...option.RequestOption) Provider {
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(modelID) == "" {
		return nil
	}
	o := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(2)}
	if hc != nil {
		o = append(o, option.WithHTTPClient(hc))
	}
	return &claude{client: anthropic.NewClient(append(o, opts...)...), model: modelID}
}

func (c *claude) Name() string  { return "claude" }
func (c *claude) Model() string { return c.model }

func (c *claude) Step(ctx context.Context, req StepRequest, onText func(string)) (StepResult, error) {
	msgs, err := toParams(ctx, req.Messages, req.Image)
	if err != nil {
		return StepResult{}, err
	}
	tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
	for i, t := range req.Tools {
		schema := anthropic.ToolInputSchemaParam{ExtraFields: map[string]any{}}
		for k, v := range t.Schema {
			switch k {
			case "type":
			case "properties":
				schema.Properties = v
			case "required":
				if rs, ok := v.([]any); ok {
					for _, r := range rs {
						if s, ok := r.(string); ok {
							schema.Required = append(schema.Required, s)
						}
					}
				}
			default:
				schema.ExtraFields[k] = v
			}
		}
		tp := anthropic.ToolParam{Name: t.Name, InputSchema: schema, Description: anthropic.String(t.Description)}
		if i == len(req.Tools)-1 { // cache the system prompt + tool list prefix
			tp.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &tp})
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: req.MaxTokens,
		System:    []anthropic.TextBlockParam{{Text: req.System}},
		Messages:  msgs,
		Tools:     tools,
	}
	stream := c.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var acc anthropic.Message
	for stream.Next() {
		ev := stream.Current()
		if err := acc.Accumulate(ev); err != nil {
			return StepResult{}, err
		}
		if ev.Type == "content_block_delta" {
			if d := ev.AsContentBlockDelta().Delta; d.Type == "text_delta" && d.Text != "" {
				onText(d.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return StepResult{}, providerError(err)
	}
	out := StepResult{StopReason: string(acc.StopReason), Usage: model.AssistantUsage{
		InputTokens:  acc.Usage.InputTokens + acc.Usage.CacheCreationInputTokens + acc.Usage.CacheReadInputTokens,
		OutputTokens: acc.Usage.OutputTokens,
	}}
	for _, b := range acc.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out.Blocks = append(out.Blocks, model.TBlock{Type: "text", Text: b.Text})
			}
		case "tool_use":
			tu := b.AsToolUse()
			in := tu.Input
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			out.Blocks = append(out.Blocks, model.TBlock{Type: "tool_use", ID: tu.ID, Name: tu.Name, Input: in})
		}
	}
	return out, nil
}

// providerError keeps upstream bodies (which may echo the prompt) out of messages shown to users.
func providerError(err error) error {
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		return fmt.Errorf("model API returned HTTP %d", ae.StatusCode)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("model API unreachable")
}

func toParams(ctx context.Context, msgs []model.TMsg, img ImageLoader) ([]anthropic.MessageParam, error) {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		var blocks []anthropic.ContentBlockParamUnion
		for _, b := range m.Blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					blocks = append(blocks, anthropic.NewTextBlock(b.Text))
				}
			case "image":
				if img == nil {
					continue
				}
				mt, data, err := img(ctx, b.AttachmentID)
				if err != nil {
					blocks = append(blocks, anthropic.NewTextBlock("[image "+b.AttachmentID+" is no longer available]"))
					continue
				}
				blocks = append(blocks, anthropic.NewImageBlockBase64(mt, base64.StdEncoding.EncodeToString(data)))
			case "tool_use":
				var in any = map[string]any{}
				if len(b.Input) > 0 {
					_ = json.Unmarshal(b.Input, &in)
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(b.ID, in, b.Name))
			case "tool_result":
				content := b.Content
				if content == "" {
					content = "(empty)"
				}
				blocks = append(blocks, anthropic.NewToolResultBlock(b.ToolUseID, content, b.IsError))
			}
		}
		if len(blocks) == 0 {
			blocks = append(blocks, anthropic.NewTextBlock("(empty)"))
		}
		if m.Role == "assistant" {
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		} else {
			out = append(out, anthropic.NewUserMessage(blocks...))
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- offline mock

type mock struct{}

// NewMock returns the offline provider: no network, no cost. It calls one read tool for the newest user message
// (the mentioned item when there is one), renders a widget and explains how to enable Claude.
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
		"This is the offline mock: set `ANTHROPIC_API_KEY` and `TT_ASSISTANT_MODEL` on the api for real answers."), nil
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
