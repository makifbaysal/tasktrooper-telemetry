// Package source gets a repository's code onto disk: provider `local` is read in place; GitHub, GitLab (gitlab.com or
// a self-managed base_url) and Bitbucket Cloud are shallow-cloned over HTTPS with the git CLI. Credentials (GitHub App
// installation token, or a credentials_ref token) travel only through GIT_CONFIG_* environment variables as an
// http.extraHeader — never in argv, URLs, logs or error messages (contracts §13, §18a).
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tasktrooper/telemetry/services/pkg/githubapp"
	"github.com/tasktrooper/telemetry/services/pkg/vcs"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// Checkout is a repository on disk.
type Checkout struct {
	Dir string
	SHA string // HEAD commit ("" when the local path is not a git work tree)
	tmp bool
	env []string // credentials env of the clone (GIT_CONFIG_*), reused by ChangedSince's fetch
}

// Cleanup removes a clone (local checkouts are left alone).
func (c *Checkout) Cleanup() {
	if c != nil && c.tmp {
		_ = os.RemoveAll(c.Dir)
	}
}

// TokenFunc resolves a credentials_ref (secret:<id> | env:NAME | file:/path) of a project.
type TokenFunc func(ctx context.Context, ref string, projectID int64) ([]byte, error)

// Fetcher clones repositories.
type Fetcher struct {
	WorkDir string            // clones go under here (default os.TempDir())
	App     *githubapp.Client // nil: GitHub App not configured
	Token   TokenFunc         // credentials_ref resolver
	APIURL  string            // GitHub REST root (clone host is derived from it)
	// CloneBase overrides the clone URL prefix (tests: "file:///tmp/remotes/"); <CloneBase><full name>.git.
	CloneBase string
	Git       string // git binary (default "git")
}

// ErrUnsupported is returned for providers without a fetcher yet.
var ErrUnsupported = errors.New("provider not supported yet")

// Fetch makes the repository's default branch available on disk.
func (f *Fetcher) Fetch(ctx context.Context, repo model.Repo) (*Checkout, error) {
	switch repo.Provider {
	case "local":
		if repo.LocalPath == "" {
			return nil, errors.New("local repository without local_path")
		}
		st, err := os.Stat(repo.LocalPath)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("local_path %q is not a directory", repo.LocalPath)
		}
		sha, _ := f.git(ctx, repo.LocalPath, nil, "rev-parse", "HEAD")
		return &Checkout{Dir: repo.LocalPath, SHA: sha}, nil
	case vcs.GitHub:
		return f.cloneGitHub(ctx, repo)
	case vcs.GitLab, vcs.Bitbucket:
		return f.cloneHosted(ctx, repo)
	default:
		return nil, fmt.Errorf("%s: %w", repo.Provider, ErrUnsupported)
	}
}

func (f *Fetcher) cloneGitHub(ctx context.Context, repo model.Repo) (*Checkout, error) {
	var token string
	switch {
	case repo.InstallationID > 0 && f.App != nil:
		t, err := f.App.InstallationToken(ctx, repo.InstallationID)
		if err != nil {
			return nil, fmt.Errorf("installation token: %w", err)
		}
		token = t.Token
	case repo.InstallationID > 0:
		return nil, errors.New("repository belongs to a GitHub App installation but TT_GITHUB_APP_ID / TT_GITHUB_APP_PRIVATE_KEY are not set")
	case repo.CredentialsRef != "" && f.Token != nil:
		b, err := f.Token(ctx, repo.CredentialsRef, repo.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("credentials_ref: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	url := f.CloneBase + repo.FullName + ".git"
	if f.CloneBase == "" {
		url = githubapp.CloneURL(f.APIURL, repo.FullName)
	}
	header, hide := vcs.AuthHeader(vcs.GitHub, token)
	co, err := f.clone(ctx, url, repo.DefaultBranch, header, hide)
	if err != nil && token != "" && repo.InstallationID > 0 && f.App != nil {
		f.App.Forget(repo.InstallationID) // a revoked token must not be reused
	}
	return co, err
}

// cloneHosted clones a GitLab (gitlab.com or base_url) or Bitbucket Cloud repository with an optional
// credentials_ref token (none: public repository).
func (f *Fetcher) cloneHosted(ctx context.Context, repo model.Repo) (*Checkout, error) {
	if err := vcs.ValidFullName(repo.Provider, repo.FullName); err != nil {
		return nil, fmt.Errorf("%s repository %q: %w", repo.Provider, repo.FullName, err)
	}
	var base string
	switch {
	case repo.Provider == vcs.GitLab:
		b, err := vcs.NormalizeBaseURL(repo.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("base_url: %w", err)
		}
		base = b
	case repo.BaseURL != "":
		return nil, errors.New("base_url only applies to provider gitlab")
	}
	var token string
	if repo.CredentialsRef != "" {
		if f.Token == nil {
			return nil, errors.New("credentials_ref: no secret resolver configured")
		}
		b, err := f.Token(ctx, repo.CredentialsRef, repo.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("credentials_ref: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	url := vcs.CloneURL(vcs.Host(repo.Provider, base), repo.FullName)
	if f.CloneBase != "" && base == "" {
		url = f.CloneBase + repo.FullName + ".git"
	}
	header, hide := vcs.AuthHeader(repo.Provider, token)
	return f.clone(ctx, url, repo.DefaultBranch, header, hide)
}

// clone makes a shallow clone of one branch into a fresh directory under WorkDir. header (http.extraHeader) only
// travels in the environment; hide lists the strings scrubbed from errors.
func (f *Fetcher) clone(ctx context.Context, url, branch, header string, hide []string) (*Checkout, error) {
	base := f.WorkDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(base, "repo-")
	if err != nil {
		return nil, err
	}
	args, env := cloneArgs(url, branch, dir, header)
	co := &Checkout{Dir: dir, tmp: true, env: env}
	if _, err := f.git(ctx, "", env, args...); err != nil {
		co.Cleanup()
		return nil, redactAll(err, hide...)
	}
	co.SHA, _ = f.git(ctx, dir, nil, "rev-parse", "HEAD")
	return co, nil
}

// CloneCommand returns git arguments and extra environment for a shallow clone of one GitHub branch. The token, when
// set, is only in the environment (http.extraHeader), never in the arguments or the URL.
func CloneCommand(url, branch, dir, token string) (args, env []string) {
	h := ""
	if token != "" {
		h = githubapp.GitAuthHeader(token)
	}
	return cloneArgs(url, branch, dir, h)
}

func cloneArgs(url, branch, dir, header string) (args, env []string) {
	args = []string{"clone", "--quiet", "--depth", "1", "--single-branch", "--no-tags"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", url, dir)
	if header != "" {
		env = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + header}
	}
	return args, env
}

func (f *Fetcher) git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	bin := f.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// redact strips a token (and its Basic form) from an error message.
func redact(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return redactAll(err, token, strings.TrimPrefix(githubapp.GitAuthHeader(token), "Authorization: Basic "))
}

// redactAll replaces every non-empty secret in an error message with ***.
func redactAll(err error, secrets ...string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return errors.New(msg)
}

// InTree reports whether rel (slash separated) stays inside the checkout.
func InTree(rel string) bool {
	c := filepath.ToSlash(filepath.Clean(rel))
	return c != ".." && !strings.HasPrefix(c, "../") && !filepath.IsAbs(rel)
}
