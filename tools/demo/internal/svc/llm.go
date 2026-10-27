package svc

// Simulated LLM calls (ADR-023, contracts §25) recorded with the Go SDK's llm package inside the
// services' real request / message traces, so they show up as `ai` dependencies, in llm_1m and — for
// the support assistant, which runs with content capture on — with prompts in llm_content.
// No provider is called: latency, streaming (TTFT), token usage, prompt caching and rate limits are
// drawn from realistic distributions. All message content is invented and PII-free.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry-go/llm"
)

// LLM models of the demo world.
const (
	AssistantModel     = "claude-sonnet-4-5"
	AssistantRespModel = "claude-sonnet-4-5-20250929"
	EmbeddingModel     = "text-embedding-3-small"
	SubjectModel       = "gemini-2.5-flash"
	assistantCached    = 2312 // system prompt + tool definitions (prompt-cached)
)

var errRateLimited = errors.New("rate limited")

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func msD(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }

const b62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62(n int) string {
	var b strings.Builder
	for range n {
		b.WriteByte(b62[rand.IntN(len(b62))])
	}
	return b.String()
}

// ---------------------------------------------------------------- support assistant (storefront-bff, anthropic)

const assistantSystem = `You are the customer support assistant of an online shop. Answer briefly and politely in the
customer's language. Use the get_order_status tool for anything about a specific order; never guess delivery dates.
Do not ask for or repeat payment card data. Escalate refunds above 5,000 TRY to a human agent.`

var assistantTools = `[{"name":"get_order_status","description":"Status, carrier and tracking events of an order","input_schema":{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}}]`

type supportTopic struct {
	question  func(order string) string
	needsTool bool
	answer    func(order string) string
}

var supportTopics = []supportTopic{
	{func(o string) string { return "Where is my order " + o + "? It said it would arrive yesterday." }, true,
		func(o string) string {
			return "Your order " + o + " left our Istanbul warehouse on Saturday and is with the carrier (tracking shows \"out for delivery\"). " +
				"It should arrive today before 20:00. Sorry for the delay — you'll get an SMS when the courier is close."
		}},
	{func(o string) string { return "Can I still change the delivery address for " + o + "?" }, true,
		func(o string) string {
			return "Order " + o + " hasn't been handed to the carrier yet, so the address can still be changed. " +
				"Open the order under Account → Orders and tap \"Change address\"; the change is possible until the status becomes \"Shipped\"."
		}},
	{func(o string) string { return "I want to cancel " + o + ", I ordered the wrong size." }, true,
		func(o string) string {
			return "No problem. Order " + o + " is still being prepared, so you can cancel it from the order page (\"Cancel order\"). " +
				"The refund goes back to your original payment method within 3–5 business days. If it has already shipped, you can return it for free within 14 days."
		}},
	{func(string) string { return "Do you ship to Northern Cyprus?" }, false,
		func(string) string {
			return "Yes, we ship to Northern Cyprus. Delivery takes 4–7 business days and a flat shipping fee of 149.90 TRY applies; " +
				"customs duties are included in the price shown at checkout."
		}},
	{func(string) string { return "How do I return a pair of sneakers? The box is opened." }, false,
		func(string) string {
			return "You can return unworn items within 14 days even if the box was opened. Go to Account → Orders, choose the item and \"Return\", " +
				"then drop the parcel at any partner cargo point with the return code. The refund is issued once the warehouse checks the item."
		}},
	{func(o string) string { return "My payment for " + o + " was taken twice!" }, true,
		func(o string) string {
			return "I checked order " + o + ": there is one captured payment and one pre-authorization that was released automatically. " +
				"The released amount usually disappears from your statement within 1–3 business days, depending on your bank."
		}},
}

// handleAssistant serves POST /v1/assistant/messages: one turn of the support chat. With probability
// the model first calls the get_order_status tool (an orders-api request inside an execute_tool span),
// then streams the final answer.
func (a *App) handleAssistant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	order := fmt.Sprintf("ord_%d", ID(100000, 999999))
	topic := Pick(supportTopics)
	conv := "conv_" + HexID(20)
	firstTurn := Chance(0.7)
	sys := []llm.Part{{Type: "text", Content: assistantSystem}}
	history := []llm.Message{}
	if !firstTurn {
		history = append(history,
			llm.Text("user", "Hi, I have a question about my order."),
			llm.Text("assistant", "Hi! Sure — what would you like to know?"))
	}
	input := append(history, llm.Text("user", topic.question(order)))
	uncached := int64(ID(90, 260) + 60*len(history))

	if topic.needsTool && Chance(0.85) {
		callID := "toolu_01" + base62(22)
		toolCall := llm.Message{Role: "assistant", Parts: []llm.Part{{Type: "tool_call", ID: callID, Name: "get_order_status",
			Content: fmt.Sprintf(`{"order_id":%q}`, order)}}, FinishReason: "tool_use"}
		if err := a.claude(ctx, conv, sys, input, toolCall, uncached, int64(ID(45, 95)), firstTurn); err != nil {
			a.Fail(w, r, http.StatusServiceUnavailable, err, "assistant unavailable: "+err.Error(), "provider", "anthropic")
			return
		}
		// the tool: a real orders-api request under an execute_tool span
		tctx, tool := llm.Start(ctx, llm.Request{Operation: llm.OperationExecuteTool, ToolName: "get_order_status",
			AgentName: "support-assistant", ConversationID: conv})
		_, err := a.Call(tctx, "orders-api", "GET", "/v1/orders/"+strings.TrimPrefix(order, "ord_"), nil)
		tool.End(llm.Usage{Err: err})
		result := fmt.Sprintf(`{"order_id":%q,"status":"in_transit","carrier":"cargo-partner","last_event":"out_for_delivery"}`, order)
		input = append(input, toolCall, llm.Message{Role: "tool", Parts: []llm.Part{{Type: "tool_call_response", ID: callID, Content: result}}})
		uncached += int64(ID(140, 220))
		firstTurn = false // the second request reads the cache the first one wrote
	}
	answer := topic.answer(order)
	out := llm.Message{Role: "assistant", Parts: []llm.Part{{Type: "text", Content: answer}}, FinishReason: "end_turn"}
	outTok := int64(len(answer)/4 + ID(5, 40))
	if err := a.claude(ctx, conv, sys, input, out, uncached, outTok, firstTurn); err != nil {
		a.Fail(w, r, http.StatusServiceUnavailable, err, "assistant unavailable: "+err.Error(), "provider", "anthropic")
		return
	}
	a.Log.InfoContext(ctx, fmt.Sprintf("assistant answered conversation=%s turns=%d", conv, len(input)/2+1))
	JSON(w, 200, fmt.Sprintf(`{"conversation_id":%q}`, conv))
}

