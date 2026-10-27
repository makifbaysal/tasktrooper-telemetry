package flatten

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// AI / LLM monitoring (ADR-023, contracts §25): OTel GenAI semantic conventions on spans (plus the legacy OpenLLMetry
// names). The writer marks model calls as dependency kind "ai", normalizes the attributes the llm_1m rollup reads, and
// moves prompt / completion content out of attrs into telemetry.llm_content.

// GenAI attribute names.
const (
	attrGenAIOperation   = "gen_ai.operation.name"
	attrGenAIProvider    = "gen_ai.provider.name"
	attrGenAISystem      = "gen_ai.system" // legacy provider
	attrGenAIReqModel    = "gen_ai.request.model"
	attrGenAIRespModel   = "gen_ai.response.model"
	attrGenAIInTokens    = "gen_ai.usage.input_tokens"
	attrGenAIOutTokens   = "gen_ai.usage.output_tokens"
	attrGenAICacheRead   = "gen_ai.usage.cache_read.input_tokens"
	attrGenAICacheWrite  = "gen_ai.usage.cache_creation.input_tokens"
	attrGenAIInputMsgs   = "gen_ai.input.messages"
	attrGenAIOutputMsgs  = "gen_ai.output.messages"
	attrGenAISystemInstr = "gen_ai.system_instructions"
	attrLLMRequestType   = "llm.request.type" // OpenLLMetry: chat | completion | embedding | rerank
	attrTTFT             = "trooper.llm.ttft_ms"

	// LLMDetailsEvent is the OTel GenAI log event that may carry the content instead of the span.
	LLMDetailsEvent = "gen_ai.client.inference.operation.details"

	// MaxLLMContentLen bounds each llm_content field.
	MaxLLMContentLen = 32 << 10
	truncatedSuffix  = "…[truncated]"
	llmReservedKeys  = 8 // attr slots kept free for the normalized GenAI keys
)

// LLMContentRow is one row of telemetry.llm_content.
type LLMContentRow struct {
	OrgID, ProjectID      uint32
	TS                    time.Time
	TraceID, SpanID       []byte
	Service               string
	System, Input, Output string
}

// LLMContentColumns is the INSERT column list matching LLMContentRow.Values.
const LLMContentColumns = "org_id, project_id, ts, trace_id, span_id, service, system, input, output"

// Values returns the row in LLMContentColumns order.
func (r *LLMContentRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.TS, r.TraceID, r.SpanID, r.Service, r.System, r.Input, r.Output}
}

func (r *LLMContentRow) empty() bool { return r.System == "" && r.Input == "" && r.Output == "" }

// llmInfo is what the writer derives from a span's GenAI attributes.
type llmInfo struct {
	op, provider, model string
	vercel              bool // Vercel AI SDK provider-call span (ai.*.doGenerate / doStream / doEmbed)
}

// detectLLM classifies a span ("" op = not an LLM span).
func detectLLM(a attrIndex, name string) llmInfo {
	if op := llmOperation(a); op != "" {
		return llmInfo{op: op, provider: llmProvider(a), model: llmModel(a)}
	}
	if op := vercelOperation(a.str("ai.operationId")); op != "" || a.get("ai.operationId") == nil {
		if op == "" {
			op = vercelOperation(name)
		}
		if op != "" {
			sys := a.first(attrGenAISystem, "ai.model.provider")
			if sys != "" {
				return llmInfo{op: op, provider: vercelProvider(sys), model: a.first(attrGenAIRespModel, attrGenAIReqModel, "ai.response.model", "ai.model.id"), vercel: true}
			}
		}
	}
	return llmInfo{}
}

// vercelOperation maps Vercel AI SDK provider-call span ids (ai.generateText.doGenerate, ai.streamText.doStream,
// ai.embed.doEmbed, ai.embedMany.doEmbed, ai.generateObject.doGenerate …) to GenAI operations. The outer wrappers
// (ai.generateText) are not LLM spans: counting them too would double every call.
func vercelOperation(id string) string {
	if !strings.HasPrefix(id, "ai.") {
		return ""
	}
	switch {
	case strings.HasSuffix(id, ".doGenerate"), strings.HasSuffix(id, ".doStream"):
		return "chat"
	case strings.HasSuffix(id, ".doEmbed"):
		return "embeddings"
	}
	return ""
}

