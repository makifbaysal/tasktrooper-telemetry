package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/llm"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/selftel"
	"github.com/tasktrooper/telemetry/services/pkg/llmapi"
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

// Tokens is the usage of one request. Input excludes cache reads and writes (aiquota semantics); for the
// OpenAI-compatible endpoint Input = prompt_tokens − cached_tokens, CacheRead = cached_tokens, CacheWrite = 0.
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
	Inventory []string // "kind service key" lines (service names have no spaces)
}

// LLM calls the platform's OpenAI-compatible Chat Completions endpoint (TT_ASSISTANT_API_URL / _API_KEY / _MODEL,
// ADR-034; Meta Muse Spark by default) and asks for JSON output. The model id comes from config (never hard-coded).
type LLM struct {
	client        *llmapi.Client
	model         string
	BatchSize     int // full-tier items per request (default 15)
	LiteBatchSize int // lite-tier items per request (default 30)
	MaxChars      int // handler source cap per item after compaction (default 6000 ≈ 1.5k tokens)
	Log           *slog.Logger
	// noSchema is set once the endpoint rejected response_format json_schema (HTTP 400); later requests rely on the
	// schema in the system prompt only.
	noSchema atomic.Bool
}

// NewLLM returns nil when either the API key or the model is empty (LLM step disabled → static rules only).
// Requests go through the trooper llm transport (ADR-027, contracts §25) with the endpoint host registered under its
// provider (llmapi.Provider: meta for api.meta.ai, openai for unknown OpenAI-compatible hosts): GenAI client spans with
// model, tokens and cost; prompt/completion content is recorded only with TROOPER_LLM_CAPTURE_CONTENT=true (default
// off). Network errors, 429 and 5xx are retried up to 3 times.
func NewLLM(cfg llmapi.Config) *LLM {
	if !cfg.Enabled() {
		return nil
	}
	// The HTTP client span (peer.service = API host) sits under the GenAI span, so connection/TLS time and
	// status codes of the LLM endpoint show up like any other dependency.
	peer := cfg.Host()
	if peer == "" {
		peer = "llm"
	}
	base := &http.Client{Timeout: 5 * time.Minute, Transport: selftel.Transport(nil, peer)}
	hc := llm.HTTPClient(base, llm.WithProvider(cfg.Host(), cfg.Provider()))
	return &LLM{client: &llmapi.Client{Config: cfg, HTTP: hc, Retries: 3}, model: cfg.Model, BatchSize: 15,
		LiteBatchSize: 30, MaxChars: 6000, Log: slog.Default()}
}

// Model returns the configured model id.
func (l *LLM) Model() string { return l.model }

// Tiers of a request. Full asks every field. Lite asks criticality, business domain and summary only: it is used for
// items the static rules rated low, whose monitor templates and load test hints are never used (suggestions start at
// high, load test drafts only cover write endpoints of critical services) but would be most of the output tokens.
const (
	tierFull = "full"
	tierLite = "lite"
)

const systemPrompt = `You classify the business impact of code inventory items (HTTP endpoints, message consumers, scheduled jobs, UI views, outgoing dependencies) found in a customer's repository by deterministic extractors. You do not discover new items; you interpret the ones given.

For every item return:
- criticality: "critical" (money movement, checkout/order placement, authentication, data loss), "high" (important user-facing flow or a consumer/job whose failure silently loses work), "medium" (state-changing but recoverable), "low" (read-only, internal, health checks).
- business_domain: one or two words (e.g. "payments", "orders", "identity", "catalog", "notifications", "internal").
- summary: one sentence on what the item does, from the handler source.
- failure_modes: up to 4 short, concrete ways it fails in production (e.g. "payment provider timeout", "duplicate charge on retry").
- suggested_monitors: monitor templates worth having, each with a one-sentence reason. Templates: error_rate, latency, throughput_drop, error_new, consumer_lag, job_missed, dlq.
- loadtest_hint: payload_example is a minimal JSON request body as a string ("" for requests without a body), idempotent tells whether repeating the call is safe, notes is a short caveat ("" if none).

Base every answer on the given source; when the source is missing, infer from the route, names and dependencies and keep the summary generic. Return exactly one result per input id.

The user message starts with the repository context (languages, services and the inventory of the service the items belong to), then the items as JSON.`

