// Package vcs holds the rules shared by the api and codeintel for repository providers (contracts §13, §18a):
// full-name formats, the self-managed GitLab base URL, clone URLs and the git HTTP authorization header.
package vcs

import (
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Providers (repositories.provider).
const (
	GitHub    = "github"
	GitLab    = "gitlab"
	Bitbucket = "bitbucket"
	Local     = "local"
)

// Default hosts.
const (
	GitHubURL    = "https://github.com"
	GitLabURL    = "https://gitlab.com"
	BitbucketURL = "https://bitbucket.org"
)

var segment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// maxGitLabDepth: GitLab allows 20 levels of subgroups; the project adds one.
const maxGitLabDepth = 21

// ValidFullName checks a repository's full name for a hosted provider: github `owner/name`, gitlab
// `group/subgroup/…/project` (2+ segments), bitbucket exactly `workspace/repo`.
func ValidFullName(provider, name string) error {
	parts := strings.Split(name, "/")
	switch provider {
	case GitHub, Bitbucket:
		if len(parts) != 2 {
			if provider == GitHub {
				return errors.New("fullName must be owner/name")
			}
			return errors.New("fullName must be workspace/repo")
		}
	case GitLab:
		if len(parts) < 2 || len(parts) > maxGitLabDepth {
			return errors.New("fullName must be group/project (subgroups allowed: group/sub/project)")
		}
	default:
		return errors.New("unknown provider")
	}
	for _, p := range parts {
		if !segment.MatchString(p) || len(p) > 255 || strings.HasSuffix(p, ".git") || strings.HasSuffix(p, ".atom") || strings.Contains(p, "..") {
			return errors.New("fullName has an invalid path segment " + quote(p))
		}
	}
	return nil
}

func quote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return `"` + s + `"`
}

// NormalizeBaseURL validates a self-managed GitLab address: https, a host, no userinfo, query or fragment. A path is
// kept (GitLab under a relative URL root); the trailing slash is dropped. "" stays "" (gitlab.com).
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("baseUrl is not a URL")
	}
	switch {
	case u.Scheme != "https":
		return "", errors.New("baseUrl must use https")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("baseUrl needs a host")
	case u.User != nil:
		return "", errors.New("baseUrl must not contain credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return "", errors.New("baseUrl must not have a query or fragment")
	case u.Opaque != "":
		return "", errors.New("baseUrl is not a URL")
	}
	p := strings.TrimRight(u.EscapedPath(), "/")
	if strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") || strings.Contains(p, "/./") {
		return "", errors.New("baseUrl path must be clean")
	}
	return "https://" + strings.ToLower(u.Host) + p, nil
}

// Host returns the web/clone host of a provider: baseURL for GitLab when set, the provider default otherwise.
func Host(provider, baseURL string) string {
	switch provider {
	case GitLab:
		if baseURL != "" {
			return strings.TrimRight(baseURL, "/")
		}
		return GitLabURL
	case Bitbucket:
		return BitbucketURL
	}
	return GitHubURL
}

// CloneURL is the HTTPS clone URL (<host>/<full name>.git).
func CloneURL(host, fullName string) string {
	return strings.TrimRight(host, "/") + "/" + strings.Trim(fullName, "/") + ".git"
}

// AuthHeader returns the git http.extraHeader for a token and the strings an error message must never show
// (the token, a password, the Basic credential). A token of the form `user:password` (Bitbucket app password /
// API token, GitLab username + token) is sent as is; a bare token uses the provider's conventional user
// (gitlab `oauth2`, bitbucket `x-token-auth`, github `x-access-token`).
func AuthHeader(provider, token string) (header string, hide []string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", nil
	}
	cred := token
	hide = []string{token}
	if user, pass, ok := strings.Cut(token, ":"); ok && user != "" && pass != "" {
		hide = append(hide, pass)
	} else {
		user := "x-access-token"
		switch provider {
		case GitLab:
			user = "oauth2"
		case Bitbucket:
			user = "x-token-auth"
		}
		cred = user + ":" + token
	}
	b := base64.StdEncoding.EncodeToString([]byte(cred))
	return "Authorization: Basic " + b, append(hide, b)
}
