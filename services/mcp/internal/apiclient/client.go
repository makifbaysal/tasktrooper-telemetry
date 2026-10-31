// Package apiclient is the single HTTP client every tool uses to reach
// services/api (ADR-025, contracts §26). It is read-only: only GET.
package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client wraps services/api with one shared *http.Client. Every call is a GET,
// so there are no non-idempotent requests to guard with retries; a request is
// issued once and its error is mapped, never retried.
type Client struct {
	base string
	key  string
	hc   *http.Client
}

// New returns a Client for baseURL authenticated with key.
func New(baseURL, key string, timeout time.Duration) *Client {
	return &Client{
		base: baseURL,
		key:  key,
		hc:   &http.Client{Timeout: timeout},
	}
}

// ToolError is a short, actionable message safe to hand to an LLM. It never
// carries a stack trace, a secret, or a raw upstream body.
type ToolError struct{ msg string }

func (e *ToolError) Error() string { return e.msg }

func toolErrf(format string, args ...any) error { return &ToolError{msg: fmt.Sprintf(format, args...)} }

// Get performs an authenticated GET on path with query q, decodes the JSON
// response into out, and maps upstream failures to ToolError.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return toolErrf("could not build request for %s", path)
	}
	// The key goes only on the wire, never into a message or a log.
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")

	res, err := c.hc.Do(req)
	if err != nil {
		if isTimeout(err) {
			return toolErrf("the API is unavailable (timeout after %s); retry later", c.hc.Timeout)
		}
		return toolErrf("the API is unavailable (%s); check TT_API_URL", brief(err))
	}
	defer drain(res)

	switch {
	case res.StatusCode == http.StatusOK:
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return toolErrf("the API returned an unreadable response for %s", path)
		}
		return nil
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden:
		return toolErrf("the API rejected the key (HTTP %d): check TT_API_KEY", res.StatusCode)
	case res.StatusCode == http.StatusNotFound:
		// hint names the fix for the common case (a stale endpoint id).
		return toolErrf("not found in this project (HTTP 404 on %s); %s", path, NotFoundHint)
	default:
		return toolErrf("the API is unavailable (HTTP %d on %s)", res.StatusCode, path)
	}
}

// NotFoundHint is appended to 404 errors. Endpoint ids are slugs the agent can
// only obtain from tt_list_endpoints, so a stale id is the usual cause.
const NotFoundHint = "call tt_list_endpoints for valid endpoint ids"

// drain discards the rest of a response body so the connection can be reused.
func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	_ = res.Body.Close()
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "timeout")
}

// brief strips a URL and any credentials from an error string so it can be
// shown to an agent without leaking the endpoint or key.
func brief(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, "://"); i >= 0 {
		if j := strings.IndexAny(s[i:], "/?"); j >= 0 {
			s = s[:i] + s[i+j:]
		}
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Check verifies at startup that the key works and, when the optional ids are
// set, that it resolves to the expected org and project (ADR-025). A bad key or
// an unreachable API fails here with a clear message instead of surfacing as a
// confusing per-tool error later. The key is never included in an error or log.
func (c *Client) Check(ctx context.Context, log *slog.Logger, wantOrg, wantProject uint32) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/me", nil)
	if err != nil {
		return fmt.Errorf("build /me request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)

	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("API unreachable at %s (is `make infra` + api running?): %w", c.base, err)
	}
	defer drain(res)

	switch {
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("API rejected the key (HTTP %d): check TT_API_KEY", res.StatusCode)
	case res.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("GET /me failed (HTTP %d): %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var me meResponse
	if err := json.NewDecoder(res.Body).Decode(&me); err != nil {
		return fmt.Errorf("decode /me: %w", err)
	}
	if wantProject != 0 && me.CurrentProjectID != strconv.FormatUint(uint64(wantProject), 10) {
		return fmt.Errorf("TT_PROJECT_ID=%d but the key resolves to project %s; fix TT_API_KEY or TT_PROJECT_ID",
			wantProject, me.CurrentProjectID)
	}
	if wantOrg != 0 && !orgListed(me.Orgs, wantOrg) {
		return fmt.Errorf("TT_ORG_ID=%d but the key's org is not %d; fix TT_API_KEY or TT_ORG_ID", wantOrg, wantOrg)
	}
	log.Debug("api key verified", "project", me.CurrentProjectID)
	return nil
}

// meResponse is the subset of GET /api/v1/me used for the cross-check.
type meResponse struct {
	CurrentProjectID string  `json:"currentProjectId"`
	Orgs             []meOrg `json:"orgs"`
}

type meOrg struct {
	ID string `json:"id"`
}

func orgListed(orgs []meOrg, want uint32) bool {
	wantID := strconv.FormatUint(uint64(want), 10)
	for _, o := range orgs {
		if o.ID == wantID {
			return true
		}
	}
	return false
}

// ---- query building ----

// Range returns the range query value, defaulting to "1h".
func Range(r string) string {
	if r == "" {
		return "1h"
	}
	return r
}

// ClampInt returns n bounded to [min, max].
func ClampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// PutIf set q[k]=v when v is non-empty.
func PutIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

// PutBool sets q[k]="true" when b.
func PutBool(q url.Values, k string, b bool) {
	if b {
		q.Set(k, "true")
	}
}

// PutInt sets q[k] when the pointer is non-nil.
func PutInt(q url.Values, k string, n *int) {
	if n != nil {
		q.Set(k, strconv.Itoa(*n))
	}
}