const liteSystemPrompt = `You classify the business impact of code inventory items (HTTP endpoints, message consumers, scheduled jobs) found in a customer's repository by deterministic extractors. You do not discover new items; you interpret the ones given. Static rules rated these items low impact; confirm or correct that.

For every item return:
- criticality: "critical" (money movement, checkout/order placement, authentication, data loss), "high" (important user-facing flow or a consumer/job whose failure silently loses work), "medium" (state-changing but recoverable), "low" (read-only, internal, health checks).
- business_domain: one or two words (e.g. "payments", "orders", "identity", "catalog", "notifications", "internal").
- summary: one short sentence on what the item does, from the handler source.

Return exactly one result per input id. The user message starts with the repository context, then the items as JSON.`

// itemProperties are the full-tier result fields; the lite tier keeps liteFields.
var itemProperties = map[string]any{
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
}

var (
	fullFields = []string{"id", "criticality", "business_domain", "summary", "failure_modes", "suggested_monitors", "loadtest_hint"}
	liteFields = []string{"id", "criticality", "business_domain", "summary"}
)

func schemaFor(fields []string) map[string]any {
	props := map[string]any{}
	for _, f := range fields {
		props[f] = itemProperties[f]
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "properties": props, "required": fields, "additionalProperties": false,
		}}},
		"required":             []string{"items"},
		"additionalProperties": false,
	}
}

// tierSpec is the prompt, schema and output estimate of one tier.
type tierSpec struct {
	system         string // instructions + schema: identical for every request of the tier (cacheable prefix)
	responseFormat map[string]any
	outPerItem     int64
}

var tiers = func() map[string]tierSpec {
	m := map[string]tierSpec{}
	for name, def := range map[string]struct {
		prompt string
		fields []string
		out    int64
	}{tierFull: {systemPrompt, fullFields, OutputPerItem}, tierLite: {liteSystemPrompt, liteFields, LiteOutputPerItem}} {
		schema := schemaFor(def.fields)
		b, _ := json.Marshal(schema)
		// The schema is also in the prompt, so JSON output works on endpoints without response_format json_schema.
		m[name] = tierSpec{
			system:         def.prompt + "\n\nReply with exactly one JSON object (no Markdown, no prose) that matches this JSON schema:\n" + string(b),
			responseFormat: map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "inventory_classification_" + name, "strict": true, "schema": schema}},
			outPerItem:     def.out,
		}
	}
	return m
}()