// vercelProvider maps the Vercel AI SDK provider id ("anthropic.messages", "openai.chat", "google.generative-ai") to
// the GenAI provider names.
func vercelProvider(sys string) string {
	sys = strings.ToLower(strings.TrimSpace(sys))
	first, _, _ := strings.Cut(sys, ".")
	switch {
	case strings.HasPrefix(sys, "google.vertex"), first == "vertex":
		return "gcp.vertex_ai"
	case first == "google":
		return "gcp.gemini"
	case first == "amazon-bedrock", first == "bedrock":
		return "aws.bedrock"
	case first == "azure-openai", first == "azure":
		return "azure.ai.openai"
	case first == "mistral":
		return "mistral_ai"
	case first == "xai":
		return "x_ai"
	}
	return first
}

// llmOperation returns the GenAI operation of a span ("" = not an LLM span). OpenLLMetry's llm.request.type is the
// fallback for instrumentations that predate gen_ai.operation.name.
func llmOperation(a attrIndex) string {
	if op := a.str(attrGenAIOperation); op != "" {
		return op
	}
	switch strings.ToLower(a.str(attrLLMRequestType)) {
	case "chat":
		return "chat"
	case "completion":
		return "text_completion"
	case "embedding", "embeddings":
		return "embeddings"
	case "rerank":
		return "rerank"
	}
	return ""
}

// isModelCall: operations that call a model (agent / tool spans don't produce a dependency).
func isModelCall(op string) bool {
	switch op {
	case "", "execute_tool", "invoke_agent", "create_agent":
		return false
	}
	return true
}

// llmProvider: gen_ai.provider.name, else the legacy gen_ai.system (lower-cased: OpenLLMetry sends "OpenAI").
func llmProvider(a attrIndex) string {
	p := a.str(attrGenAIProvider)
	if p == "" {
		p = a.str(attrGenAISystem)
	}
	return strings.ToLower(strings.TrimSpace(p))
}

// llmModel: the response model wins over the requested one.
func llmModel(a attrIndex) string { return a.first(attrGenAIRespModel, attrGenAIReqModel) }

// llmDep is the dependency name of a model call: "<provider> · <model>".
func llmDep(provider, model string) string {
	if provider == "" {
		provider = "ai"
	}
	return join(provider, model)
}

// token attributes: canonical name → accepted names (first non-empty wins).
var llmTokenKeys = [...]struct {
	canonical string
	aliases   []string
}{
	{attrGenAIInTokens, []string{attrGenAIInTokens, "gen_ai.usage.prompt_tokens", "llm.usage.prompt_tokens", "ai.usage.promptTokens", "ai.usage.inputTokens"}},
	{attrGenAIOutTokens, []string{attrGenAIOutTokens, "gen_ai.usage.completion_tokens", "llm.usage.completion_tokens", "ai.usage.completionTokens", "ai.usage.outputTokens"}},
	{attrGenAICacheRead, []string{attrGenAICacheRead, "gen_ai.usage.cache_read_input_tokens", "llm.usage.cache_read_input_tokens"}},
	{attrGenAICacheWrite, []string{attrGenAICacheWrite, "gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.cache_creation_input_tokens", "llm.usage.cache_creation_input_tokens"}},
}

// legacyUsageKey: names normalized into the canonical token keys (not stored twice).
func legacyUsageKey(k string) bool {
	if strings.HasPrefix(k, "llm.usage.") || strings.HasPrefix(k, "ai.usage.") {
		return true
	}
	switch k {
	case "gen_ai.usage.prompt_tokens", "gen_ai.usage.completion_tokens",
		"gen_ai.usage.cache_read_input_tokens", "gen_ai.usage.cache_creation_input_tokens",
		"gen_ai.usage.cache_write.input_tokens":
		return true
	}
	return false
}

// hasGenAIKeys is a cheap pre-check: any gen_ai.* / llm.* attribute.
func hasGenAIKeys(kvs []*commonpb.KeyValue) bool {
	for _, kv := range kvs {
		if k := kv.GetKey(); strings.HasPrefix(k, "gen_ai.") || strings.HasPrefix(k, "llm.") || strings.HasPrefix(k, "ai.") {
			return true
		}
	}
	return false
}

// legacyMsg collects gen_ai.prompt.N.* / gen_ai.completion.N.* fields.
type legacyMsg struct {
	idx    int
	fields map[string]string
}

type legacyMsgs []*legacyMsg

