// Package ai is the incident-summary model call. It uses the one OpenAI-compatible endpoint of the platform
// (services/pkg/llmapi, ADR-034) and defaults to an offline mock.
package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/llmapi"
)

type Input struct {
	Title    string   `json:"title"`
	Evidence []string `json:"evidence"`
}

// IncidentSummaryMaxInputBytes caps the entire user-controlled prompt before a
// paid request is made. The output cap is sent to the provider in max_tokens.
const IncidentSummaryMaxInputBytes = 4000
const IncidentSummaryMaxOutputTokens = 150

// IncidentSummarySystemPrompt is the instruction sent with every summary request.
const IncidentSummarySystemPrompt = "Write a concise incident summary using only the evidence given. State uncertainty. Do not invent a root cause. Reply in plain text, at most 100 words."

// HTTPTransport is the transport of model clients built by NewFromEnv (nil = http.DefaultTransport; main sets a
// GenAI-instrumented one when self telemetry is on, ADR-027).
var HTTPTransport http.RoundTripper

var ErrInputTooLarge = errors.New("incident summary input exceeds 4000 bytes")

func ValidateInput(in Input) error {
	if len(in.Title)+len(strings.Join(in.Evidence, "\n- ")) > IncidentSummaryMaxInputBytes {
		return ErrInputTooLarge
	}
	return nil
}

// PromptChars is the size of the prompt sent for in (pre-call AI quota estimate).
func PromptChars(in Input) int {
	return len(IncidentSummarySystemPrompt) + len(userPrompt(in))
}

func userPrompt(in Input) string {
	return "Incident: " + in.Title + "\nEvidence:\n- " + strings.Join(in.Evidence, "\n- ")
}

type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

type Result struct {
	Summary  string `json:"summary"`
	Provider string `json:"provider"`
	Usage    Usage  `json:"usage"`
}

type Summarizer interface {
	Summarize(context.Context, Input) (Result, error)
	// Model is the model id of a real provider; "" for the offline mock (no AI credits involved).
	Model() string
}

type mock struct{}

func NewMock() Summarizer { return mock{} }

func (mock) Model() string { return "" }

func (mock) Summarize(_ context.Context, in Input) (Result, error) {
	return Result{
		Summary:  fmt.Sprintf("[Mock AI draft] %s. Review the supplied evidence and verify the cause before acting.", strings.TrimSpace(in.Title)),
		Provider: "mock",
	}, nil
}

type remote struct {
	client *llmapi.Client
}

// New returns the summarizer of the shared OpenAI-compatible endpoint (ADR-034), or the offline mock when its key or
// model is empty. hc nil = a client with a 15 second timeout over HTTPTransport. There is no retry: one summary costs
// at most one request.
func New(cfg llmapi.Config, hc *http.Client) Summarizer {
	if !cfg.Enabled() {
		return mock{}
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second, Transport: HTTPTransport}
	}
	return &remote{client: &llmapi.Client{Config: cfg, HTTP: hc}}
}

// NewFromEnv reads TT_ASSISTANT_API_URL / TT_ASSISTANT_API_KEY / TT_ASSISTANT_MODEL, the endpoint shared with the
// dashboard assistant and codeintel: key and model set → real model, otherwise the offline mock.
func NewFromEnv() Summarizer { return New(llmapi.FromEnv(), nil) }

func (p *remote) Model() string { return p.client.Model }

// Summarize returns the usage of a billed request even when the answer is unusable (the caller records it).
func (p *remote) Summarize(ctx context.Context, in Input) (Result, error) {
	if err := ValidateInput(in); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	resp, err := p.client.Complete(ctx, llmapi.Request{MaxTokens: IncidentSummaryMaxOutputTokens, Messages: []llmapi.Message{
		{Role: "system", Content: IncidentSummarySystemPrompt},
		{Role: "user", Content: userPrompt(in)},
	}})
	provider := p.client.Provider()
	usage := Usage{InputTokens: int(resp.Usage.Prompt), OutputTokens: int(resp.Usage.Completion)}
	if err != nil {
		return Result{Usage: usage}, fmt.Errorf("%s: %w", provider, err)
	}
	if strings.TrimSpace(resp.Content) == "" {
		return Result{Usage: usage}, fmt.Errorf("%s API returned no summary", provider)
	}
	return Result{Summary: strings.TrimSpace(resp.Content), Provider: provider, Usage: usage}, nil
}
