// Package config reads the MCP server's TT_* settings and validates them once
// at startup (ADR-025, contracts §26).
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/pkg/config"
)

// Config is the server's validated settings.
type Config struct {
	APIURL string
	APIKey string
	// OrgID and ProjectID are optional cross-checks: when set, the key must
	// resolve to them or the server refuses to start. The tenant itself always
	// comes from the key (ADR-016); these ids only prevent reading the wrong
	// project with a stale key.
	OrgID     uint32
	ProjectID uint32
	LogLevel  slog.Level
}

// Load reads TT_* and fails fast on anything that would otherwise surface as a
// confusing per-tool error later.
func Load() (Config, error) {
	c := Config{
		APIURL: strings.TrimRight(config.String("TT_API_URL", "http://localhost:8080"), "/"),
		APIKey: config.String("TT_API_KEY", ""),
	}
	if c.APIKey == "" {
		return Config{}, fmt.Errorf("TT_API_KEY is required (dev: tt_dev_api_key)")
	}
	if u, err := url.ParseRequestURI(c.APIURL); err != nil || u.Host == "" {
		return Config{}, fmt.Errorf("TT_API_URL is not a valid URL")
	}
	var err error
	if c.OrgID, err = optionalID("TT_ORG_ID"); err != nil {
		return Config{}, err
	}
	if c.ProjectID, err = optionalID("TT_PROJECT_ID"); err != nil {
		return Config{}, err
	}
	c.LogLevel = parseLevel(config.String("TT_LOG_LEVEL", "info"))
	return c, nil
}

// optionalID parses a TT_*_ID that may be absent. Zero means "not set".
func optionalID(key string) (uint32, error) {
	v := config.String(key, "")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return uint32(n), nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