type llmItem struct {
	ID                int                      `json:"id"`
	Criticality       string                   `json:"criticality"`
	BusinessDomain    string                   `json:"business_domain"`
	Summary           string                   `json:"summary"`
	FailureModes      []string                 `json:"failure_modes"`
	SuggestedMonitors []model.SuggestedMonitor `json:"suggested_monitors"`
	LoadtestHint      *struct {
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

// maxInventoryLines caps the inventory rows of one service in a request's context.
const maxInventoryLines = 300

// contextText is the repository context of a batch: languages, services and the inventory rows of the batch's service
// (the other services are listed by name only). Batches are grouped by service, so consecutive requests of a service
// share the same prefix; rows are sorted for a deterministic, cacheable text.
func contextText(r RepoSummary, service string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\nLanguages: %s\nServices: %s\n\nInventory of service %s (kind key):\n", r.Name,
		strings.Join(r.Languages, ", "), strings.Join(r.Services, ", "), service)
	var inv []string
	for _, l := range r.Inventory {
		kind, rest, _ := strings.Cut(l, " ")
		svc, key, _ := strings.Cut(rest, " ")
		if svc == service {
			inv = append(inv, kind+" "+key)
		}
	}
	sort.Strings(inv)
	if len(inv) > maxInventoryLines {
		inv = inv[:maxInventoryLines]
	}
	for _, l := range inv {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// maxTokens is the output cap of one request.
const maxTokens = 16000

// OutputPerItem is the expected output tokens per full-tier item used for the pre-call quota estimate (a batch of 15
// items answers with ~4–6k tokens; estimating with maxTokens would deny long before the limit).
const OutputPerItem = 600

// LiteOutputPerItem is the expected output tokens per lite-tier item (criticality, domain, one-sentence summary).
const LiteOutputPerItem = 80

// llmBatch is one planned request.
type llmBatch struct {
	tier    string
	service string
	items   []*model.Item
}

// plan groups items into requests: full tier first (items arrive most critical first), then lite; within a tier by
// service in order of first appearance, so the most critical services go first and share a context prefix.
func (l *LLM) plan(items []*model.Item) []llmBatch {
	var out []llmBatch
	for _, tier := range []string{tierFull, tierLite} {
		size := l.BatchSize
		if tier == tierLite {
			size = l.LiteBatchSize
		}
		size = max(size, 1)
		var order []string
		bySvc := map[string][]*model.Item{}
		for _, it := range items {
			if tierOf(it) != tier {
				continue
			}
			if _, ok := bySvc[it.Service]; !ok {
				order = append(order, it.Service)
			}
			bySvc[it.Service] = append(bySvc[it.Service], it)
		}
		for _, svc := range order {
			list := bySvc[svc]
			for start := 0; start < len(list); start += size {
				out = append(out, llmBatch{tier: tier, service: svc, items: list[start:min(start+size, len(list))]})
			}
		}
	}
	return out
}

// tierOf is lite for items the static rules rated low, full otherwise.
func tierOf(it *model.Item) string {
	if it.Criticality == model.Low {
		return tierLite
	}
	return tierFull
}

// Enrich fills it.LLM for items in batches (see plan); a failed batch is logged and skipped (static rules stay). A
// truncated or undecodable answer is retried once per half of the batch. Before every request the budget is asked; a
// denial leaves the remaining items static and reports quota_exceeded.
func (l *LLM) Enrich(ctx context.Context, repo RepoSummary, items []*model.Item, budget Budget) (Outcome, error) {
	out := Outcome{Status: model.LLMOK}
	var firstErr error
	batches := l.plan(items)
	for i, b := range batches {
		err := l.send(ctx, repo, b, budget, &out, true)
		if errors.Is(err, errQuota) {
			for _, rest := range batches[i+1:] {
				out.Skipped += len(rest.items)
			}
			l.Log.InfoContext(ctx, "llm enrichment stopped by AI quota", "reason", out.Reason, "skipped", out.Skipped)
			break
		}
		if err != nil {
			l.Log.WarnContext(ctx, "llm enrichment batch failed", "err", err, "items", len(b.items))
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

// errQuota stops the remaining requests of an analysis (out.Status / Reason / Skipped are set).
var errQuota = errors.New("ai quota exceeded")

// errSplittable marks answers worth retrying with fewer items (output truncated, JSON undecodable).
var errSplittable = errors.New("retry with a smaller batch")

// send reserves budget, sends one request and, when the answer was truncated or undecodable and split is set, sends
// each half of the batch once more.
func (l *LLM) send(ctx context.Context, repo RepoSummary, b llmBatch, budget Budget, out *Outcome, split bool) error {
	spec := tiers[b.tier]
	user := contextText(repo, b.service) + "\n" + l.payload(b.items)
	if budget != nil {
		chars := len(spec.system) + len(user)
		if ok, reason := budget.Reserve(ctx, l.model, int64((chars+3)/4), min(int64(maxTokens), spec.outPerItem*int64(len(b.items)))); !ok {
			out.Status, out.Reason, out.Skipped = model.LLMQuotaExceeded, reason, out.Skipped+len(b.items)
			return errQuota
		}
	}
	usage, err := l.batch(ctx, spec, user, b.items)
	if usage != nil && budget != nil {
		out.Credits += budget.Record(ctx, l.model, *usage)
	}
	if err == nil || !errors.Is(err, errSplittable) || !split || len(b.items) < 2 {
		return err
	}
	l.Log.InfoContext(ctx, "llm batch retried in halves", "err", err, "items", len(b.items))
	half := len(b.items) / 2
	var firstErr error
	for _, part := range [][]*model.Item{b.items[:half], b.items[half:]} {
		perr := l.send(ctx, repo, llmBatch{tier: b.tier, service: b.service, items: part}, budget, out, false)
		if errors.Is(perr, errQuota) {
			return perr
		}
		if perr != nil && firstErr == nil {
			firstErr = perr
		}
	}
	return firstErr
}

// payload is the user message of one batch.
func (l *LLM) payload(batch []*model.Item) string {
	in := make([]llmInput, len(batch))
	for i, it := range batch {
		code := compactCode(it.Snippet, it.Language)
		if len(code) > l.MaxChars {
			code = code[:l.MaxChars] + "\n…(truncated)"
		}
		in[i] = llmInput{ID: i, Kind: it.Kind, Service: it.Service, Key: it.Key, Source: it.Source, Framework: it.Framework,
			File: it.File, Handler: it.Handler, Code: code, Callees: it.Callees}
		if s := it.Stats; s != nil {
			in[i].Traffic = fmt.Sprintf("%.1f rpm, error rate %.2f%%, p95 %.0f ms (7d)", s.RPM, s.ErrorRate*100, s.P95)
		}
	}
	b, _ := json.Marshal(in) // compact: indentation would be ~10% of the input tokens
	return "Classify these inventory items:\n" + string(b)
}

// batch sends one request. usage is non-nil whenever the API answered (billed), even when the answer is unusable.
func (l *LLM) batch(ctx context.Context, spec tierSpec, user string, batch []*model.Item) (*Tokens, error) {
	req := llmapi.Request{MaxTokens: maxTokens, Messages: []llmapi.Message{
		{Role: "system", Content: spec.system},
		{Role: "user", Content: user},
	}}
	if !l.noSchema.Load() {
		req.ResponseFormat = spec.responseFormat
	}
	resp, err := l.client.Complete(ctx, req)
	var se *llmapi.StatusError
	if err != nil && req.ResponseFormat != nil && errors.As(err, &se) && se.Code == http.StatusBadRequest {
		// The endpoint does not support json_schema output: rely on the schema in the system prompt from now on.
		l.noSchema.Store(true)
		l.Log.InfoContext(ctx, "llm endpoint rejected response_format json_schema; using prompt-only JSON", "err", err)
		req.ResponseFormat = nil
		resp, err = l.client.Complete(ctx, req)
	}
	var usage *Tokens
	if err == nil || errors.Is(err, llmapi.ErrEmpty) { // answered → billed
		u := resp.Usage
		usage = &Tokens{Input: u.Prompt - u.Cached, CacheRead: u.Cached, Output: u.Completion}
	}
	if err != nil {
		return usage, fmt.Errorf("llm: %w", err)
	}
	switch resp.FinishReason {
	case "content_filter":
		return usage, errors.New("llm: request refused (content_filter)")
	case "length":
		return usage, fmt.Errorf("llm: output truncated (max_tokens): %w", errSplittable)
	}
	var out struct {
		Items []llmItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(jsonObject(resp.Content)), &out); err != nil {
		return usage, fmt.Errorf("llm: decode JSON output: %w (%w)", err, errSplittable)
	}
	l.Log.DebugContext(ctx, "llm batch", "items", len(batch), "prompt_tokens", resp.Usage.Prompt, "cached_tokens", resp.Usage.Cached)
	for _, r := range out.Items {
		if r.ID < 0 || r.ID >= len(batch) {
			continue
		}
		res := &model.LLMResult{Criticality: r.Criticality, BusinessDomain: r.BusinessDomain, Summary: r.Summary,
			FailureModes: r.FailureModes, SuggestedMonitors: r.SuggestedMonitors, Model: l.model}
		if r.LoadtestHint == nil { // lite tier
			Apply(batch[r.ID], res)
			continue
		}
		res.LoadtestHint = &model.LoadtestHint{Idempotent: r.LoadtestHint.Idempotent, Notes: r.LoadtestHint.Notes}
		if p := strings.TrimSpace(r.LoadtestHint.PayloadExample); p != "" && json.Valid([]byte(p)) {
			res.LoadtestHint.PayloadExample = json.RawMessage(p)
		}
		Apply(batch[r.ID], res)
	}
	return usage, nil
}

// jsonObject returns the JSON object in a model answer: Markdown code fences and text around the outermost braces
// are dropped (models without json_schema support sometimes add them).
func jsonObject(s string) string {
	s = strings.TrimSpace(s)
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return s
	}
	return s[i : j+1]
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

// compactCode shrinks a handler snippet before it is sent: trailing whitespace, blank lines and the common indentation
// go, and a comment run of 3+ lines (license headers, long doc blocks) keeps its first 2 lines plus "…". `#` starts a
// comment only in Python (it is a private field / preprocessor line elsewhere).
func compactCode(src, lang string) string {
	var kept []string
	prefix, first := "", true
	for _, l := range strings.Split(src, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if l == "" {
			continue
		}
		ind := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if first {
			prefix, first = ind, false
		}
		for !strings.HasPrefix(ind, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
		kept = append(kept, l)
	}
	var b strings.Builder
	run, inBlock := 0, false
	for _, l := range kept {
		l = strings.TrimPrefix(l, prefix)
		t := strings.TrimSpace(l)
		comment := inBlock || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || (lang == "python" && strings.HasPrefix(t, "#"))
		if inBlock || strings.HasPrefix(t, "/*") {
			inBlock = !strings.Contains(t, "*/")
		}
		if !comment {
			run = 0
		} else if run++; run >= 3 {
			if run == 3 {
				b.WriteString(l[:len(l)-len(strings.TrimLeft(l, " \t"))] + "…\n")
			}
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}
