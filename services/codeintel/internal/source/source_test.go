package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestCloneCommandKeepsTokenOutOfArgs(t *testing.T) {
	args, env := CloneCommand("https://github.com/acme/shop.git", "main", "/tmp/x", "ghs_secret123")
	for _, a := range args {
		if strings.Contains(a, "ghs_secret123") {
			t.Fatal("token in argv")
		}
	}
	if len(env) != 3 || !strings.HasPrefix(env[2], "GIT_CONFIG_VALUE_0=Authorization: Basic ") {
		t.Fatalf("env %v", env)
	}
	if _, env := CloneCommand("u", "", "d", ""); env != nil {
		t.Fatal("no token → no env")
	}
}

func TestFetchLocalAndClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	remotes := t.TempDir()
	src := filepath.Join(remotes, "acme", "shop")
	_ = os.MkdirAll(src, 0o755)
	run(t, src, "init", "-q", "-b", "trunk")
	_ = os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-q", "-m", "init")
	// bare copy at <remotes>/acme/shop.git
	run(t, remotes, "clone", "-q", "--bare", src, filepath.Join(remotes, "acme", "shop.git"))

	f := &Fetcher{WorkDir: t.TempDir(), CloneBase: "file://" + remotes + "/"}
	ctx := context.Background()

	local, err := f.Fetch(ctx, model.Repo{Provider: "local", LocalPath: src})
	if err != nil || local.Dir != src || len(local.SHA) != 40 {
		t.Fatalf("local %+v %v", local, err)
	}
	local.Cleanup()
	if _, err := os.Stat(src); err != nil {
		t.Fatal("cleanup removed a local checkout")
	}

	tokenCalls := 0
	f.Token = func(_ context.Context, ref string, project int64) ([]byte, error) {
		tokenCalls++
		if ref != "env:GH_TOKEN" || project != 7 {
			t.Fatalf("ref %s project %d", ref, project)
		}
		return []byte("tok\n"), nil
	}
	co, err := f.Fetch(ctx, model.Repo{Provider: "github", FullName: "acme/shop", DefaultBranch: "trunk", CredentialsRef: "env:GH_TOKEN", ProjectID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if co.SHA != local.SHA || tokenCalls != 1 {
		t.Fatalf("sha %s vs %s, token calls %d", co.SHA, local.SHA, tokenCalls)
	}
	if _, err := os.Stat(filepath.Join(co.Dir, "main.go")); err != nil {
		t.Fatal(err)
	}
	co.Cleanup()
	if _, err := os.Stat(co.Dir); !os.IsNotExist(err) {
		t.Fatal("clone not removed")
	}

	_, err = f.Fetch(ctx, model.Repo{Provider: "github", FullName: "acme/missing", DefaultBranch: "main", CredentialsRef: "env:GH_TOKEN", ProjectID: 7})
	if err == nil || strings.Contains(err.Error(), "tok") && strings.Contains(err.Error(), "Basic") {
		t.Fatalf("missing repo err %v", err)
	}
	if _, err := f.Fetch(ctx, model.Repo{Provider: "svn", FullName: "a/b"}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := f.Fetch(ctx, model.Repo{Provider: "github", FullName: "a/b", InstallationID: 5}); err == nil {
		t.Fatal("installation without app config must fail")
	}
}

func TestRedact(t *testing.T) {
	err := redact(errors.New("fatal: auth ghs_abc failed"), "ghs_abc")
	if strings.Contains(err.Error(), "ghs_abc") {
		t.Fatal(err)
	}
}
