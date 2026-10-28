// Package llm records LLM (generative AI) calls as OpenTelemetry GenAI client spans for TaskTrooper
// AI monitoring (.ai/contracts.md §25, ADR-023).
//
// Zero-code: wrap the HTTP client of an OpenAI / Anthropic / Gemini (or OpenAI-compatible) SDK.
// Model, token usage (streams included), time to first token and finish reasons are read from the
// request and response JSON without changing the bytes the SDK sees:
//
//	client := openai.NewClient(option.WithHTTPClient(llm.HTTPClient(nil)))
//	client := anthropic.NewClient(option.WithHTTPClient(llm.HTTPClient(nil)))
//
// Manual:
//
//	ctx, call := llm.Start(ctx, llm.Request{Provider: "anthropic", Model: model, Operation: "chat"})
//	resp, err := doCall(ctx)
//	call.End(llm.Usage{InputTokens: resp.In, OutputTokens: resp.Out, Err: err})
//
// Prompt and completion content is recorded only when content capture is enabled
// (TROOPER_LLM_CAPTURE_CONTENT=true, SetCaptureContent or WithCaptureContent).
//
// Nothing in this package panics or alters the application's requests: parsing failures only
// lose telemetry and are counted in trooper.sdk.errors{component="llm"}.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry-go/internal/sdkerr"
)

// ScopeName is the instrumentation scope of LLM spans.
const ScopeName = "github.com/tasktrooper/telemetry-go/llm"

// Operation names (gen_ai.operation.name).
const (
	OperationChat            = "chat"
	OperationTextCompletion  = "text_completion"
	OperationEmbeddings      = "embeddings"
	OperationGenerateContent = "generate_content"
	OperationExecuteTool     = "execute_tool"
	OperationInvokeAgent     = "invoke_agent"
	OperationCreateAgent     = "create_agent"
)

// Attribute keys (OTel GenAI semantic conventions + trooper.llm.ttft_ms).
const (
	AttrOperation          = attribute.Key("gen_ai.operation.name")
	AttrProvider           = attribute.Key("gen_ai.provider.name")
	AttrRequestModel       = attribute.Key("gen_ai.request.model")
	AttrResponseModel      = attribute.Key("gen_ai.response.model")
	AttrRequestMaxTokens   = attribute.Key("gen_ai.request.max_tokens")
	AttrRequestTemperature = attribute.Key("gen_ai.request.temperature")
	AttrRequestTopP        = attribute.Key("gen_ai.request.top_p")
	AttrResponseID         = attribute.Key("gen_ai.response.id")
	AttrFinishReasons      = attribute.Key("gen_ai.response.finish_reasons")
	AttrInputTokens        = attribute.Key("gen_ai.usage.input_tokens")
	AttrOutputTokens       = attribute.Key("gen_ai.usage.output_tokens")
	AttrCacheReadTokens    = attribute.Key("gen_ai.usage.cache_read.input_tokens")
	AttrCacheWriteTokens   = attribute.Key("gen_ai.usage.cache_creation.input_tokens")
	AttrConversationID     = attribute.Key("gen_ai.conversation.id")
	AttrAgentName          = attribute.Key("gen_ai.agent.name")
	AttrToolName           = attribute.Key("gen_ai.tool.name")
	AttrInputMessages      = attribute.Key("gen_ai.input.messages")
	AttrOutputMessages     = attribute.Key("gen_ai.output.messages")
	AttrSystemInstructions = attribute.Key("gen_ai.system_instructions")
	AttrTTFT               = attribute.Key("trooper.llm.ttft_ms")
	AttrErrorType          = attribute.Key("error.type")
)

var capture atomic.Int32 // 0 = from env, 1 = on, 2 = off

// SetCaptureContent turns recording of prompts and completions on or off process-wide, overriding
// TROOPER_LLM_CAPTURE_CONTENT. Content can contain PII; it is off by default.
func SetCaptureContent(on bool) {
	if on {
		capture.Store(1)
	} else {
		capture.Store(2)
	}
}

// CaptureContent reports whether content capture is enabled process-wide.
func CaptureContent() bool {
	switch capture.Load() {
	case 1:
		return true
	case 2:
		return false
	}
	v, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("TROOPER_LLM_CAPTURE_CONTENT")))
	return v
}

// Part is one part of a message in the GenAI semantic-convention format.
type Part struct {
	// Type: "text", "tool_call", "tool_call_response", "reasoning", or a provider-specific type
	// such as "image".
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
}

