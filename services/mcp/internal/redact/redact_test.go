package redact

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain untouched", "connection refused after 3 retries", "connection refused after 3 retries"},
		{"email", "login failed for alice@example.com: 401", "login failed for [redacted]: 401"},
		{"ipv4", "dial tcp 10.0.0.8:5432: i/o timeout", "dial tcp [redacted]:5432: i/o timeout"},
		{"ipv6", "peer 2001:0db8:85a3:0000:0000:8a2e:0370:7334 unreachable",
			"peer [redacted] unreachable"},
		{"bearer", "Authorization: Bearer abcDEF-123_xyz.ghi", "Authorization: [redacted]"},
		{"bearer bare", "Bearer eyJhbGciOiJIUzI1NiJ9.abc", "[redacted]"},
		{"api key", "rejected key tt_api_AbC123dEfGhIjKlMnOpQrStUvWxYz0123456789",
			"rejected key [redacted]"},
		{"ingest key", "tt_ingest_ZzYyXx-WwVvUu", "[redacted]"},
		{"dev key", "configured tt_dev_api_key", "configured [redacted]"},
		// Regression: the pattern must not eat our own tool names.
		{"tool name untouched", "agent called tt_list_services then tt_get_trace",
			"agent called tt_list_services then tt_get_trace"},
		{"group name untouched", "consumer group tt_worker_group_1 lagging",
			"consumer group tt_worker_group_1 lagging"},
		{"card spaced", "charged card 4111 1111 1111 1111 today", "charged card [redacted] today"},
		{"card dashed", "charged card 4111-1111-1111-1111 today", "charged card [redacted] today"},
		{"card plain", "charged card 4111111111111111 today", "charged card [redacted] today"},
		// \b anchors: a digit run longer than 16 must not be rewritten.
		{"32-digit trace id untouched", "trace=01234567890123456789012345678901",
			"trace=01234567890123456789012345678901"},
		{"19-digit ns timestamp untouched", "ts=1730000000123456789", "ts=1730000000123456789"},
		{"13-digit ms timestamp untouched", "ts=1730000000123", "ts=1730000000123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := String(tc.in); got != tc.want {
				t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMap(t *testing.T) {
	attrs := map[string]string{
		"http.request.header.x_email": "bob@corp.io",
		"net.peer.ip":                 "192.168.1.5",
		"deployment.environment":      "production", // untouched
		"count":                       "42",         // untouched
	}
	Map(attrs)
	if attrs["http.request.header.x_email"] != "[redacted]" {
		t.Errorf("email not masked: %q", attrs["http.request.header.x_email"])
	}
	if attrs["net.peer.ip"] != "[redacted]" {
		t.Errorf("ip not masked: %q", attrs["net.peer.ip"])
	}
	if attrs["deployment.environment"] != "production" {
		t.Errorf("unrelated attr rewritten: %q", attrs["deployment.environment"])
	}
	if attrs["count"] != "42" {
		t.Errorf("unrelated attr rewritten: %q", attrs["count"])
	}
}
