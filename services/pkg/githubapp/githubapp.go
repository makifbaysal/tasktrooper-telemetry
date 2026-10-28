// Package githubapp is the shared GitHub App client (ADR-017, contracts §18): app JWT (RS256, stdlib only),
// installation access tokens with a cache, installation repository listing (paginated) and webhook signature
// verification. Used by api (install flow, repo picker) and codeintel (clone, webhooks).
//
// Configuration (FromEnv): TT_GITHUB_APP_ID, TT_GITHUB_APP_PRIVATE_KEY (a PEM, or a credentials reference
// env:NAME / file:/path), TT_GITHUB_API_URL (default https://api.github.com; GitHub Enterprise: https://host/api/v3).
package githubapp

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/secrets"
)

// DefaultAPIURL is the public GitHub REST API.
const DefaultAPIURL = "https://api.github.com"

// ErrNotConfigured is returned by FromEnv when TT_GITHUB_APP_ID or the private key is missing.
var ErrNotConfigured = errors.New("githubapp: TT_GITHUB_APP_ID / TT_GITHUB_APP_PRIVATE_KEY not set")

// Client talks to the GitHub REST API as a GitHub App. Safe for concurrent use.
type Client struct {
	AppID   int64
	Key     *rsa.PrivateKey
	BaseURL string       // REST API root, no trailing slash
	HTTP    *http.Client // default: 15 s timeout
	Now     func() time.Time

	mu     sync.Mutex
	tokens map[int64]Token
}

// Token is an installation access token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// New builds a client from an app id and a PEM private key (PKCS#1 or PKCS#8).
func New(appID int64, pemKey []byte, baseURL string) (*Client, error) {
	key, err := ParseKey(pemKey)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		baseURL = DefaultAPIURL
	}
	return &Client{AppID: appID, Key: key, BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 15 * time.Second},
		Now: time.Now, tokens: map[int64]Token{}}, nil
}

// FromEnv builds a client from TT_GITHUB_APP_ID / TT_GITHUB_APP_PRIVATE_KEY / TT_GITHUB_API_URL.
// Returns ErrNotConfigured when the app is not configured.
func FromEnv(ctx context.Context) (*Client, error) {
	idS, keyRef := os.Getenv("TT_GITHUB_APP_ID"), os.Getenv("TT_GITHUB_APP_PRIVATE_KEY")
	if idS == "" || keyRef == "" {
		return nil, ErrNotConfigured
	}
	id, err := strconv.ParseInt(idS, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("githubapp: TT_GITHUB_APP_ID: %w", err)
	}
	pemKey := []byte(keyRef)
	if !strings.Contains(keyRef, "-----BEGIN") {
		if pemKey, err = secrets.Resolve(ctx, keyRef, 0, nil, nil); err != nil {
			return nil, fmt.Errorf("githubapp: private key: %w", err)
		}
	}
	return New(id, pemKey, os.Getenv("TT_GITHUB_API_URL"))
}

// ParseKey parses an RSA private key PEM (PKCS#1 "RSA PRIVATE KEY" or PKCS#8 "PRIVATE KEY").
func ParseKey(pemKey []byte) (*rsa.PrivateKey, error) {
	// Keys pasted into env vars often carry literal "\n".
	pemKey = []byte(strings.ReplaceAll(string(pemKey), `\n`, "\n"))
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, errors.New("githubapp: private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("githubapp: private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("githubapp: private key is not RSA")
	}
	return rk, nil
}

// JWT returns an app JWT (RS256) valid for 9 minutes, issued 60 s in the past for clock drift.
func (c *Client) JWT() (string, error) {
	now := c.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(c.AppID, 10)})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// InstallationToken returns a token for an installation, cached until 5 minutes before it expires.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (Token, error) {
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[int64]Token{}
	}
	t, ok := c.tokens[installationID]
	c.mu.Unlock()
	if ok && c.now().Add(5*time.Minute).Before(t.ExpiresAt) {
		return t, nil
	}
	jwt, err := c.JWT()
	if err != nil {
		return Token{}, err
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), "Bearer "+jwt, &t); err != nil {
		return Token{}, err
	}
	if t.Token == "" {
		return Token{}, errors.New("githubapp: empty installation token")
	}
	c.mu.Lock()
	c.tokens[installationID] = t
	c.mu.Unlock()
	return t, nil
}

// Forget drops a cached installation token (after a 401 or an uninstall).
func (c *Client) Forget(installationID int64) {
	c.mu.Lock()
	delete(c.tokens, installationID)
	c.mu.Unlock()
}

// Installation is the subset of GET /app/installations/{id} callers need.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"` // User | Organization
	} `json:"account"`
}

// Installation fetches an installation (app JWT).
func (c *Client) Installation(ctx context.Context, installationID int64) (Installation, error) {
	var in Installation
	jwt, err := c.JWT()
	if err != nil {
		return in, err
	}
	err = c.do(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", installationID), "Bearer "+jwt, &in)
	return in, err
}

// Repo is a repository visible to an installation.
type Repo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Language      string `json:"language"`
	CloneURL      string `json:"clone_url"`
}

// InstallationRepos lists every repository of an installation (GET /installation/repositories, all pages).
func (c *Client) InstallationRepos(ctx context.Context, installationID int64) ([]Repo, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var out []Repo
	for page := 1; page <= 100; page++ {
		var res struct {
			Total int    `json:"total_count"`
			Repos []Repo `json:"repositories"`
		}
		if err := c.do(ctx, http.MethodGet, "/installation/repositories?per_page=100&page="+strconv.Itoa(page), "token "+tok.Token, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Repos...)
		if len(res.Repos) < 100 || len(out) >= res.Total {
			break
		}
	}
	return out, nil
}

// APIError is a non-2xx GitHub response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path, auth string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", auth)
	req.Header.Set("User-Agent", "TaskTrooper")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &m)
		return &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// GitAuthHeader returns the http.extraHeader value git needs to clone over HTTPS with token
// ("Authorization: Basic base64(x-access-token:<token>)"). Pass it through GIT_CONFIG_* env, never argv/URLs.
func GitAuthHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// CloneURL returns the HTTPS clone URL of a repository on the GitHub host behind apiURL
// (https://api.github.com → https://github.com/<full>.git; https://ghe/api/v3 → https://ghe/<full>.git).
func CloneURL(apiURL, fullName string) string {
	host := "https://github.com"
	if apiURL != "" && apiURL != DefaultAPIURL {
		if u, err := url.Parse(apiURL); err == nil && u.Host != "" {
			host = u.Scheme + "://" + u.Host
		}
	}
	return host + "/" + strings.Trim(fullName, "/") + ".git"
}

// VerifySignature checks an X-Hub-Signature-256 header ("sha256=<hex>") against body with secret.
func VerifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	got, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	sig, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(sig, m.Sum(nil))
}

// Sign returns the X-Hub-Signature-256 header value for body (tests, fixtures).
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}