func (l *legacyMsgs) set(rest string, v string) {
	idxStr, field, ok := strings.Cut(rest, ".")
	if !ok {
		return
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx > 10_000 {
		return
	}
	for _, m := range *l {
		if m.idx == idx {
			m.fields[field] = v
			return
		}
	}
	*l = append(*l, &legacyMsg{idx: idx, fields: map[string]string{field: v}})
}

// genAIMessage is the OTel GenAI message shape (gen_ai.input.messages / gen_ai.output.messages).
type genAIMessage struct {
	Role         string      `json:"role"`
	Parts        []genAIPart `json:"parts"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type genAIPart struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
}

// json renders legacy messages in the GenAI message format ("" when there are none).
func (l legacyMsgs) json() string {
	if len(l) == 0 {
		return ""
	}
	slices.SortFunc(l, func(a, b *legacyMsg) int { return a.idx - b.idx })
	out := make([]genAIMessage, 0, len(l))
	for _, m := range l {
		msg := genAIMessage{Role: m.fields["role"], FinishReason: m.fields["finish_reason"], Parts: []genAIPart{}}
		if c := m.fields["content"]; c != "" {
			msg.Parts = append(msg.Parts, genAIPart{Type: "text", Content: c})
		}
		if id := m.fields["tool_call_id"]; id != "" {
			msg.Parts = append(msg.Parts, genAIPart{Type: "tool_call_response", ID: id})
		}
		// tool_calls.N.{id,name,arguments}
		var calls legacyMsgs
		for k, v := range m.fields {
			if rest, ok := strings.CutPrefix(k, "tool_calls."); ok {
				calls.set(rest, v)
			}
		}
		slices.SortFunc(calls, func(a, b *legacyMsg) int { return a.idx - b.idx })
		for _, c := range calls {
			p := genAIPart{Type: "tool_call", ID: c.fields["id"], Name: c.fields["name"]}
			if args := c.fields["arguments"]; args != "" {
				if json.Valid([]byte(args)) {
					p.Arguments = json.RawMessage(args)
				} else {
					p.Arguments = args
				}
			}
			msg.Parts = append(msg.Parts, p)
		}
		if msg.Role == "" {
			msg.Role = "user"
		}
		out = append(out, msg)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// jsonValue renders a content attribute: strings as is (already JSON per the convention), structured values as JSON.
func jsonValue(v *commonpb.AnyValue) string {
	if s, ok := v.GetValue().(*commonpb.AnyValue_StringValue); ok {
		return s.StringValue
	}
	b, err := json.Marshal(anyToGo(v))
	if err != nil {
		return anyString(v)
	}
	return string(b)
}

func anyToGo(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		if !finite(x.DoubleValue) {
			return nil
		}
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, anyToGo(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.GetKey()] = anyToGo(kv.GetValue())
		}
		return out
	}
	return nil
}

// capContent truncates a content field to MaxLLMContentLen bytes with the "…[truncated]" marker.
func capContent(s string) string {
	if len(s) <= MaxLLMContentLen {
		return s
	}
	return capValue(s, MaxLLMContentLen-len(truncatedSuffix)) + truncatedSuffix
}

// contentCollector gathers content attributes (current and legacy names).
type contentCollector struct {
	system, input, output       string
	legacyPrompt, legacyCompl   legacyMsgs
	legacyPromptS, legacyComplS string // gen_ai.prompt / gen_ai.completion as a single string (old semconv)
	// Vercel AI SDK (recordInputs / recordOutputs)
	aiMessages, aiPrompt, aiText, aiOther string
}

// take consumes k=v when it is a content attribute.
func (c *contentCollector) take(k string, v *commonpb.AnyValue) bool {
	switch k {
	case attrGenAIInputMsgs:
		c.input = jsonValue(v)
	case attrGenAIOutputMsgs:
		c.output = jsonValue(v)
	case attrGenAISystemInstr:
		c.system = jsonValue(v)
	case "gen_ai.prompt":
		c.legacyPromptS = anyString(v)
	case "gen_ai.completion":
		c.legacyComplS = anyString(v)
	case "ai.prompt.messages":
		c.aiMessages = jsonValue(v)
	case "ai.prompt":
		c.aiPrompt = jsonValue(v)
	case "ai.response.text":
		c.aiText = anyString(v)
	case "ai.response.object", "ai.response.toolCalls":
		if c.aiOther == "" {
			c.aiOther = jsonValue(v)
		}
	default:
		if rest, ok := strings.CutPrefix(k, "gen_ai.prompt."); ok {
			c.legacyPrompt.set(rest, anyString(v))
		} else if rest, ok := strings.CutPrefix(k, "gen_ai.completion."); ok {
			c.legacyCompl.set(rest, anyString(v))
		} else {
			return false
		}
	}
	return true
}

func (c *contentCollector) row() LLMContentRow {
	r := LLMContentRow{System: c.system, Input: c.input, Output: c.output}
	if r.Input == "" {
		r.Input = firstNonEmpty(c.legacyPrompt.json(), c.legacyPromptS, c.aiMessages, c.aiPrompt)
	}
	if r.Output == "" {
		r.Output = firstNonEmpty(c.legacyCompl.json(), c.legacyComplS)
	}
	if r.Output == "" && c.aiText != "" {
		if b, err := json.Marshal([]genAIMessage{{Role: "assistant", Parts: []genAIPart{{Type: "text", Content: c.aiText}}}}); err == nil {
			r.Output = string(b)
		}
	}
	if r.Output == "" {
		r.Output = c.aiOther
	}
	r.System, r.Input, r.Output = capContent(r.System), capContent(r.Input), capContent(r.Output)
	return r
}

// genAIAttrs builds the attrs map of a span with GenAI attributes: content is split off (returned), legacy token
// names are normalized to the canonical ones, and provider / operation are set when derived from legacy names.
func genAIAttrs(kvs []*commonpb.KeyValue, a attrIndex, info llmInfo, extra int) (map[string]string, LLMContentRow) {
	var c contentCollector
	m := make(map[string]string, min(len(kvs), MaxAttrKeys)+extra)
	for _, kv := range kvs {
		k := kv.GetKey()
		if k == "" || c.take(k, kv.GetValue()) || legacyUsageKey(k) {
			continue
		}
		if len(m) >= MaxAttrKeys-llmReservedKeys {
			continue
		}
		m[k] = capValue(anyString(kv.GetValue()), MaxAttrValueLen)
	}
	if info.op != "" {
		putAttr(m, attrGenAIOperation, info.op, MaxAttrValueLen)
		putAttr(m, attrGenAIProvider, info.provider, MaxAttrValueLen)
		if info.vercel {
			putAttr(m, attrGenAIReqModel, a.first(attrGenAIReqModel, "ai.model.id"), MaxAttrValueLen)
			if rm := a.first(attrGenAIRespModel, "ai.response.model"); rm != "" {
				putAttr(m, attrGenAIRespModel, rm, MaxAttrValueLen)
			}
			if _, ok := m[attrTTFT]; !ok {
				if f, ok := a.float("ai.response.msToFirstChunk"); ok && finite(f) && f >= 0 {
					putAttr(m, attrTTFT, strconv.FormatFloat(f, 'f', -1, 64), MaxAttrValueLen)
				}
			}
		}
		// Anthropic SDK / .NET native GenAI spans: time to first chunk in seconds.
		if _, ok := m[attrTTFT]; !ok {
			if f, ok := a.float("gen_ai.response.time_to_first_chunk"); ok && finite(f) && f >= 0 {
				putAttr(m, attrTTFT, strconv.FormatFloat(f*1000, 'f', -1, 64), MaxAttrValueLen)
			}
		}
		for _, tk := range llmTokenKeys {
			for _, alias := range tk.aliases {
				if n, ok := a.int(alias); ok {
					putAttr(m, tk.canonical, strconv.FormatInt(max(n, 0), 10), MaxAttrValueLen)
					break
				}
			}
		}
	}
	return m, c.row()
}

// llmContentFromLog extracts content from a gen_ai.client.inference.operation.details log record (attributes, or a
// kvlist body with the same keys) and removes it from the log attrs.
func llmContentFromLog(attrs []*commonpb.KeyValue, body *commonpb.AnyValue, logAttrs map[string]string) LLMContentRow {
	var c contentCollector
	for _, kv := range attrs {
		if c.take(kv.GetKey(), kv.GetValue()) {
			delete(logAttrs, kv.GetKey())
		}
	}
	if kl, ok := body.GetValue().(*commonpb.AnyValue_KvlistValue); ok {
		for _, kv := range kl.KvlistValue.GetValues() {
			c.take(kv.GetKey(), kv.GetValue())
		}
	}
	return c.row()
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
