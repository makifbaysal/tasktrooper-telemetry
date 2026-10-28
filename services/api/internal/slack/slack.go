// Package slack is the api's minimal Slack Web API client (ADR-017, contracts §17): OAuth v2 code exchange,
// channel listing, user lookup, interactivity responses and request signature verification.
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is the Slack Web API root.
const DefaultAPIURL = "https://slack.com/api"

// Scopes are the bot scopes the app requests: posting (also to public channels it was not invited to), channel
// pickers, channels:join, and users:read(.email) (users:read is required by Slack for .email) to map a button press to a TaskTrooper user by email.
const Scopes = "chat:write,chat:write.public,channels:read,groups:read,channels:join,users:read,users:read.email"

// Client calls the Slack Web API.
type Client struct {
	BaseURL string // default DefaultAPIURL
	HTTP    *http.Client
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultAPIURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) hc() *http.Client {
	if c.HTTP == nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	return c.HTTP
}

// APIError is a Slack {"ok": false, "error": "..."} response.
type APIError struct{ Code string }

func (e *APIError) Error() string { return "slack: " + e.Code }

func (c *Client) call(ctx context.Context, method, token string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusTooManyRequests {
		return &APIError{Code: "ratelimited"}
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack %s: http %d", method, resp.StatusCode)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	if !env.OK {
		return &APIError{Code: env.Error}
	}
	return json.Unmarshal(body, out)
}

// OAuthResult is what oauth.v2.access returns for a bot installation.
type OAuthResult struct {
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
	BotUserID   string `json:"bot_user_id"`
	Team        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
}

// OAuthAccess exchanges an OAuth v2 code for the bot token.
func (c *Client) OAuthAccess(ctx context.Context, clientID, clientSecret, code, redirectURI string) (OAuthResult, error) {
	var out OAuthResult
	err := c.call(ctx, "oauth.v2.access", "", url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code},
		"redirect_uri": {redirectURI}}, &out)
	if err == nil && out.AccessToken == "" {
		err = errors.New("slack: oauth.v2.access returned no access token")
	}
	return out, err
}

// Channel is a conversation the bot can see.
type Channel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
	IsMember  bool   `json:"is_member"`
}

// Channels lists public and private (where the bot is a member) channels, all pages, archived excluded.
func (c *Client) Channels(ctx context.Context, token string) ([]Channel, error) {
	var out []Channel
	cursor := ""
	for page := 0; page < 100; page++ {
		var res struct {
			Channels []Channel `json:"channels"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		form := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"1000"}}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		if err := c.call(ctx, "conversations.list", token, form, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Channels...)
		if cursor = res.Meta.NextCursor; cursor == "" {
			break
		}
	}
	return out, nil
}

// UserEmail returns a Slack user's email (needs users:read.email).
func (c *Client) UserEmail(ctx context.Context, token, userID string) (string, error) {
	var res struct {
		User struct {
			Profile struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, "users.info", token, url.Values{"user": {userID}}, &res); err != nil {
		return "", err
	}
	return res.User.Profile.Email, nil
}

// Respond posts an ephemeral reply to an interaction's response_url (only the clicking user sees it).
func (c *Client) Respond(ctx context.Context, responseURL, text string) error {
	if responseURL == "" {
		return nil
	}
	if u, err := url.Parse(responseURL); err != nil || (u.Scheme != "https" && !strings.HasPrefix(u.Host, "127.0.0.1")) {
		return errors.New("slack: unexpected response_url")
	}
	b, _ := json.Marshal(map[string]any{"response_type": "ephemeral", "replace_original": false, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ErrBadSignature is returned for a missing, stale or wrong X-Slack-Signature.
var ErrBadSignature = errors.New("slack: bad request signature")

// Verify checks X-Slack-Signature ("v0=" + hex HMAC-SHA256 of "v0:<ts>:<body>") and that the timestamp is within
// five minutes of now (replay protection).
func Verify(signingSecret string, ts, sig string, body []byte, now time.Time) error {
	if signingSecret == "" || ts == "" || sig == "" {
		return ErrBadSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return ErrBadSignature
	}
	if !hmac.Equal([]byte(Sign(signingSecret, ts, body)), []byte(sig)) {
		return ErrBadSignature
	}
	return nil
}

// Sign returns the X-Slack-Signature value for body at ts (tests, fixtures).
func Sign(signingSecret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(signingSecret))
	m.Write([]byte("v0:" + ts + ":"))
	m.Write(body)
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}
