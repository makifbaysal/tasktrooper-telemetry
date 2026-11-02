package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/llmapi"
)

// openAI talks to any OpenAI-compatible Chat Completions endpoint (Meta Muse Spark, gateways, ...) with streaming
// and function calling.
type openAI struct {
	endpoint string
	key      string
	model    string
	hc       *http.Client
	retries  int
}

// NewOpenAI returns the OpenAI-compatible provider, or nil when the key or the model is empty (model ids are never
// hard-coded: TT_ASSISTANT_MODEL). baseURL is the API root (".../v1") or the full ".../chat/completions" URL; empty
// means llmapi.DefaultURL (the platform's single LLM endpoint, ADR-034). hc carries the GenAI client spans (ADR-027);
// nil = a client with a 5 minute timeout.
func NewOpenAI(baseURL, apiKey, modelID string, hc *http.Client) Provider {
	cfg := llmapi.New(baseURL, apiKey, modelID)
	if !cfg.Enabled() {
		return nil
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &openAI{endpoint: cfg.URL, key: cfg.Key, model: cfg.Model, hc: hc, retries: 2}
}

func (p *openAI) Name() string  { return "openai" }
func (p *openAI) Model() string { return p.model }

// ---- wire types (OpenAI Chat Completions)

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"` // string | []oaPart | nil
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaPart struct {
	Type     string      `json:"type"` // text | image_url
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

type oaImageURL struct {
	URL string `json:"url"`
}

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaRequest struct {
	Model         string      `json:"model"`
	Messages      []oaMessage `json:"messages"`
	Tools         []oaTool    `json:"tools,omitempty"`
	MaxTokens     int64       `json:"max_tokens,omitempty"`
	Stream        bool        `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content   string       `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func (p *openAI) Step(ctx context.Context, req StepRequest, onText func(string)) (StepResult, error) {
	msgs, err := toOpenAI(ctx, req.System, req.Messages, req.Image)
	if err != nil {
		return StepResult{}, err
	}
	body := oaRequest{Model: p.model, Messages: msgs, MaxTokens: req.MaxTokens, Stream: true}
	body.StreamOptions = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{true}
	for _, t := range req.Tools {
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name, ot.Function.Description, ot.Function.Parameters = t.Name, t.Description, objectSchema(t.Schema)
		body.Tools = append(body.Tools, ot)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return StepResult{}, err
	}
	res, err := p.post(ctx, data)
	if err != nil {
		return StepResult{}, err
	}
	defer res.Body.Close()
	return readStream(res.Body, onText)
}

// post sends the request, retrying 429 / 5xx / network errors before any byte of the stream is read.
func (p *openAI) post(ctx context.Context, data []byte) (*http.Response, error) {
	var last error
	for attempt := 0; attempt <= p.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		res, err := p.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = errors.New("model API unreachable")
			continue
		}
		if res.StatusCode == http.StatusOK {
			return res, nil
		}
		// upstream bodies may echo the prompt: never put them in a user-visible error
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		_ = res.Body.Close()
		last = fmt.Errorf("model API returned HTTP %d", res.StatusCode)
		if res.StatusCode != http.StatusTooManyRequests && res.StatusCode < 500 {
			break
		}
	}
	return nil, last
}

// readStream parses the SSE stream ("data: {chunk}" lines, "data: [DONE]") into a StepResult.
func readStream(r io.Reader, onText func(string)) (StepResult, error) {
	var (
		text   strings.Builder
		calls  = map[int]*oaToolCall{}
		out    StepResult
		finish string
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ch oaChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue // keep-alive comments or vendor extensions
		}
		if ch.Usage != nil {
			out.Usage = model.AssistantUsage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" {
				text.WriteString(c.Delta.Content)
				onText(c.Delta.Content)
			}
			for i, tc := range c.Delta.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				cur := calls[idx]
				if cur == nil {
					cur = &oaToolCall{}
					calls[idx] = cur
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				cur.Function.Name += tc.Function.Name
				cur.Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return StepResult{}, errors.New("model API stream broke off")
	}
	if s := text.String(); s != "" {
		out.Blocks = append(out.Blocks, model.TBlock{Type: "text", Text: s})
	}
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for n, i := range idxs {
		c := calls[i]
		if c.Function.Name == "" {
			continue
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", n+1)
		}
		in := json.RawMessage(strings.TrimSpace(c.Function.Arguments))
		if len(in) == 0 || !json.Valid(in) {
			in = json.RawMessage(`{}`)
		}
		out.Blocks = append(out.Blocks, model.TBlock{Type: "tool_use", ID: id, Name: c.Function.Name, Input: in})
	}
	switch finish {
	case "tool_calls", "function_call":
		out.StopReason = "tool_use"
	case "length":
		out.StopReason = "max_tokens"
	default:
		out.StopReason = "end_turn"
	}
	return out, nil
}

// objectSchema makes sure the parameters are a JSON schema object (some servers reject a missing "properties").
func objectSchema(s map[string]any) map[string]any {
	out := make(map[string]any, len(s)+2)
	for k, v := range s {
		out[k] = v
	}
	out["type"] = "object"
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	return out
}

// toOpenAI converts the transcript: tool results become "tool" messages right after the assistant's tool calls,
// images become data URLs.
func toOpenAI(ctx context.Context, system string, msgs []model.TMsg, img ImageLoader) ([]oaMessage, error) {
	out := []oaMessage{{Role: "system", Content: system}}
	for _, m := range msgs {
		if m.Role == "assistant" {
			am := oaMessage{Role: "assistant"}
			var text strings.Builder
			for _, b := range m.Blocks {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_use":
					in := string(b.Input)
					if strings.TrimSpace(in) == "" {
						in = "{}"
					}
					var tc oaToolCall
					tc.ID, tc.Type = b.ID, "function"
					tc.Function.Name, tc.Function.Arguments = b.Name, in
					am.ToolCalls = append(am.ToolCalls, tc)
				}
			}
			if s := text.String(); s != "" || len(am.ToolCalls) == 0 {
				if s == "" {
					s = "(empty)"
				}
				am.Content = s
			}
			out = append(out, am)
			continue
		}
		var parts []oaPart
		for _, b := range m.Blocks {
			switch b.Type {
			case "tool_result":
				content := b.Content
				if content == "" {
					content = "(empty)"
				}
				if b.IsError {
					content = "ERROR: " + content
				}
				out = append(out, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: content})
			case "text":
				if b.Text != "" {
					parts = append(parts, oaPart{Type: "text", Text: b.Text})
				}
			case "image":
				if img == nil {
					continue
				}
				mt, data, err := img(ctx, b.AttachmentID)
				if err != nil {
					parts = append(parts, oaPart{Type: "text", Text: "[image " + b.AttachmentID + " is no longer available]"})
					continue
				}
				parts = append(parts, oaPart{Type: "image_url",
					ImageURL: &oaImageURL{URL: "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)}})
			}
		}
		switch {
		case len(parts) == 1 && parts[0].Type == "text":
			out = append(out, oaMessage{Role: "user", Content: parts[0].Text})
		case len(parts) > 0:
			out = append(out, oaMessage{Role: "user", Content: parts})
		}
	}
	return out, nil
}
