// Package symbolicate calls the symbolicator service (.ai/contracts.md §12): POST {base}/v1/symbolicate
// { app, release, platform, stack } → { stack, frames, symbolicated }. Every failure falls back to the raw stack.
package symbolicate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// Timeout is the per-call budget (contract: 2 s).
const Timeout = 2 * time.Second

// Request is the symbolicator request body.
type Request struct {
	App      string `json:"app"`
	Release  string `json:"release"`
	Platform string `json:"platform"`
	Stack    string `json:"stack"`
}

// Result is the symbolicator response.
type Result struct {
	Stack        string        `json:"stack"`
	Frames       []model.Frame `json:"frames"`
	Symbolicated *bool         `json:"symbolicated,omitempty"`
}

// Internal credential headers (contracts §2), sent when the caller has no bearer key (dashboard cookie session).
const (
	HeaderInternal = "X-Trooper-Internal"
	HeaderProject  = "X-Trooper-Project"
)

// Client talks to the symbolicator. A nil *Client or an empty BaseURL disables symbolication.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// InternalToken (TT_INTERNAL_TOKEN) authenticates calls without a caller bearer key as
	// x-trooper-internal + x-trooper-project (the request's tenant). Empty = forward the caller's header only.
	InternalToken string
}

// New returns a client for base ("" = disabled → nil).
func New(base string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}
	return &Client{BaseURL: base, HTTP: &http.Client{Timeout: Timeout}}
}

// Symbolicate resolves stack. A bearer authorization is forwarded as is (the caller's "Bearer <api key>");
// without one (cookie session) the internal credential is used with the project of the tenant in ctx.
// ok=false means the raw stack should be shown (service down, timeout, no artifacts, empty stack).
func (c *Client) Symbolicate(ctx context.Context, authorization string, req Request) (stack string, frames []model.Frame, ok bool, err error) {
	if c == nil || c.BaseURL == "" || strings.TrimSpace(req.Stack) == "" {
		return req.Stack, nil, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/symbolicate", bytes.NewReader(body))
	if err != nil {
		return req.Stack, nil, false, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if isBearer(authorization) {
		hr.Header.Set("Authorization", authorization)
	} else if c.InternalToken != "" {
		t, ok := tenant.From(ctx)
		if !ok || t.ProjectID == 0 {
			return req.Stack, nil, false, fmt.Errorf("symbolicator: no tenant for the internal credential")
		}
		hr.Header.Set(HeaderInternal, c.InternalToken)
		hr.Header.Set(HeaderProject, strconv.FormatUint(uint64(t.ProjectID), 10))
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: Timeout}
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return req.Stack, nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return req.Stack, nil, false, fmt.Errorf("symbolicator: HTTP %d", resp.StatusCode)
	}
	var r Result
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return req.Stack, nil, false, fmt.Errorf("symbolicator: %w", err)
	}
	sym := len(r.Frames) > 0
	if r.Symbolicated != nil {
		sym = *r.Symbolicated
	}
	if r.Stack == "" {
		r.Stack = req.Stack
	}
	if r.Frames == nil {
		r.Frames = []model.Frame{}
	}
	return r.Stack, r.Frames, sym, nil
}

func isBearer(h string) bool {
	return len(h) > 7 && strings.EqualFold(h[:7], "bearer ") && strings.TrimSpace(h[7:]) != ""
}