// Message is a chat message in the GenAI semantic-convention format.
type Message struct {
	Role         string `json:"role"`
	Parts        []Part `json:"parts"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// Text returns a message with a single text part.
func Text(role, content string) Message {
	return Message{Role: role, Parts: []Part{{Type: "text", Content: content}}}
}

// Request describes an LLM call.
type Request struct {
	// Provider is gen_ai.provider.name: openai, anthropic, gcp.gemini, gcp.vertex_ai, aws.bedrock,
	// azure.ai.openai, mistral_ai, groq, deepseek, x_ai, ollama …
	Provider string
	// Model is the requested model (gen_ai.request.model).
	Model string
	// Operation is gen_ai.operation.name; default "chat".
	Operation string
	// MaxTokens is recorded when > 0.
	MaxTokens int
	// Temperature is recorded when non-zero (use Attributes to record an explicit 0).
	Temperature float64
	// TopP is recorded when non-zero.
	TopP float64
	// Stream marks a streaming call (documentation only; TTFT comes from Call.FirstToken).
	Stream         bool
	ConversationID string
	AgentName      string
	ToolName       string
	// ServerAddress / ServerPort of the provider endpoint (server.address / server.port).
	ServerAddress string
	ServerPort    int
	// SystemInstructions and InputMessages are recorded only with content capture enabled.
	SystemInstructions []Part
	InputMessages      []Message
	// Attributes are extra span attributes.
	Attributes []attribute.KeyValue
}

// Usage is the outcome of an LLM call.
type Usage struct {
	// InputTokens should include cached input tokens (OTel GenAI convention); the transport sums
	// Anthropic's input_tokens + cache_read_input_tokens + cache_creation_input_tokens.
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ResponseModel    string
	ResponseID       string
	FinishReasons    []string
	// OutputMessages are recorded only with content capture enabled.
	OutputMessages []Message
	// Err marks the call as failed (span status Error, error.type).
	Err error
	// ErrorType overrides error.type (e.g. an HTTP status code "429"); defaults to Err's type.
	ErrorType string
	// Attributes are extra span attributes.
	Attributes []attribute.KeyValue
}

// Call is an in-flight LLM call. All methods are safe on nil and for concurrent use.
type Call struct {
	span    trace.Span
	start   time.Time
	capture bool
	ttft    atomic.Int64 // ns since start; 0 = none
	once    sync.Once
}

// Start starts a GenAI client span named "<operation> <model>" and returns a context carrying it.
// End must be called exactly once (further calls are ignored).
func Start(ctx context.Context, req Request) (context.Context, *Call) {
	return start(ctx, req, CaptureContent())
}

func start(ctx context.Context, req Request, captureContent bool) (rctx context.Context, call *Call) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if recover() != nil {
			sdkerr.Count("llm")
			rctx, call = ctx, nil
		}
	}()
	op := req.Operation
	if op == "" {
		op = OperationChat
	}
	name := strings.TrimSpace(op + " " + req.Model)
	kv := make([]attribute.KeyValue, 0, 12+len(req.Attributes))
	kv = append(kv, AttrOperation.String(op))
	if req.Provider != "" {
		kv = append(kv, AttrProvider.String(req.Provider))
	}
	if req.Model != "" {
		kv = append(kv, AttrRequestModel.String(req.Model))
	}
	if req.MaxTokens > 0 {
		kv = append(kv, AttrRequestMaxTokens.Int(req.MaxTokens))
	}
	if req.Temperature != 0 {
		kv = append(kv, AttrRequestTemperature.Float64(req.Temperature))
	}
	if req.TopP != 0 {
		kv = append(kv, AttrRequestTopP.Float64(req.TopP))
	}
	if req.ConversationID != "" {
		kv = append(kv, AttrConversationID.String(req.ConversationID))
	}
	if req.AgentName != "" {
		kv = append(kv, AttrAgentName.String(req.AgentName))
	}
	if req.ToolName != "" {
		kv = append(kv, AttrToolName.String(req.ToolName))
	}
	if req.ServerAddress != "" {
		kv = append(kv, attribute.String("server.address", req.ServerAddress))
		if req.ServerPort > 0 {
			kv = append(kv, attribute.Int("server.port", req.ServerPort))
		}
	}
	if captureContent {
		if s := marshalContent(req.SystemInstructions); s != "" {
			kv = append(kv, AttrSystemInstructions.String(s))
		}
		if s := marshalContent(req.InputMessages); s != "" {
			kv = append(kv, AttrInputMessages.String(s))
		}
	}
	kv = append(kv, req.Attributes...)
	kind := trace.SpanKindClient
	if op == OperationExecuteTool || op == OperationInvokeAgent || op == OperationCreateAgent {
		kind = trace.SpanKindInternal
	}
	ctx, span := otel.Tracer(ScopeName).Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(kv...))
	return ctx, &Call{span: span, start: time.Now(), capture: captureContent}
}

// Span returns the span of the call (a no-op span for nil).
func (c *Call) Span() trace.Span {
	if c == nil || c.span == nil {
		return trace.SpanFromContext(context.Background())
	}
	return c.span
}

// FirstToken records the time to first token (trooper.llm.ttft_ms) of a streaming call. Only the
// first call counts.
func (c *Call) FirstToken() {
	if c == nil {
		return
	}
	d := max(time.Since(c.start), 1)
	c.ttft.CompareAndSwap(0, int64(d))
}

// End records usage and ends the span. Only the first call has an effect.
func (c *Call) End(u Usage) {
	if c == nil || c.span == nil {
		return
	}
	c.once.Do(func() {
		defer func() {
			if recover() != nil {
				sdkerr.Count("llm")
			}
		}()
		defer c.span.End()
		kv := make([]attribute.KeyValue, 0, 12+len(u.Attributes))
		if u.ResponseModel != "" {
			kv = append(kv, AttrResponseModel.String(u.ResponseModel))
		}
		if u.ResponseID != "" {
			kv = append(kv, AttrResponseID.String(u.ResponseID))
		}
		if len(u.FinishReasons) > 0 {
			kv = append(kv, AttrFinishReasons.StringSlice(u.FinishReasons))
		}
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			kv = append(kv, AttrInputTokens.Int64(u.InputTokens), AttrOutputTokens.Int64(u.OutputTokens))
		}
		if u.CacheReadTokens > 0 {
			kv = append(kv, AttrCacheReadTokens.Int64(u.CacheReadTokens))
		}
		if u.CacheWriteTokens > 0 {
			kv = append(kv, AttrCacheWriteTokens.Int64(u.CacheWriteTokens))
		}
		if t := c.ttft.Load(); t > 0 {
			kv = append(kv, AttrTTFT.Float64(float64(t)/1e6))
		}
		if c.capture {
			if s := marshalContent(u.OutputMessages); s != "" {
				kv = append(kv, AttrOutputMessages.String(s))
			}
		}
		kv = append(kv, u.Attributes...)
		if u.Err != nil || u.ErrorType != "" {
			typ := u.ErrorType
			if typ == "" {
				typ = errorType(u.Err)
			}
			kv = append(kv, AttrErrorType.String(typ))
			msg := typ
			if u.Err != nil {
				c.span.RecordError(u.Err)
				msg = u.Err.Error()
			}
			c.span.SetStatus(codes.Error, msg)
		}
		c.span.SetAttributes(kv...)
	})
}

func errorType(err error) string {
	if err == nil {
		return "_OTHER"
	}
	if t := reflect.TypeOf(err); t != nil {
		return t.String()
	}
	return fmt.Sprintf("%T", err)
}

// maxPartContent bounds a single captured part; the writer truncates whole fields at 32 KB.
const maxPartContent = 16 << 10

func marshalContent[T any](v []T) string {
	if len(v) == 0 {
		return ""
	}
	switch x := any(v).(type) {
	case []Message:
		cp := make([]Message, len(x))
		for i, m := range x {
			cp[i] = m
			cp[i].Parts = truncateParts(m.Parts)
		}
		v = any(cp).([]T)
	case []Part:
		v = any(truncateParts(x)).([]T)
	}
	b, err := json.Marshal(v)
	if err != nil {
		sdkerr.Count("llm")
		return ""
	}
	return string(b)
}

func truncateParts(parts []Part) []Part {
	out := parts
	for i, p := range parts {
		if len(p.Content) > maxPartContent {
			if &out[0] == &parts[0] {
				out = append([]Part(nil), parts...)
			}
			n := maxPartContent
			for n > 0 && !utf8.RuneStart(p.Content[n]) {
				n--
			}
			out[i].Content = p.Content[:n] + "…[truncated]"
		}
	}
	return out
}
