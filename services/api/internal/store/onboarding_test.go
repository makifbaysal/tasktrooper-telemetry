package store

import (
	"strings"
	"testing"
)

func TestCleanName(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"  Acme  ", "Acme", true},
		{"", "", false},
		{"   ", "", false},
		{"a\nb", "", false},
		{strings.Repeat("x", 100), strings.Repeat("x", 100), true},
		{strings.Repeat("ç", 100), strings.Repeat("ç", 100), true},
		{strings.Repeat("x", 101), "", false},
		{"Şirket A.Ş.", "Şirket A.Ş.", true},
	}
	for _, tc := range tests {
		got, err := CleanName("name", tc.in, 100)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("CleanName(%q) = %q, %v", tc.in, got, err)
		}
	}
}

func TestProjectSlug(t *testing.T) {
	tests := []struct {
		name, slug, want string
		ok               bool
	}{
		{"My Shop", "", "my-shop", true},
		{"TaskTrooper", "", "tasktrooper", true},
		{"日本", "", "project", true},
		{strings.Repeat("ab-", 40), "", strings.TrimRight(strings.Repeat("ab-", 21), "-"), true},
		{"x", "custom-1", "custom-1", true},
		{"x", "Bad Slug", "", false},
		{"x", strings.Repeat("a", 64), "", false},
	}
	for _, tc := range tests {
		got, err := ProjectSlug(tc.name, tc.slug)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ProjectSlug(%q, %q) = %q, %v; want %q", tc.name, tc.slug, got, err, tc.want)
		}
	}
}

func TestDSN(t *testing.T) {
	tests := []struct {
		base, want string
		ok         bool
	}{
		{"", "http://tt_ingest_k@localhost:4318/12", true},
		{"https://ingest.example.com", "https://tt_ingest_k@ingest.example.com/12", true},
		{"https://example.com/ingest/", "https://tt_ingest_k@example.com/ingest/12", true},
		{"localhost:4318", "", false},
		{"ftp://x", "", false},
	}
	for _, tc := range tests {
		got, err := DSN(tc.base, "tt_ingest_k", 12)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("DSN(%q) = %q, %v", tc.base, got, err)
		}
		if tc.ok && KeyFromDSN(got) != "tt_ingest_k" {
			t.Errorf("KeyFromDSN(%q) = %q", got, KeyFromDSN(got))
		}
	}
	if k := KeyFromDSN(" tt_ingest_bare\n"); k != "tt_ingest_bare" {
		t.Errorf("bare key: %q", k)
	}
}
