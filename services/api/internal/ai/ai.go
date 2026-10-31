// Package ai isolates paid model calls from the API and defaults to an offline mock.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Input struct {
	Title    string   `json:"title"`
	Evidence []string `json:"evidence"`
}

// IncidentSummaryMaxInputBytes caps the entire user-controlled prompt before a
// paid request is made. The output cap is sent to the provider in max_tokens.
const IncidentSummaryMaxInputBytes = 4000
const IncidentSummaryMaxOutputTokens = 150

var ErrInputTooLarge = errors.New("incident summary input exceeds 4000 bytes")

func ValidateInput(in Input) error {
	if len(in.Title)+len(strings.Join(in.Evidence, "\n- ")) > IncidentSummaryMaxInputBytes {
		return ErrInputTooLarge
	}
	return nil
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
}

type mock struct{}

func NewMock() Summarizer { return mock{} }

func (mock) Summarize(_ context.Context, in Input) (Result, error) {
	return Result{
		Summary:  fmt.Sprintf("[Mock AI draft] %s. Review the supplied evidence and verify the cause before acting.", strings.TrimSpace(in.Title)),
		Provider: "mock",
	}, nil
}

type remote struct {
	provider string
	endpoint string
	model    string
	key      string
	client   *http.Client
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// NewFromEnv is deliberately fail-closed: paid network calls require both an
// explicit provider and TT_AI_ALLOW_PAID=true. A key alone cannot enable them.
func NewFromEnv() (Summarizer, error) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("TT_AI_PROVIDER")))
	if provider == "" || provider == "mock" {
		return mock{}, nil
	}
	if provider != "meta" && provider != "baseten" {
		return nil, fmt.Errorf("invalid TT_AI_PROVIDER %q", provider)
	}
	if os.Getenv("TT_AI_ALLOW_PAID") != "true" {
		return nil, errors.New("paid AI provider requires TT_AI_ALLOW_PAID=true")
	}
	var endpoint, model, key string
	switch provider {
	case "meta":
		endpoint, model, key = "https://api.meta.ai/v1/chat/completions", "muse-spark-1.3", os.Getenv("MODEL_API_KEY")
	case "baseten":
		endpoint, model, key = "https://inference.baseten.co/v1/chat/completions", "meta-models/muse-spark-open", os.Getenv("BASETEN_API_KEY")
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("%s API key is required for paid AI provider", provider)
	}
	if v := strings.TrimSpace(os.Getenv("TT_AI_MODEL")); v != "" {
		model = v
	}
	return &remote{provider: provider, endpoint: endpoint, model: model, key: key,
		client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (p *remote) Summarize(ctx context.Context, in Input) (Result, error) {
	if err := ValidateInput(in); err != nil {
		return Result{}, err
	}
	payload := struct {
		Model     string    `json:"model"`
		Messages  []message `json:"messages"`
		MaxTokens int       `json:"max_tokens"`
	}{Model: p.model, MaxTokens: IncidentSummaryMaxOutputTokens}
	payload.Messages = append(payload.Messages,
		message{"developer", "Write a concise incident summary using only the evidence given. State uncertainty. Do not invent a root cause. Reply in plain text, at most 100 words."},
		message{"user", "Incident: " + in.Title + "\nEvidence:\n- " + strings.Join(in.Evidence, "\n- ")})
	data, err := json.Marshal(payload)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(data))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("%s API returned HTTP %d", p.provider, resp.StatusCode)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Result{}, fmt.Errorf("decode %s response: %w", p.provider, err)
	}
	if len(body.Choices) == 0 || strings.TrimSpace(body.Choices[0].Message.Content) == "" {
		return Result{}, fmt.Errorf("%s API returned no summary", p.provider)
	}
	return Result{Summary: strings.TrimSpace(body.Choices[0].Message.Content), Provider: p.provider,
		Usage: Usage{InputTokens: body.Usage.PromptTokens, OutputTokens: body.Usage.CompletionTokens}}, nil
}
