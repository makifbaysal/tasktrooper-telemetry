package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// Enricher interprets items (never extracts them). budget (nil: unlimited) gates every request (AI quota, ADR-018).
type Enricher interface {
	Enrich(ctx context.Context, repo RepoSummary, items []*model.Item, budget Budget) (Outcome, error)
	Model() string
}

// Budget is the AI quota of one analysis (implemented by internal/quota over services/pkg/aiquota).
type Budget interface {
	// Reserve is asked before every request with its estimated tokens; ok=false (reason org_limit | user_limit |
	// check_failed) stops LLM calls for the remaining items, which keep their static criticality.
	Reserve(ctx context.Context, model string, estInputTokens, estOutputTokens int64) (ok bool, reason string)
	// Record stores the real usage of a billed request and returns its credits.
	Record(ctx context.Context, model string, u Tokens) float64
}

// Tokens is the usage of one request (Anthropic semantics: Input excludes cache reads and writes).
type Tokens struct {
	Input, CacheWrite, CacheRead, Output int64
}

// Outcome is the LLM step result of one Enrich call.
type Outcome struct {
	Status  string  // model.LLMOK | model.LLMQuotaExceeded
	Reason  string  // quota denial reason
	Credits float64 // spent (recorded) credits
	Skipped int     // items not sent because the quota denied a batch
}

// RepoSummary is the stable, cached prefix of every request for one analysis.
type RepoSummary struct {
	Name      string
	Languages []string
	Services  []string
	Inventory []string // "kind service key" lines
}

// LLM calls the Claude Messages API with structured output. Model id comes from TT_CODEINTEL_MODEL (never hard-coded).
type LLM struct {
	client    anthropic.Client
	model     string
	BatchSize int // items per request (default 15)
	MaxChars  int // handler source cap per item (default 6000 ≈ 1.5k tokens)
	Log       *slog.Logger
}

// NewLLM returns nil when either the API key or the model is empty (LLM step disabled → static rules only).
func NewLLM(apiKey, model string, opts ...option.RequestOption) *LLM {
	if apiKey == "" || model == "" {
		return nil
	}
	opts = append([]option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(3)}, opts...)
	return &LLM{client: anthropic.NewClient(opts...), model: model, BatchSize: 15, MaxChars: 6000, Log: slog.Default()}
}

// Model returns the configured model id.
func (l *LLM) Model() string { return l.model }

const systemPrompt = `You classify the business impact of code inventory items (HTTP endpoints, message consumers, scheduled jobs, UI views, outgoing dependencies) found in a customer's repository by deterministic extractors. You do not discover new items; you interpret the ones given.

For every item return:
- criticality: "critical" (money movement, checkout/order placement, authentication, data loss), "high" (important user-facing flow or a consumer/job whose failure silently loses work), "medium" (state-changing but recoverable), "low" (read-only, internal, health checks).
- business_domain: one or two words (e.g. "payments", "orders", "identity", "catalog", "notifications", "internal").
- summary: one sentence on what the item does, from the handler source.
- failure_modes: up to 4 short, concrete ways it fails in production (e.g. "payment provider timeout", "duplicate charge on retry").
- suggested_monitors: monitor templates worth having, each with a one-sentence reason. Templates: error_rate, latency, throughput_drop, error_new, consumer_lag, job_missed, dlq.
- loadtest_hint: payload_example is a minimal JSON request body as a string ("" for requests without a body), idempotent tells whether repeating the call is safe, notes is a short caveat ("" if none).

Base every answer on the given source; when the source is missing, infer from the route, names and dependencies and keep the summary generic. Return exactly one result per input id.`

var outputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"items": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":              map[string]any{"type": "integer"},
					"criticality":     map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low"}},
					"business_domain": map[string]any{"type": "string"},
					"summary":         map[string]any{"type": "string"},
					"failure_modes":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"suggested_monitors": map[string]any{"type": "array", "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"template": map[string]any{"type": "string", "enum": []string{"error_rate", "latency", "throughput_drop", "error_new", "consumer_lag", "job_missed", "dlq"}},
							"reason":   map[string]any{"type": "string"},
						},
						"required":             []string{"template", "reason"},
						"additionalProperties": false,
					}},
					"loadtest_hint": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"payload_example": map[string]any{"type": "string"},
							"idempotent":      map[string]any{"type": "boolean"},
							"notes":           map[string]any{"type": "string"},
						},
						"required":             []string{"payload_example", "idempotent", "notes"},
						"additionalProperties": false,
					},
				},
				"required":             []string{"id", "criticality", "business_domain", "summary", "failure_modes", "suggested_monitors", "loadtest_hint"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"items"},
	"additionalProperties": false,
}

type llmItem struct {
	ID                int                      `json:"id"`
	Criticality       string                   `json:"criticality"`
	BusinessDomain    string                   `json:"business_domain"`
	Summary           string                   `json:"summary"`
	FailureModes      []string                 `json:"failure_modes"`
	SuggestedMonitors []model.SuggestedMonitor `json:"suggested_monitors"`
	LoadtestHint      struct {
		PayloadExample string `json:"payload_example"`
		Idempotent     bool   `json:"idempotent"`
		Notes          string `json:"notes"`
	} `json:"loadtest_hint"`
}

type llmInput struct {
	ID        int      `json:"id"`
	Kind      string   `json:"kind"`
	Service   string   `json:"service"`
	Key       string   `json:"key"`
	Source    string   `json:"source,omitempty"`
	Framework string   `json:"framework,omitempty"`
	File      string   `json:"file,omitempty"`
	Handler   string   `json:"handler,omitempty"`
	Code      string   `json:"handler_source,omitempty"`
	Callees   []string `json:"callee_signatures,omitempty"`
	Traffic   string   `json:"observed_traffic,omitempty"`
}

