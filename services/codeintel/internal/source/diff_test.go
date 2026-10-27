package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return gitOut(t, dir, "rev-parse", "HEAD")
}

func TestChangedSince(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	cases := []struct {
		name string
		// change edits the work tree of the source repo after the base commit; it returns the base sha to diff from
		// ("" = the base commit) and whether to force-push (rewrite history) instead of a normal push.
		change     func(t *testing.T, dir, base string) (from string, force bool)
		wantPaths  []string
		wantLayout []string
		wantErr    error
	}{
		{
			name: "modify, add, delete",
			change: func(t *testing.T, dir, _ string) (string, bool) {
				write(t, dir, "api/orders.go", "package api // v2\n")
				write(t, dir, "api/new file.go", "package api\n")
				_ = os.Remove(filepath.Join(dir, "web/app.ts"))
				commit(t, dir, "c2")
				write(t, dir, "README.md", "readme v2\n")
				commit(t, dir, "c3")
				return "", false
			},
			wantPaths: []string{"README.md", "api/new file.go", "api/orders.go", "web/app.ts"},
		},
		{
			name: "rename → old and new path",
			change: func(t *testing.T, dir, _ string) (string, bool) {
				run(t, dir, "mv", "api/orders.go", "api/order_handlers.go")
				commit(t, dir, "rename")
				return "", false
			},
			wantPaths: []string{"api/order_handlers.go", "api/orders.go"},
		},
		{
			name: "HEAD is the base → nothing changed",
			change: func(t *testing.T, dir, _ string) (string, bool) {
				return "", false
			},
		},
		{
			name: "empty commit → nothing changed",
			change: func(t *testing.T, dir, _ string) (string, bool) {
				commit(t, dir, "empty")
				return "", false
			},
		},
		{
			name: "config and added manifest are layout changes, modified manifest is not",
			change: func(t *testing.T, dir, _ string) (string, bool) {
				write(t, dir, ".trooper/config.yaml", "services: [{path: api, name: orders}]\n")
				write(t, dir, "api/go.mod", "module x // v2\n")
				write(t, dir, "billing/go.mod", "module billing\n")
				commit(t, dir, "layout")
				return "", false
			},
			wantPaths:  []string{".trooper/config.yaml", "api/go.mod", "billing/go.mod"},
			wantLayout: []string{".trooper/config.yaml", "billing/go.mod"},
		},
		{
			name: "force push, old commit garbage collected → no base",
			change: func(t *testing.T, dir, base string) (string, bool) {
				// an unpushed commit: it never reaches the remote
				write(t, dir, "api/orders.go", "package api // local only\n")
				orphan := commit(t, dir, "local")
				run(t, dir, "reset", "-q", "--hard", base)
				write(t, dir, "api/orders.go", "package api // rewritten\n")
				commit(t, dir, "rewritten")
				return orphan, true
			},
			wantErr: ErrNoBase,
		},
		{
			name: "unknown sha → no base",
			change: func(t *testing.T, dir, _ string) (string, bool) {
				return strings.Repeat("ab", 20), false
			},
			wantErr: ErrNoBase,
		},
		{
			name: "invalid sha → no base",
			change: func(t *testing.T, dir, _ string) (string, bool) {
				write(t, dir, "x.go", "package x\n")
				commit(t, dir, "x")
				return "--upload-pack=touch /tmp/pwned", false
			},
			wantErr: ErrNoBase,
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remotes := t.TempDir()
			src := filepath.Join(t.TempDir(), "src")
			_ = os.MkdirAll(src, 0o755)
			run(t, src, "init", "-q", "-b", "main")
			write(t, src, "api/orders.go", "package api\n")
			write(t, src, "api/go.mod", "module x\n")
			write(t, src, "web/app.ts", "export {}\n")
			write(t, src, "README.md", "readme\n")
			write(t, src, "docs/log.txt", "x")
			base := commit(t, src, "c1")
			for _, v := range []string{"a", "b", "x"} { // history between base and HEAD (no net change): a depth-1 clone lacks the base
				write(t, src, "docs/log.txt", v)
				commit(t, src, "filler "+v)
			}
			if c.name == "HEAD is the base → nothing changed" {
				run(t, src, "reset", "-q", "--hard", base)
			}
			bare := filepath.Join(remotes, "acme", fmt.Sprintf("shop%d.git", i))
			run(t, remotes, "init", "-q", "--bare", "-b", "main", bare)
			run(t, src, "push", "-q", bare, "main")
			from, force := c.change(t, src, base)
			if from == "" {
				from = base
			}
			if force {
				run(t, src, "push", "-q", "--force", bare, "main")
				run(t, bare, "reflog", "expire", "--expire=now", "--all")
				run(t, bare, "gc", "-q", "--prune=now")
			} else {
				run(t, src, "push", "-q", bare, "main")
			}

			f := &Fetcher{WorkDir: t.TempDir(), CloneBase: "file://" + remotes + "/"}
			co, err := f.Fetch(ctx, model.Repo{Provider: "github", FullName: fmt.Sprintf("acme/shop%d", i), DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			defer co.Cleanup()
			if gitOut(t, co.Dir, "rev-parse", "--is-shallow-repository") != "true" {
				t.Fatal("clone is expected to be shallow")
			}
			d, err := f.ChangedSince(ctx, co, from)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(d.Paths)
			if !slices.Equal(d.Paths, c.wantPaths) || !slices.Equal(d.Layout, c.wantLayout) {
				t.Fatalf("paths %q layout %q, want %q %q", d.Paths, d.Layout, c.wantPaths, c.wantLayout)
			}
		})
	}
}

func TestChangedSinceLocalCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.go", "package a\n")
	sha := commit(t, dir, "c1")
	f := &Fetcher{}
	co, err := f.Fetch(context.Background(), model.Repo{Provider: "local", LocalPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ChangedSince(context.Background(), co, sha); !errors.Is(err, ErrNoBase) {
		t.Fatalf("local checkout must not diff: %v", err)
	}
}

func TestLayoutChange(t *testing.T) {
	cases := []struct {
		path, status string
		want         bool
	}{
		{".trooper/config.yaml", "M", true},
		{".trooper/config.yml", "D", true},
		{"svc/.trooper/config.yaml", "M", false},
		{"svc/package.json", "M", false},
		{"svc/package.json", "A", true},
		{"svc/pyproject.toml", "D", true},
		{"go.mod", "", true},
		{"main.go", "A", false},
	}
	for _, c := range cases {
		if got := LayoutChange(c.path, c.status); got != c.want {
			t.Errorf("%s %s: %v", c.path, c.status, got)
		}
	}
}

func TestRedactEnv(t *testing.T) {
	_, env := CloneCommand("u", "", "d", "ghs_secret")
	hdr := strings.TrimPrefix(env[2], "GIT_CONFIG_VALUE_0=Authorization: Basic ")
	err := redactEnv(errors.New("fatal: ghs_secret / "+hdr), env)
	if strings.Contains(err.Error(), "ghs_secret") || strings.Contains(err.Error(), hdr) {
		t.Fatal(err)
	}
}
