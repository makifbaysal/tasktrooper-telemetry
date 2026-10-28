package source

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// bareRepo creates <root>/<full>.git with one commit on branch and returns its HEAD sha.
func bareRepo(t *testing.T, root, full, branch string) string {
	t.Helper()
	src := t.TempDir()
	run(t, src, "init", "-q", "-b", branch)
	_ = os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-q", "-m", "init")
	dst := filepath.Join(root, filepath.FromSlash(full)+".git")
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	run(t, root, "clone", "-q", "--bare", src, dst)
	out, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func needGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// GitLab and Bitbucket clone from a file:// remote (CloneBase), public and with a token.
func TestFetchHostedFileRemote(t *testing.T) {
	needGit(t)
	remotes := t.TempDir()
	glSHA := bareRepo(t, remotes, "acme/platform/orders", "main")
	bbSHA := bareRepo(t, remotes, "acme-ws/shop", "trunk")
	f := &Fetcher{WorkDir: t.TempDir(), CloneBase: "file://" + remotes + "/",
		Token: func(context.Context, string, int64) ([]byte, error) { return []byte("glpat-xyz\n"), nil }}
	for _, tc := range []struct {
		name string
		repo model.Repo
		sha  string
	}{
		{"gitlab subgroup public", model.Repo{Provider: "gitlab", FullName: "acme/platform/orders", DefaultBranch: "main"}, glSHA},
		{"gitlab token", model.Repo{Provider: "gitlab", FullName: "acme/platform/orders", DefaultBranch: "main", CredentialsRef: "env:T"}, glSHA},
		{"bitbucket", model.Repo{Provider: "bitbucket", FullName: "acme-ws/shop", DefaultBranch: "trunk", CredentialsRef: "env:T"}, bbSHA},
	} {
		co, err := f.Fetch(context.Background(), tc.repo)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if co.SHA != tc.sha {
			t.Errorf("%s: sha %s want %s", tc.name, co.SHA, tc.sha)
		}
		if (tc.repo.CredentialsRef != "") != (len(co.env) == 3) {
			t.Errorf("%s: env %v", tc.name, co.env)
		}
		co.Cleanup()
	}
}

func TestFetchHostedRejectsBadInput(t *testing.T) {
	f := &Fetcher{WorkDir: t.TempDir()}
	for _, r := range []model.Repo{
		{Provider: "gitlab", FullName: "acme/x", BaseURL: "http://gitlab.example.com"},
		{Provider: "gitlab", FullName: "acme/x", BaseURL: "https://user:pw@gitlab.example.com"},
		{Provider: "bitbucket", FullName: "ws/x", BaseURL: "https://bitbucket.example.com"},
		{Provider: "bitbucket", FullName: "ws/group/x"},
		{Provider: "gitlab", FullName: "--upload-pack=x"},
		{Provider: "gitlab", FullName: "acme/x", CredentialsRef: "env:T"}, // no resolver
	} {
		if _, err := f.Fetch(context.Background(), r); err == nil {
			t.Errorf("%+v: expected an error", r)
		}
	}
}

// fakeGitHost serves smart HTTP (git http-backend over CGI) over TLS under prefix and requires the Basic credential.
func fakeGitHost(t *testing.T, root, prefix, wantCred string) (*httptest.Server, *[]string) {
	t.Helper()
	gitBin, _ := exec.LookPath("git")
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"}, Root: prefix,
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(wantCred)) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "HTTP Basic: Access denied", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", ca)
	return srv, &seen
}

// Self-managed GitLab (base_url with a path) and Bitbucket over HTTPS: the token reaches the server only as the
// Authorization header, and a rejected token never shows up in the error.
func TestFetchHostedHTTPS(t *testing.T) {
	needGit(t)
	if out, err := exec.Command("git", "http-backend").CombinedOutput(); err != nil && !strings.Contains(string(out), "Status") {
		t.Skipf("git http-backend unavailable: %v %s", err, out)
	}
	root := t.TempDir()
	sha := bareRepo(t, root, "acme/platform/orders", "main")

	srv, seen := fakeGitHost(t, root, "/gitlab", "oauth2:glpat-secret123")
	token := "glpat-secret123"
	f := &Fetcher{WorkDir: t.TempDir(), Token: func(context.Context, string, int64) ([]byte, error) { return []byte(token), nil }}
	repo := model.Repo{Provider: "gitlab", FullName: "acme/platform/orders", DefaultBranch: "main", BaseURL: srv.URL + "/gitlab/",
		CredentialsRef: "secret:1", ProjectID: 3}
	co, err := f.Fetch(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if co.SHA != sha {
		t.Fatalf("sha %s want %s", co.SHA, sha)
	}
	co.Cleanup()
	if len(*seen) == 0 || !strings.HasPrefix((*seen)[0], "/gitlab/acme/platform/orders.git/") {
		t.Fatalf("requests %v", *seen)
	}

	token = "glpat-wrong999"
	_, err = f.Fetch(context.Background(), repo)
	if err == nil {
		t.Fatal("wrong token must fail")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("oauth2:glpat-wrong999"))
	if strings.Contains(err.Error(), "glpat-wrong999") || strings.Contains(err.Error(), b64) {
		t.Fatalf("token leaked: %v", err)
	}

	// Bitbucket app password (user:password) through CloneBase pointing at the TLS host.
	srv2, _ := fakeGitHost(t, root, "", "jane:app-pass-777")
	token = "jane:app-pass-777"
	f.CloneBase = srv2.URL + "/"
	co, err = f.Fetch(context.Background(), model.Repo{Provider: "bitbucket", FullName: "acme/platform", DefaultBranch: "main", CredentialsRef: "secret:2"})
	if err == nil {
		co.Cleanup()
		t.Fatal("acme/platform does not exist")
	}
	if strings.Contains(err.Error(), "app-pass-777") {
		t.Fatalf("password leaked: %v", err)
	}
	_ = bareRepo(t, root, "acme-ws/shop", "main")
	co, err = f.Fetch(context.Background(), model.Repo{Provider: "bitbucket", FullName: "acme-ws/shop", DefaultBranch: "main", CredentialsRef: "secret:2"})
	if err != nil {
		t.Fatal(err)
	}
	co.Cleanup()
}
