// Package httpx is the small REST client the Neon, Vercel and Google providers share: JSON, bearer auth,
// timeouts, and retries on 429/5xx honouring Retry-After. Secrets never appear in errors (only method + path).
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client calls one REST API.
type Client struct {
	Base string
	HTTP *http.Client
	// Auth sets credentials on each request (bearer token, OAuth2 token, ...). May be nil.
	Auth func(*http.Request) error
	// MaxAttempts per request (default 4).
	MaxAttempts int
	// BaseDelay of the exponential backoff (default 1s).
	BaseDelay time.Duration
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Method, Path string
	Code         int
	Body         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Code, e.Body)
}

// IsStatus reports whether err is a StatusError with code.
func IsStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

// Bearer returns an Auth func setting `Authorization: Bearer <token>`.
func Bearer(token string) func(*http.Request) error {
	return func(r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

// Get decodes GET base+path?query into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.Do(ctx, http.MethodGet, path, query, nil, out)
}

// Do sends a JSON request and decodes the JSON answer into out (when non-nil).
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	attempts := c.MaxAttempts
	if attempts <= 0 {
		attempts = 4
	}
	delay := c.BaseDelay
	if delay <= 0 {
		delay = time.Second
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := sleep(ctx, last, delay<<(i-1)); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.Auth != nil {
			if err := c.Auth(req); err != nil {
				return fmt.Errorf("%s %s: auth: %w", method, path, err)
			}
		}
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = fmt.Errorf("%s %s: %w", method, path, stripURL(err))
			continue
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			last = &retryable{err: &StatusError{method, path, resp.StatusCode, trunc(b)}, after: retryAfter(resp.Header.Get("Retry-After"))}
			continue
		}
		if resp.StatusCode >= 300 {
			return &StatusError{method, path, resp.StatusCode, trunc(b)}
		}
		if rerr != nil {
			return fmt.Errorf("%s %s: %w", method, path, rerr)
		}
		if out != nil && len(b) > 0 {
			if err := json.Unmarshal(b, out); err != nil {
				return fmt.Errorf("%s %s: decode: %w", method, path, err)
			}
		}
		return nil
	}
	var r *retryable
	if errors.As(last, &r) {
		return r.err
	}
	return last
}

type retryable struct {
	err   error
	after time.Duration
}

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

func sleep(ctx context.Context, last error, backoff time.Duration) error {
	d := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
	var r *retryable
	if errors.As(last, &r) && r.after > 0 {
		d = r.after
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// stripURL drops the URL (which may carry query parameters) from transport errors.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s", ue.Err)
	}
	return err
}

func trunc(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}
