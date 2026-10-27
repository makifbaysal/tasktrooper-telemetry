package source

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Diff is the set of files that differ between an earlier analyzed commit and the checkout's HEAD.
type Diff struct {
	Paths []string // added, modified and deleted files (a rename is its old and its new path), slash separated
	// Layout lists changes that can move files between services (contracts §13 service mapping): `.trooper/config.yaml`
	// edits and module manifests (go.mod, package.json, …) that were added or deleted. Callers fall back to a full scan.
	Layout []string
}

// ErrNoBase: the base commit cannot be obtained (history rewritten, garbage collected, or not a git checkout).
var ErrNoBase = errors.New("base commit not available")

var reSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ChangedSince lists the files that differ between commit sha and HEAD of co. A clone is shallow, so the base commit
// is fetched first (`git fetch --depth=1 origin <sha>`; GitHub/GitLab/Bitbucket serve reachable commits by id over
// protocol v2) with the same credentials as the clone (GIT_CONFIG_* env only). The diff is tree to tree, so it is
// correct after a force push too, as long as the old commit can still be fetched; otherwise ErrNoBase.
//
// Only clones are supported: a local checkout is read in place, uncommitted changes included, so a commit diff would
// not describe what the previous analysis saw.
func (f *Fetcher) ChangedSince(ctx context.Context, co *Checkout, sha string) (*Diff, error) {
	if co == nil || !co.tmp || co.SHA == "" {
		return nil, fmt.Errorf("%w: not a clone", ErrNoBase)
	}
	if !reSHA.MatchString(sha) {
		return nil, fmt.Errorf("%w: invalid sha %q", ErrNoBase, sha)
	}
	if sha == co.SHA {
		return &Diff{}, nil
	}
	if _, err := f.git(ctx, co.Dir, nil, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := f.git(ctx, co.Dir, co.env, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--depth=1", "origin", sha); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoBase, redactEnv(err, co.env))
		}
	}
	out, err := f.git(ctx, co.Dir, nil, "diff", "--name-status", "-z", "--no-renames", "--no-ext-diff", "--ignore-submodules=all", sha, co.SHA, "--")
	if err != nil {
		return nil, err
	}
	return parseNameStatus([]byte(out)) // git() trims spaces only; NUL separators survive
}

// parseNameStatus reads `git diff --name-status -z --no-renames` output: <status>\0<path>\0 …
func parseNameStatus(out []byte) (*Diff, error) {
	d := &Diff{}
	parts := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	if len(out) == 0 {
		return d, nil
	}
	if len(parts)%2 != 0 {
		return nil, errors.New("git diff: unexpected output")
	}
	for i := 0; i < len(parts); i += 2 {
		status, p := string(parts[i]), string(parts[i+1])
		d.Paths = append(d.Paths, p)
		if LayoutChange(p, status) {
			d.Layout = append(d.Layout, p)
		}
	}
	return d, nil
}

// LayoutChange reports whether a changed file can change the service of other files: `.trooper/config.yaml` (any
// change), or a module manifest that was added or deleted (status "A"/"D"; "" = unknown, treated as add/delete).
func LayoutChange(p, status string) bool {
	if p == ".trooper/config.yaml" || p == ".trooper/config.yml" {
		return true
	}
	switch path.Base(p) {
	case "go.mod", "package.json", "pyproject.toml", "requirements.txt", "setup.py":
		return status == "" || strings.HasPrefix(status, "A") || strings.HasPrefix(status, "D")
	}
	return false
}

// redactEnv strips credential values passed through GIT_CONFIG_VALUE_* from an error.
func redactEnv(err error, env []string) error {
	msg := err.Error()
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "GIT_CONFIG_VALUE_") && v != "" {
			msg = strings.ReplaceAll(msg, v, "***")
			if _, cred, ok := strings.Cut(v, "Basic "); ok && cred != "" {
				msg = strings.ReplaceAll(msg, cred, "***")
				if raw, err := base64.StdEncoding.DecodeString(cred); err == nil { // user:token → the token itself
					if _, tok, ok := strings.Cut(string(raw), ":"); ok && tok != "" {
						msg = strings.ReplaceAll(msg, tok, "***")
					}
				}
			}
		}
	}
	return errors.New(msg)
}
