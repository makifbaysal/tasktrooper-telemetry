package config

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RemotePath is the api endpoint the collector polls.
const RemotePath = "/api/v1/internal/collector-config"

// Fetcher pulls the target list from the api.
type Fetcher struct {
	APIURL string
	APIKey string
	Client *http.Client
}

// Fetch performs one GET {APIURL}/api/v1/internal/collector-config with the api key.
func (f Fetcher) Fetch(ctx context.Context) (*Config, error) {
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.APIURL, "/")+RemotePath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "trooper-agent")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("collector-config: %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return ParseJSON(body)
}
