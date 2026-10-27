package vcs

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidFullName(t *testing.T) {
	for _, tc := range []struct {
		provider, name string
		ok             bool
	}{
		{GitHub, "acme/shop", true},
		{GitHub, "acme/shop/extra", false},
		{GitHub, "acme", false},
		{GitLab, "acme/shop", true},
		{GitLab, "acme/platform/backend/orders", true},
		{GitLab, "acme", false},
		{GitLab, "acme//shop", false},
		{GitLab, "acme/../shop", false},
		{GitLab, "acme/shop.git", false},
		{GitLab, "acme/-shop", false},
		{GitLab, "acme/sh op", false},
		{GitLab, strings.Repeat("g/", 21) + "p", false},
		{Bitbucket, "my-workspace/my_repo.v2", true},
		{Bitbucket, "ws/group/repo", false},
		{Bitbucket, "ws", false},
		{Bitbucket, "ws/re$po", false},
		{Local, "a/b", false},
	} {
		err := ValidFullName(tc.provider, tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("%s %q: err=%v, want ok=%v", tc.provider, tc.name, err, tc.ok)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		err      bool
	}{
		{"", "", false},
		{"https://GitLab.Example.com/", "https://gitlab.example.com", false},
		{"https://example.com:8443/gitlab/", "https://example.com:8443/gitlab", false},
		{"http://gitlab.example.com", "", true},
		{"https://user:pw@gitlab.example.com", "", true},
		{"https://token@gitlab.example.com", "", true},
		{"https://gitlab.example.com/?x=1", "", true},
		{"https://gitlab.example.com/#frag", "", true},
		{"https://", "", true},
		{"gitlab.example.com", "", true},
		{"https://gitlab.example.com/a/../b", "", true},
		{"file:///etc", "", true},
	} {
		got, err := NormalizeBaseURL(tc.in)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%q: got %q err=%v, want %q err=%v", tc.in, got, err, tc.want, tc.err)
		}
	}
}

func TestHostAndCloneURL(t *testing.T) {
	for _, tc := range []struct{ provider, base, want string }{
		{GitLab, "", "https://gitlab.com/g/s/p.git"},
		{GitLab, "https://git.acme.io/", "https://git.acme.io/g/s/p.git"},
		{Bitbucket, "https://ignored", "https://bitbucket.org/g/s/p.git"},
		{GitHub, "", "https://github.com/g/s/p.git"},
	} {
		if got := CloneURL(Host(tc.provider, tc.base), "g/s/p"); got != tc.want {
			t.Errorf("%s %q: %s", tc.provider, tc.base, got)
		}
	}
}

func TestAuthHeader(t *testing.T) {
	basic := func(s string) string { return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		provider, token, want string
		hide                  []string
	}{
		{GitLab, "glpat-abc", basic("oauth2:glpat-abc"), []string{"glpat-abc"}},
		{Bitbucket, "ATCTT3x", basic("x-token-auth:ATCTT3x"), []string{"ATCTT3x"}},
		{Bitbucket, "jane:app-pass", basic("jane:app-pass"), []string{"jane:app-pass", "app-pass"}},
		{GitHub, "ghp_1", basic("x-access-token:ghp_1"), []string{"ghp_1"}},
		{GitLab, " ", "", nil},
	} {
		h, hide := AuthHeader(tc.provider, tc.token)
		if h != tc.want {
			t.Errorf("%s %q: header %q", tc.provider, tc.token, h)
		}
		if h == "" {
			continue
		}
		joined := strings.Join(hide, "\n")
		for _, s := range append(tc.hide, strings.TrimPrefix(h, "Authorization: Basic ")) {
			if !strings.Contains(joined, s) {
				t.Errorf("%s %q: %q not hidden (%v)", tc.provider, tc.token, s, hide)
			}
		}
	}
}
