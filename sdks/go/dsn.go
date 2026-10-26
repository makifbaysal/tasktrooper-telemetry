package trooper

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DSN is a parsed TaskTrooper DSN: https://<key>@<host>[:port][/<prefix>]/<project_id>.
type DSN struct {
	// Endpoint is the base URL of the ingest gateway without trailing slash
	// (scheme://host[:port][/prefix]). Signal paths (/v1/traces, ...) are appended to it.
	Endpoint string
	// Key is the ingest key sent as x-trooper-key.
	Key string
	// ProjectID is the project identifier (informational; the key determines the tenant).
	ProjectID string
}

// ParseDSN parses a DSN string. The project segment is optional; the key and host are required.
func ParseDSN(s string) (DSN, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DSN{}, errors.New("trooper: empty DSN")
	}
	u, err := url.Parse(s)
	if err != nil {
		return DSN{}, fmt.Errorf("trooper: invalid DSN: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return DSN{}, fmt.Errorf("trooper: invalid DSN scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return DSN{}, errors.New("trooper: DSN has no host")
	}
	if u.User == nil || u.User.Username() == "" {
		return DSN{}, errors.New("trooper: DSN has no ingest key (https://<key>@host/<project>)")
	}
	d := DSN{Key: u.User.Username()}
	path := strings.Trim(u.Path, "/")
	prefix := ""
	if path != "" {
		if i := strings.LastIndex(path, "/"); i >= 0 {
			prefix, d.ProjectID = "/"+path[:i], path[i+1:]
		} else {
			d.ProjectID = path
		}
	}
	d.Endpoint = u.Scheme + "://" + u.Host + prefix
	return d, nil
}

// signalURL returns the full URL of a signal path (e.g. "/v1/traces").
func (d DSN) signalURL(path string) string { return d.Endpoint + path }
