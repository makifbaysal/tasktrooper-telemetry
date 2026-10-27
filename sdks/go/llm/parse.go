package llm

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry-go/internal/sdkerr"
)

func httpStatusAttr(code int) attribute.KeyValue {
	return attribute.Int("http.response.status_code", code)
}

// ---------------------------------------------------------------------------------------------
// Request

type reqInfo struct {
	model       string
	maxTokens   int
	temperature float64
	topP        float64
	system      []Part
	input       []Message
}

type wireRequest struct {
	Model               string   `json:"model"`
	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	MaxOutputTokens     *int     `json:"max_output_tokens"`
	Temperature         *float64 `json:"temperature"`
	TopP                *float64 `json:"top_p"`
	GenerationConfig    *struct {
		MaxOutputTokens *int     `json:"maxOutputTokens"`
		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"topP"`
	} `json:"generationConfig"`
}

type wireRequestContent struct {
	Messages           json.RawMessage `json:"messages"`
	System             json.RawMessage `json:"system"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	Prompt             json.RawMessage `json:"prompt"`
	Contents           json.RawMessage `json:"contents"`
	SystemInstruction  json.RawMessage `json:"systemInstruction"`
	SystemInstruction2 json.RawMessage `json:"system_instruction"`
}

func parseRequest(ep endpoint, body []byte, capture bool) (info reqInfo) {
	info.model = ep.model
	if len(body) == 0 {
		return info
	}
	defer func() {
		if recover() != nil {
			sdkerr.Count("llm")
		}
	}()
	var rq wireRequest
	if json.Unmarshal(body, &rq) != nil {
		return info
	}
	if rq.Model != "" {
		info.model = rq.Model
	}
	info.model = strings.TrimPrefix(info.model, "models/")
	for _, p := range []*int{rq.MaxTokens, rq.MaxCompletionTokens, rq.MaxOutputTokens} {
		if p != nil && *p > 0 {
			info.maxTokens = *p
			break
		}
	}
	if rq.Temperature != nil {
		info.temperature = *rq.Temperature
	}
	if rq.TopP != nil {
		info.topP = *rq.TopP
	}
	if g := rq.GenerationConfig; g != nil {
		if g.MaxOutputTokens != nil && info.maxTokens == 0 {
			info.maxTokens = *g.MaxOutputTokens
		}
		if g.Temperature != nil {
			info.temperature = *g.Temperature
		}
		if g.TopP != nil {
			info.topP = *g.TopP
		}
	}
	if !capture || ep.operation == OperationEmbeddings {
		return info
	}
	var c wireRequestContent
	if json.Unmarshal(body, &c) != nil {
		return info
	}
	switch ep.provider.dialect {
	case dialectAnthropic:
		info.system = blocksToParts(c.System)
		info.input = anthropicMessages(c.Messages)
	case dialectGemini:
		si := c.SystemInstruction
		if len(si) == 0 {
			si = c.SystemInstruction2
		}
		if len(si) > 0 {
			var content geminiContent
			if json.Unmarshal(si, &content) == nil {
				info.system = geminiParts(content.Parts)
			} else {
				info.system = blocksToParts(si)
			}
		}
		info.input = geminiMessages(c.Contents)
	default:
		if c.Instructions != "" {
			info.system = []Part{{Type: "text", Content: c.Instructions}}
		}
		switch {
		case len(c.Messages) > 0:
			info.input = openAIMessages(c.Messages)
		case len(c.Input) > 0: // Responses API
			info.input = openAIInput(c.Input)
		case len(c.Prompt) > 0: // legacy completions
			info.input = openAIInput(c.Prompt)
		}
	}
	return info
}

// blocksToParts converts a string or an array of {type, text} blocks.
func blocksToParts(raw json.RawMessage) []Part {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []Part{{Type: "text", Content: s}}
	}
	var blocks []wireBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	parts := make([]Part, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.part())
	}
	return parts
}

