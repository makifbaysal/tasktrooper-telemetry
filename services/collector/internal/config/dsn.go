package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// IngestDSN is a parsed TaskTrooper DSN: `<scheme>://<key>@<host>[:port]/<project_id>`.
type IngestDSN struct {
	// MetricsURL is the OTLP/HTTP metrics endpoint (`<scheme>://<host>[:port]/v1/metrics`).
	MetricsURL string
	Key        string
	ProjectID  string
	Insecure   bool
}

// ParseIngestDSN parses TT_DSN (contracts §2), e.g. http://tt_dev_ingest_key@localhost:4318/1.
func ParseIngestDSN(s string) (IngestDSN, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return IngestDSN{}, errors.New("dsn is empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return IngestDSN{}, fmt.Errorf("dsn: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return IngestDSN{}, fmt.Errorf("dsn: scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return IngestDSN{}, errors.New("dsn: host is missing")
	}
	if u.User == nil || u.User.Username() == "" {
		return IngestDSN{}, errors.New("dsn: ingest key is missing (want <scheme>://<key>@host/<project>)")
	}
	project := strings.Trim(u.Path, "/")
	base := url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/v1/metrics"}
	return IngestDSN{
		MetricsURL: base.String(),
		Key:        u.User.Username(),
		ProjectID:  project,
		Insecure:   u.Scheme == "http",
	}, nil
}
