// Package redact masks PII in telemetry text before it is handed to an LLM
// (ADR-025, contracts §26). It is applied to log messages and trace attribute
// values only — MCP collects nothing beyond what services/api already returns.
//
// This is the one implementation for services/mcp; do not add a second masking
// helper elsewhere in this module. It does not replace the SDK-side masking
// (PII is not collected by default, sdk.md): it is defense in depth for the
// strings that legitimately arrive in logs and span attributes.
package redact

import "regexp"

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// IPv4, plus a conservative IPv6 form (a run of ≥3 hex groups separated by
	// colons). Kept separate from IPv4 so the two patterns stay readable.
	ipv4Re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Re = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{1,4}:){3,}[0-9A-Fa-f]{0,4}\b`)
	// Bearer token values, whether or not the "Authorization:" key is present.
	bearerRe = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
	// TaskTrooper api/ingest keys: "tt_" + kind ("api"|"ingest") + "_" + a
	// 40-char base64url token (store.CreateAPIKey); the dev seed uses
	// tt_dev_api_key / tt_dev_ingest_key. Anchored on the kind so tool names
	// like tt_list_services are left alone.
	ttKeyRe = regexp.MustCompile(`\btt_(?:api|ingest|dev)_[A-Za-z0-9_-]+\b`)
	// A 16-digit run, optionally grouped by single spaces or dashes — the
	// card-like number mcp.md §4 calls for. The \b anchors keep it from
	// matching inside a longer digit run, so trace ids (32 hex chars) and
	// 19-digit nanosecond timestamps are untouched.
	cardRe = regexp.MustCompile(`\b(?:\d[ -]?){15}\d\b`)
)

const mask = "[redacted]"

// String masks every PII pattern in s. The result is always safe to return to
// an agent or to write to a log line.
func String(s string) string {
	if s == "" {
		return s
	}
	s = emailRe.ReplaceAllString(s, mask)
	s = ipv4Re.ReplaceAllString(s, mask)
	s = ipv6Re.ReplaceAllString(s, mask)
	s = bearerRe.ReplaceAllString(s, mask)
	s = ttKeyRe.ReplaceAllString(s, mask)
	s = cardRe.ReplaceAllString(s, mask)
	return s
}

// Map masks every string value in attrs, in place. Non-string values are left
// untouched (the API types attrs as strings, but be defensive).
func Map(attrs map[string]string) {
	for k, v := range attrs {
		if m := String(v); m != v {
			attrs[k] = m
		}
	}
}