// wireBlock is a content block / part in OpenAI or Anthropic format.
type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	CallID    string          `json:"call_id"`
	Content   json.RawMessage `json:"content"`
	Output    json.RawMessage `json:"output"`
}

func (b wireBlock) part() Part {
	switch b.Type {
	case "text", "input_text", "output_text":
		return Part{Type: "text", Content: b.Text}
	case "thinking":
		return Part{Type: "reasoning", Content: b.Thinking}
	case "tool_use", "server_tool_use", "function_call":
		return Part{Type: "tool_call", ID: first(b.ID, b.CallID), Name: b.Name}
	case "tool_result", "function_call_output":
		raw := b.Content
		if len(raw) == 0 {
			raw = b.Output
		}
		return Part{Type: "tool_call_response", ID: first(b.ToolUseID, b.CallID), Content: textOf(raw)}
	case "image", "image_url", "input_image":
		return Part{Type: "image"}
	case "":
		if b.Text != "" {
			return Part{Type: "text", Content: b.Text}
		}
	}
	return Part{Type: b.Type}
}

// textOf flattens a string or a block array to text.
func textOf(raw json.RawMessage) string {
	var sb strings.Builder
	for _, p := range blocksToParts(raw) {
		if p.Content != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Content)
		}
	}
	return sb.String()
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type wireToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type wireChatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        []wireToolCall  `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
	Refusal          string          `json:"refusal"`
	Type             string          `json:"type"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Output           json.RawMessage `json:"output"`
}

func openAIMessages(raw json.RawMessage) []Message {
	var msgs []wireChatMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.message())
	}
	return out
}

func (m wireChatMessage) message() Message {
	msg := Message{Role: m.Role}
	switch m.Type {
	case "function_call": // Responses API input item
		return Message{Role: "assistant", Parts: []Part{{Type: "tool_call", ID: m.CallID, Name: m.Name}}}
	case "function_call_output":
		return Message{Role: "tool", Parts: []Part{{Type: "tool_call_response", ID: m.CallID, Content: textOf(m.Output)}}}
	}
	if m.Role == "tool" {
		msg.Parts = append(msg.Parts, Part{Type: "tool_call_response", ID: m.ToolCallID, Content: textOf(m.Content)})
	} else {
		msg.Parts = append(msg.Parts, blocksToParts(m.Content)...)
	}
	for _, tc := range m.ToolCalls {
		msg.Parts = append(msg.Parts, Part{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name})
	}
	if msg.Parts == nil {
		msg.Parts = []Part{}
	}
	return msg
}

// openAIInput converts Responses API input / completions prompt (string, string array or items).
func openAIInput(raw json.RawMessage) []Message {
	raw = bytes.TrimSpace(raw)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []Message{Text("user", s)}
	}
	var ss []string
	if json.Unmarshal(raw, &ss) == nil {
		out := make([]Message, 0, len(ss))
		for _, s := range ss {
			out = append(out, Text("user", s))
		}
		return out
	}
	return openAIMessages(raw)
}

func anthropicMessages(raw json.RawMessage) []Message {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		parts := blocksToParts(m.Content)
		if parts == nil {
			parts = []Part{}
		}
		out = append(out, Message{Role: m.Role, Parts: parts})
	}
	return out
}