func summaryText(r RepoSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\nLanguages: %s\nServices: %s\n\nFull inventory (kind service key):\n", r.Name,
		strings.Join(r.Languages, ", "), strings.Join(r.Services, ", "))
	inv := append([]string(nil), r.Inventory...)
	sort.Strings(inv) // deterministic → cacheable prefix
	if len(inv) > 600 {
		inv = inv[:600]
	}
	for _, l := range inv {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// maxTokens is the output cap of one request.
const maxTokens = 16000

// OutputPerItem is the expected output tokens per item used for the pre-call quota estimate (a batch of 15 items
// answers with ~4–6k tokens; estimating with maxTokens would deny long before the limit).
const OutputPerItem = 600

// Enrich fills it.LLM for items in batches; a failed batch is logged and skipped (static rules stay). Before every
// batch the budget is asked; a denial leaves the remaining items static and reports quota_exceeded.
func (l *LLM) Enrich(ctx context.Context, repo RepoSummary, items []*model.Item, budget Budget) (Outcome, error) {
	sum := summaryText(repo)
	system := []anthropic.TextBlockParam{
		{Text: systemPrompt},
		{Text: sum, CacheControl: anthropic.NewCacheControlEphemeralParam()},
	}
	out := Outcome{Status: model.LLMOK}
	var firstErr error
	for start := 0; start < len(items); start += l.BatchSize {
		batch := items[start:min(start+l.BatchSize, len(items))]
		payload := l.payload(batch)
		if budget != nil {
			chars := len(systemPrompt) + len(sum) + len(payload)
			if ok, reason := budget.Reserve(ctx, l.model, int64((chars+3)/4), min(int64(maxTokens), int64(OutputPerItem*len(batch)))); !ok {
				out.Status, out.Reason, out.Skipped = model.LLMQuotaExceeded, reason, len(items)-start
				l.Log.Info("llm enrichment stopped by AI quota", "reason", reason, "skipped", out.Skipped)
				break
			}
		}
		usage, err := l.batch(ctx, system, batch, payload)
		if usage != nil && budget != nil {
			out.Credits += budget.Record(ctx, l.model, *usage)
		}
		if err != nil {
			l.Log.Warn("llm enrichment batch failed", "err", err, "items", len(batch))
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
		}
	}
	return out, firstErr
}

// payload is the user message of one batch.
func (l *LLM) payload(batch []*model.Item) string {
	in := make([]llmInput, len(batch))
	for i, it := range batch {
		code := it.Snippet
		if len(code) > l.MaxChars {
			code = code[:l.MaxChars] + "\n…(truncated)"
		}
		in[i] = llmInput{ID: i, Kind: it.Kind, Service: it.Service, Key: it.Key, Source: it.Source, Framework: it.Framework,
			File: it.File, Handler: it.Handler, Code: code, Callees: it.Callees}
		if s := it.Stats; s != nil {
			in[i].Traffic = fmt.Sprintf("%.1f rpm, error rate %.2f%%, p95 %.0f ms (7d)", s.RPM, s.ErrorRate*100, s.P95)
		}
	}
	b, _ := json.MarshalIndent(in, "", " ")
	return "Classify these inventory items:\n" + string(b)
}

// batch sends one request. usage is non-nil whenever the API answered (billed), even when the answer is unusable.
func (l *LLM) batch(ctx context.Context, system []anthropic.TextBlockParam, batch []*model.Item, payload string) (*Tokens, error) {
	resp, err := l.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(l.model),
		MaxTokens: maxTokens,
		System:    system,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(payload)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: outputSchema},
		},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("anthropic %d: %w", apiErr.StatusCode, err)
		}
		return nil, err
	}
	usage := &Tokens{Input: resp.Usage.InputTokens, CacheWrite: resp.Usage.CacheCreationInputTokens,
		CacheRead: resp.Usage.CacheReadInputTokens, Output: resp.Usage.OutputTokens}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return usage, errors.New("anthropic: request refused")
	}
	if resp.StopReason == anthropic.StopReasonMaxTokens {
		return usage, errors.New("anthropic: output truncated (max_tokens)")
	}
	var text string
	for _, b := range resp.Content {
		if tb, ok := b.AsAny().(anthropic.TextBlock); ok {
			text += tb.Text
		}
	}
	var out struct {
		Items []llmItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return usage, fmt.Errorf("anthropic: decode structured output: %w", err)
	}
	l.Log.Debug("llm batch", "items", len(batch), "cache_read", resp.Usage.CacheReadInputTokens, "cache_write", resp.Usage.CacheCreationInputTokens)
	for _, r := range out.Items {
		if r.ID < 0 || r.ID >= len(batch) {
			continue
		}
		res := &model.LLMResult{Criticality: r.Criticality, BusinessDomain: r.BusinessDomain, Summary: r.Summary,
			FailureModes: r.FailureModes, SuggestedMonitors: r.SuggestedMonitors, Model: l.model}
		res.LoadtestHint = &model.LoadtestHint{Idempotent: r.LoadtestHint.Idempotent, Notes: r.LoadtestHint.Notes}
		if p := strings.TrimSpace(r.LoadtestHint.PayloadExample); p != "" && json.Valid([]byte(p)) {
			res.LoadtestHint.PayloadExample = json.RawMessage(p)
		}
		Apply(batch[r.ID], res)
	}
	return usage, nil
}

// Apply attaches an LLM result (fresh or cached by code hash) and lets it decide criticality.
func Apply(it *model.Item, res *model.LLMResult) {
	if res == nil {
		return
	}
	it.LLM = res
	if model.CriticalityRank(res.Criticality) > 0 || res.Criticality == model.Low {
		it.Criticality = res.Criticality
	}
	if res.Summary != "" {
		it.Summary = res.Summary
	}
}