// claude records one streaming Messages API call (retried once after a 429).
func (a *App) claude(ctx context.Context, conv string, sys []llm.Part, in []llm.Message, out llm.Message, uncached, outTok int64, writeCache bool) error {
	for attempt := 0; ; attempt++ {
		_, call := llm.Start(ctx, llm.Request{Provider: "anthropic", Model: AssistantModel, Operation: llm.OperationChat,
			MaxTokens: 1024, Temperature: 0.3, Stream: true, ConversationID: conv, AgentName: "support-assistant",
			ServerAddress: "api.anthropic.com", ServerPort: 443, SystemInstructions: sys, InputMessages: in})
		switch {
		case Chance(0.025): // 429 rate_limit_error (output tokens per minute)
			sleep(ctx, msD(Lognormal(120, 0.4)))
			call.End(llm.Usage{Err: errors.New("anthropic: 429 rate_limit_error: This request would exceed the rate limit for your organization of 80,000 output tokens per minute"),
				ErrorType: "429"})
			if attempt == 0 {
				sleep(ctx, msD(Lognormal(700, 0.3))) // retry-after
				continue
			}
			return errRateLimited
		case Chance(0.004): // 529 overloaded_error
			sleep(ctx, msD(Lognormal(900, 0.4)))
			call.End(llm.Usage{Err: errors.New("anthropic: 529 overloaded_error: Overloaded"), ErrorType: "529"})
			return errors.New("provider overloaded")
		}
		ttft := Lognormal(620, 0.35)
		if writeCache {
			ttft *= 1.3 // cache write
		}
		sleep(ctx, msD(ttft))
		call.FirstToken()
		tps := math.Max(35, Lognormal(78, 0.2)) // output tokens / s
		sleep(ctx, msD(float64(outTok)/tps*1000))
		u := llm.Usage{OutputTokens: outTok, ResponseModel: AssistantRespModel, ResponseID: "msg_01" + base62(22),
			FinishReasons: []string{out.FinishReason}, OutputMessages: []llm.Message{out}}
		if writeCache {
			u.CacheWriteTokens = assistantCached
		} else {
			u.CacheReadTokens = assistantCached
		}
		u.InputTokens = uncached + assistantCached // OTel: input includes cached tokens
		call.End(u)
		return nil
	}
}

// ---------------------------------------------------------------- embeddings (catalog-api, openai)

// embed records an embeddings call for the given inputs; false = rate limited (caller falls back).
func (a *App) embed(ctx context.Context, tokens int64, batch int) bool {
	_, call := llm.Start(ctx, llm.Request{Provider: "openai", Model: EmbeddingModel, Operation: llm.OperationEmbeddings,
		ServerAddress: "api.openai.com", ServerPort: 443, Attributes: nil})
	if Chance(0.004) {
		sleep(ctx, msD(Lognormal(35, 0.3)))
		call.End(llm.Usage{Err: errors.New("openai: 429 Rate limit reached for text-embedding-3-small on tokens per min (TPM): Limit 1000000"),
			ErrorType: "429"})
		return false
	}
	sleep(ctx, msD(Lognormal(48+6*float64(batch), 0.4)))
	call.End(llm.Usage{InputTokens: tokens, ResponseModel: EmbeddingModel})
	return true
}

// ---------------------------------------------------------------- subject lines (email-worker, gemini)

// subjectLine records a Gemini call that personalizes a newsletter subject; false = fell back to the default.
func (a *App) subjectLine(ctx context.Context) bool {
	_, call := llm.Start(ctx, llm.Request{Provider: "gcp.gemini", Model: SubjectModel, Operation: llm.OperationGenerateContent,
		MaxTokens: 256, Temperature: 0.9, ServerAddress: "generativelanguage.googleapis.com", ServerPort: 443})
	if Chance(0.015) {
		sleep(ctx, msD(Lognormal(90, 0.3)))
		call.End(llm.Usage{Err: errors.New("gemini: 429 RESOURCE_EXHAUSTED: Quota exceeded for generate_content_paid_tier_input_token_count"),
			ErrorType: "429"})
		a.Log.WarnContext(ctx, "subject personalization rate limited, using default subject provider=gemini")
		return false
	}
	in := int64(ID(640, 1050))
	u := llm.Usage{InputTokens: in, OutputTokens: int64(ID(22, 70)), ResponseModel: SubjectModel, FinishReasons: []string{"STOP"},
		ResponseID: base62(24)}
	if Chance(0.4) { // implicit caching of the shared prompt prefix
		u.CacheReadTokens = 512
	}
	sleep(ctx, msD(Lognormal(480, 0.35)))
	call.End(u)
	return true
}