type geminiPart struct {
	Text         string `json:"text"`
	Thought      bool   `json:"thought"`
	FunctionCall *struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"functionCall"`
	FunctionResponse *struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"functionResponse"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
	} `json:"inlineData"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

func geminiParts(ps []geminiPart) []Part {
	out := make([]Part, 0, len(ps))
	for _, p := range ps {
		switch {
		case p.FunctionCall != nil:
			out = append(out, Part{Type: "tool_call", ID: p.FunctionCall.ID, Name: p.FunctionCall.Name})
		case p.FunctionResponse != nil:
			out = append(out, Part{Type: "tool_call_response", ID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name})
		case p.InlineData != nil:
			out = append(out, Part{Type: "blob"})
		case p.Thought:
			out = append(out, Part{Type: "reasoning", Content: p.Text})
		default:
			out = append(out, Part{Type: "text", Content: p.Text})
		}
	}
	return out
}

func geminiRole(r string) string {
	if r == "model" {
		return "assistant"
	}
	if r == "" {
		return "user"
	}
	return r
}

func geminiMessages(raw json.RawMessage) []Message {
	var cs []geminiContent
	if json.Unmarshal(raw, &cs) != nil {
		var c geminiContent // a single Content object
		if json.Unmarshal(raw, &c) != nil {
			return nil
		}
		cs = []geminiContent{c}
	}
	out := make([]Message, 0, len(cs))
	for _, c := range cs {
		out = append(out, Message{Role: geminiRole(c.Role), Parts: geminiParts(c.Parts)})
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// Response

type wireUsage struct {
	// OpenAI chat / completions / embeddings
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"` // DeepSeek
	// OpenAI Responses + Anthropic
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// Anthropic
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

type wireGeminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
}

type wireChoice struct {
	Index        int              `json:"index"`
	Message      *wireChatMessage `json:"message"`
	Delta        *wireChatMessage `json:"delta"`
	Text         string           `json:"text"`
	FinishReason *string          `json:"finish_reason"`
}

type wireCandidate struct {
	Index        int           `json:"index"`
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type wireDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// wire is the union of the response documents and stream events of all dialects.
type wire struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Type    string       `json:"type"`
	Model   string       `json:"model"`
	Status  string       `json:"status"`
	Usage   *wireUsage   `json:"usage"`
	Choices []wireChoice `json:"choices"`
	// Anthropic
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stop_reason"`
	Message      *wire           `json:"message"`
	ContentBlock *wireBlock      `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Error        json.RawMessage `json:"error"`
	// OpenAI Responses API
	Output            []wireOutput `json:"output"`
	Response          *wire        `json:"response"`
	Item              *wireOutput  `json:"item"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	// Gemini
	Candidates    []wireCandidate  `json:"candidates"`
	UsageMetadata *wireGeminiUsage `json:"usageMetadata"`
	ModelVersion  string           `json:"modelVersion"`
	ResponseID    string           `json:"responseId"`
}

type wireOutput struct {
	Type    string      `json:"type"`
	Role    string      `json:"role"`
	ID      string      `json:"id"`
	CallID  string      `json:"call_id"`
	Name    string      `json:"name"`
	Content []wireBlock `json:"content"`
}

// maxCapturedText bounds the completion text kept for content capture.
const maxCapturedText = 64 << 10

// accumulator collects usage (and, with capture, the completion) from a response document or a
// stream of events.
type accumulator struct {
	dialect dialect
	call    *Call
	capture bool

	model, id string
	finish    []string
	errorType string

	in, out, cacheRead, cacheWrite int64
	// Anthropic reports input tokens without cache tokens; summed in usage().
	aIn, aCacheRead, aCacheWrite int64

	text  strings.Builder
	parts []Part // tool calls / reasoning (capture only)
}

func newAccumulator(d dialect, call *Call, capture bool) *accumulator {
	return &accumulator{dialect: d, call: call, capture: capture}
}

func (a *accumulator) firstToken() { a.call.FirstToken() }

func (a *accumulator) addText(s string) {
	if a.capture && s != "" && a.text.Len() < maxCapturedText {
		if room := maxCapturedText - a.text.Len(); len(s) > room {
			s = s[:room]
		}
		a.text.WriteString(s)
	}
}

func (a *accumulator) addPart(p Part) {
	if a.capture && len(a.parts) < 64 {
		a.parts = append(a.parts, p)
	}
}

func (a *accumulator) addFinish(r string) {
	if r != "" && !slices.Contains(a.finish, r) {
		a.finish = append(a.finish, r)
	}
}

func (a *accumulator) usageOpenAI(u *wireUsage) {
	if u == nil {
		return
	}
	switch {
	case u.PromptTokens != nil || u.CompletionTokens != nil:
		a.in, a.out = deref(u.PromptTokens), deref(u.CompletionTokens)
		if u.PromptTokensDetails != nil {
			a.cacheRead = u.PromptTokensDetails.CachedTokens
		}
		if u.PromptCacheHitTokens > 0 {
			a.cacheRead = u.PromptCacheHitTokens
		}
	case u.InputTokens != nil || u.OutputTokens != nil: // Responses API
		a.in, a.out = deref(u.InputTokens), deref(u.OutputTokens)
		if u.InputTokensDetails != nil {
			a.cacheRead = u.InputTokensDetails.CachedTokens
		}
	}
}

// usageAnthropic applies a usage object; values in later events (message_delta) are cumulative.
func (a *accumulator) usageAnthropic(u *wireUsage) {
	if u == nil {
		return
	}
	if u.InputTokens != nil && *u.InputTokens > 0 {
		a.aIn = *u.InputTokens
	}
	if u.OutputTokens != nil {
		a.out = *u.OutputTokens
	}
	if u.CacheReadInputTokens != nil && *u.CacheReadInputTokens > 0 {
		a.aCacheRead = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil && *u.CacheCreationInputTokens > 0 {
		a.aCacheWrite = *u.CacheCreationInputTokens
	}
}

func (a *accumulator) usageGemini(u *wireGeminiUsage) {
	if u == nil {
		return
	}
	a.in = u.PromptTokenCount + u.ToolUsePromptTokenCount
	a.out = u.CandidatesTokenCount + u.ThoughtsTokenCount
	a.cacheRead = u.CachedContentTokenCount
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (a *accumulator) setMeta(model, id string) {
	if model != "" {
		a.model = model
	}
	if id != "" && a.id == "" {
		a.id = id
	}
}

// document parses a complete (non-streaming) response body.
func (a *accumulator) document(body []byte) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return
	}
	if body[0] == '[' { // Gemini streamGenerateContent without alt=sse: JSON array of chunks
		var chunks []wire
		if json.Unmarshal(body, &chunks) != nil {
			return
		}
		for i := range chunks {
			a.gemini(&chunks[i], false)
		}
		return
	}
	var w wire
	if json.Unmarshal(body, &w) != nil {
		return
	}
	switch a.dialect {
	case dialectAnthropic:
		a.anthropicMessage(&w)
	case dialectGemini:
		a.gemini(&w, false)
	default:
		if w.Object == "response" || w.Output != nil {
			a.responsesDoc(&w)
			return
		}
		a.setMeta(w.Model, w.ID)
		for _, c := range w.Choices {
			if c.FinishReason != nil {
				a.addFinish(*c.FinishReason)
			}
			if c.Index != 0 {
				continue
			}
			a.addText(c.Text)
			if m := c.Message; m != nil {
				if a.capture {
					for _, p := range blocksToParts(m.Content) {
						if p.Type == "text" {
							a.addText(p.Content)
						} else {
							a.addPart(p)
						}
					}
					for _, tc := range m.ToolCalls {
						a.addPart(Part{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name})
					}
				}
			}
		}
		a.usageOpenAI(w.Usage)
	}
}

func (a *accumulator) anthropicMessage(w *wire) {
	a.setMeta(w.Model, w.ID)
	a.addFinish(w.StopReason)
	a.usageAnthropic(w.Usage)
	if a.capture {
		for _, p := range blocksToParts(w.Content) {
			if p.Type == "text" {
				a.addText(p.Content)
			} else {
				a.addPart(p)
			}
		}
	}
}

func (a *accumulator) responsesDoc(w *wire) {
	a.setMeta(w.Model, w.ID)
	a.usageOpenAI(w.Usage)
	switch {
	case w.IncompleteDetails != nil && w.IncompleteDetails.Reason != "":
		a.addFinish(w.IncompleteDetails.Reason)
	case w.Status != "":
		a.addFinish(w.Status)
	}
	if !a.capture {
		return
	}
	for _, o := range w.Output {
		switch o.Type {
		case "message":
			for _, b := range o.Content {
				if p := b.part(); p.Type == "text" {
					a.addText(p.Content)
				} else {
					a.addPart(p)
				}
			}
		case "function_call":
			a.addPart(Part{Type: "tool_call", ID: o.CallID, Name: o.Name})
		}
	}
}

func (a *accumulator) gemini(w *wire, stream bool) {
	a.setMeta(w.ModelVersion, w.ResponseID)
	a.usageGemini(w.UsageMetadata)
	for _, c := range w.Candidates {
		a.addFinish(c.FinishReason)
		if c.Index != 0 {
			continue
		}
		for _, p := range geminiParts(c.Content.Parts) {
			if stream && (p.Content != "" || p.Type == "tool_call") {
				a.firstToken()
			}
			if p.Type == "text" {
				a.addText(p.Content)
			} else {
				a.addPart(p)
			}
		}
	}
}

// event handles one SSE event.
func (a *accumulator) event(name string, data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return // e.g. [DONE]
	}
	var w wire
	if json.Unmarshal(data, &w) != nil {
		return
	}
	if w.Type == "" {
		w.Type = name
	}
	switch a.dialect {
	case dialectAnthropic:
		a.anthropicEvent(&w)
	case dialectGemini:
		a.gemini(&w, true)
	default:
		if strings.HasPrefix(w.Type, "response.") {
			a.responsesEvent(&w)
			return
		}
		if w.Type == "error" || len(w.Error) > 0 && string(w.Error) != "null" {
			a.errorType = errorTypeOf(w.Error, "error")
			return
		}
		a.setMeta(w.Model, w.ID)
		for _, c := range w.Choices {
			if c.FinishReason != nil {
				a.addFinish(*c.FinishReason)
			}
			d := c.Delta
			if d == nil {
				if c.Text != "" { // legacy completions stream
					a.firstToken()
					if c.Index == 0 {
						a.addText(c.Text)
					}
				}
				continue
			}
			content := textOf(d.Content)
			if content != "" || len(d.ToolCalls) > 0 || d.ReasoningContent != "" || d.Refusal != "" {
				a.firstToken()
			}
			if c.Index != 0 {
				continue
			}
			a.addText(content)
			for _, tc := range d.ToolCalls {
				if tc.Function.Name != "" {
					a.addPart(Part{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name})
				}
			}
		}
		a.usageOpenAI(w.Usage)
	}
}

func (a *accumulator) anthropicEvent(w *wire) {
	switch w.Type {
	case "message_start":
		if w.Message != nil {
			a.setMeta(w.Message.Model, w.Message.ID)
			a.usageAnthropic(w.Message.Usage)
		}
	case "content_block_start":
		if b := w.ContentBlock; b != nil && b.Type != "text" && b.Type != "thinking" {
			a.firstToken()
			a.addPart(b.part())
		}
	case "content_block_delta":
		var d wireDelta
		if json.Unmarshal(w.Delta, &d) != nil {
			return
		}
		if d.Text != "" || d.Thinking != "" || d.PartialJSON != "" {
			a.firstToken()
		}
		a.addText(d.Text)
	case "message_delta":
		var d wireDelta
		if json.Unmarshal(w.Delta, &d) == nil {
			a.addFinish(d.StopReason)
		}
		a.usageAnthropic(w.Usage)
	case "error":
		a.errorType = errorTypeOf(w.Error, "error")
	}
}

func (a *accumulator) responsesEvent(w *wire) {
	switch w.Type {
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta":
		a.firstToken()
		if w.Type == "response.output_text.delta" {
			var s string
			if json.Unmarshal(w.Delta, &s) == nil {
				a.addText(s)
			}
		}
	case "response.created", "response.in_progress":
		if w.Response != nil {
			a.setMeta(w.Response.Model, w.Response.ID)
		}
	case "response.completed", "response.incomplete", "response.failed":
		if w.Response != nil {
			text := a.text.String()
			a.text.Reset()
			a.parts = nil
			a.responsesDoc(w.Response)
			if a.text.Len() == 0 {
				a.addText(text)
			}
			if w.Type == "response.failed" {
				a.errorType = errorTypeOf(w.Response.Error, "response.failed")
			}
		}
	case "error":
		a.errorType = errorTypeOf(w.Error, "error")
	}
}

func errorTypeOf(raw json.RawMessage, def string) string {
	var e struct {
		Type string `json:"type"`
		Code any    `json:"code"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Type != "" {
			return e.Type
		}
		if s, ok := e.Code.(string); ok && s != "" {
			return s
		}
	}
	return def
}

// usage returns the collected usage for Call.End.
func (a *accumulator) usage() Usage {
	u := Usage{
		InputTokens:      a.in,
		OutputTokens:     a.out,
		CacheReadTokens:  a.cacheRead,
		CacheWriteTokens: a.cacheWrite,
		ResponseModel:    strings.TrimPrefix(a.model, "models/"),
		ResponseID:       a.id,
		FinishReasons:    a.finish,
		ErrorType:        a.errorType,
	}
	if a.dialect == dialectAnthropic {
		u.InputTokens = a.aIn + a.aCacheRead + a.aCacheWrite
		u.CacheReadTokens, u.CacheWriteTokens = a.aCacheRead, a.aCacheWrite
	}
	if a.capture && (a.text.Len() > 0 || len(a.parts) > 0) {
		parts := make([]Part, 0, 1+len(a.parts))
		if a.text.Len() > 0 {
			parts = append(parts, Part{Type: "text", Content: a.text.String()})
		}
		parts = append(parts, a.parts...)
		m := Message{Role: "assistant", Parts: parts}
		if len(a.finish) > 0 {
			m.FinishReason = a.finish[0]
		}
		u.OutputMessages = []Message{m}
	}
	return u
}

// ---------------------------------------------------------------------------------------------
// Server-sent events

// maxSSELine bounds a single line / event; beyond it parsing stops (bytes still pass through).
const maxSSELine = 4 << 20

// sseParser incrementally parses a text/event-stream (WHATWG) and calls onEvent per event.
type sseParser struct {
	onEvent func(event string, data []byte)
	line    []byte
	event   string
	data    []byte
	hasData bool
	broken  bool
}

func (s *sseParser) write(p []byte) {
	for len(p) > 0 && !s.broken {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.line = append(s.line, p...)
			if len(s.line) > maxSSELine {
				s.broken = true
			}
			return
		}
		s.line = append(s.line, p[:i]...)
		p = p[i+1:]
		s.processLine(bytes.TrimSuffix(s.line, []byte{'\r'}))
		s.line = s.line[:0]
	}
}

func (s *sseParser) processLine(l []byte) {
	if len(l) == 0 {
		s.dispatch()
		return
	}
	if l[0] == ':' {
		return
	}
	field, value := l, []byte(nil)
	if i := bytes.IndexByte(l, ':'); i >= 0 {
		field, value = l[:i], l[i+1:]
		value = bytes.TrimPrefix(value, []byte{' '})
	}
	switch string(field) {
	case "event":
		s.event = string(value)
	case "data":
		if s.hasData {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, value...)
		s.hasData = true
		if len(s.data) > maxSSELine {
			s.broken = true
		}
	}
}

func (s *sseParser) dispatch() {
	if s.hasData && s.onEvent != nil {
		s.onEvent(s.event, s.data)
	}
	s.event, s.data, s.hasData = "", s.data[:0], false
}

// flush dispatches a trailing event without a final blank line.
func (s *sseParser) flush() {
	if s.broken {
		return
	}
	if len(s.line) > 0 {
		s.processLine(bytes.TrimSuffix(s.line, []byte{'\r'}))
		s.line = s.line[:0]
	}
	s.dispatch()
}
