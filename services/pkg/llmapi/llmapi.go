// Package llmapi is the single OpenAI-compatible Chat Completions endpoint every LLM call of the platform goes
// through (ADR-034): TT_ASSISTANT_API_URL, TT_ASSISTANT_API_KEY and TT_ASSISTANT_MODEL, Meta Muse Spark by default.
// The dashboard assistant, the api's incident summary and codeintel enrichment all read their endpoint from here.
// Plain net/http, no vendor SDK; model ids are never hard-coded.
package llmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultURL is the base URL used when TT_ASSISTANT_API_URL is empty (Meta Muse Spark, OpenAI-compatible).
const DefaultURL = "https://api.meta.ai/v1"

// Environment variables of the endpoint.
const (
	EnvURL   = "TT_ASSISTANT_API_URL"
	EnvKey   = "TT_ASSISTANT_API_KEY"
	EnvModel = "TT_ASSISTANT_MODEL"
)

// Config is the endpoint. URL is the full chat completions URL.
type Config struct {
	URL   string
	Key   string
	Model string
}

// New normalizes base (".../v1", the full ".../chat/completions" URL, or empty = DefaultURL), key and model.
func New(base, key, model string) Config {
	return Config{URL: ChatCompletionsURL(base), Key: strings.TrimSpace(key), Model: strings.TrimSpace(model)}
}

// FromEnv reads TT_ASSISTANT_API_URL, TT_ASSISTANT_API_KEY and TT_ASSISTANT_MODEL.
func FromEnv() Config { return New(os.Getenv(EnvURL), os.Getenv(EnvKey), os.Getenv(EnvModel)) }

// Enabled reports whether key and model are both set; otherwise callers use their offline fallback.
func (c Config) Enabled() bool { return c.Key != "" && c.Model != "" }

// Provider is the gen_ai.provider.name of the endpoint.
func (c Config) Provider() string { return Provider(c.URL) }

// Host is the endpoint's host[:port] (for llm.WithProvider).
func (c Config) Host() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// ChatCompletionsURL turns a base URL into the chat completions endpoint.
func ChatCompletionsURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultURL
	}
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// Provider maps an endpoint URL to its gen_ai.provider.name (also the llmprice family where one exists). Unknown
// hosts speak the OpenAI dialect and are reported as "openai".
func Provider(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "openai"
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case h == "api.meta.ai" || strings.HasSuffix(h, ".meta.ai") || strings.HasSuffix(h, ".llama.com"):
		return "meta"
	case h == "baseten.co" || strings.HasSuffix(h, ".baseten.co"):
		return "baseten"
	case h == "openrouter.ai":
		return "openrouter"
	case h == "api.groq.com":
		return "groq"
	case h == "api.together.xyz":
		return "together_ai"
	case h == "api.fireworks.ai":
		return "fireworks_ai"
	}
	return "openai"
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// Request is one non-streaming chat completions request. Model empty = Config.Model.
type Request struct {
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	MaxTokens      int64     `json:"max_tokens,omitempty"`
	ResponseFormat any       `json:"response_format,omitempty"`
	Stream         bool      `json:"stream"`
}

// Usage is the token usage of one response. Prompt includes Cached (OpenAI semantics).
type Usage struct {
	Prompt, Completion, Cached int64
}

// Response is the first choice of a completion.
type Response struct {
	Content      string
	FinishReason string // stop | length | content_filter | ...
	Usage        Usage
}

// StatusError is a non-200 answer.
type StatusError struct {
	Code int
	Body string // first bytes of the body
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("chat completions: HTTP %d", e.Code)
	}
	return fmt.Sprintf("chat completions: HTTP %d: %s", e.Code, e.Body)
}

// Retryable reports whether the request may be repeated (rate limit, server error).
func (e *StatusError) Retryable() bool { return e.Code == http.StatusTooManyRequests || e.Code >= 500 }

// ErrEmpty is returned when the response has no choices.
var ErrEmpty = errors.New("chat completions: no choices")

// Client sends requests to one endpoint.
type Client struct {
	Config
	HTTP    *http.Client  // nil = http.DefaultClient
	Retries int           // extra attempts after a network error, 429 or 5xx
	Backoff time.Duration // first retry delay, doubled per attempt (0 = 1s)
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Complete sends req (non-streaming) and returns the first choice.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		req.Model = c.Model
	}
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	delay := c.Backoff
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, body)
		if err == nil || attempt >= c.Retries || ctx.Err() != nil || !retryable(err) {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Retryable()
	}
	var de *decodeError
	return !errors.As(err, &de) && !errors.Is(err, ErrEmpty)
}

type decodeError struct{ err error }

func (e *decodeError) Error() string { return "chat completions: decode response: " + e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }

func (c *Client) do(ctx context.Context, body []byte) (Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.Key)
	hreq.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return Response{}, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var w wireResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&w); err != nil {
		return Response{}, &decodeError{err}
	}
	u := Usage{Prompt: w.Usage.PromptTokens, Completion: w.Usage.CompletionTokens}
	if d := w.Usage.PromptTokensDetails; d != nil {
		u.Cached = min(d.CachedTokens, u.Prompt)
	}
	if len(w.Choices) == 0 {
		return Response{Usage: u}, ErrEmpty
	}
	return Response{Content: w.Choices[0].Message.Content, FinishReason: w.Choices[0].FinishReason, Usage: u}, nil
}
