package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/pkg/githubapp"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/obs"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

func (s *fakeStore) ReposByName(_ context.Context, provider, name string) ([]model.Repo, error) {
	var out []model.Repo
	for _, r := range s.repos {
		if r.Provider == provider && strings.EqualFold(r.FullName, name) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) WebhookReceived(_ context.Context, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call{op: "seen", names: []string{fmt.Sprint(ids)}})
	return nil
}

func repoHook(provider string, st *fakeStore) *RepoHook {
	return &RepoHook{Provider: provider, Store: st, Metrics: obs.NewRegistry(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		RepoSecret: func(_ context.Context, r model.Repo) ([]byte, error) {
			switch r.WebhookSecretRef {
			case "secret:1":
				return []byte("s3cret-one"), nil
			case "secret:2":
				return []byte("s3cret-two"), nil
			}
			return nil, fmt.Errorf("unknown ref")
		}}
}

func hookRepos() []model.Repo {
	return []model.Repo{
		{ID: 1, Provider: "gitlab", FullName: "acme/platform/orders", DefaultBranch: "main", Status: "ready", WebhookSecretRef: "secret:1"},
		{ID: 2, Provider: "gitlab", FullName: "acme/platform/orders", DefaultBranch: "main", Status: "ready", WebhookSecretRef: "secret:2"}, // other project
		{ID: 3, Provider: "github", FullName: "acme/platform/orders", DefaultBranch: "main", Status: "ready", WebhookSecretRef: "secret:1"},
		{ID: 4, Provider: "gitlab", FullName: "acme/platform/orders", DefaultBranch: "main", Status: "disconnected", WebhookSecretRef: "secret:1"},
		{ID: 5, Provider: "bitbucket", FullName: "acme-ws/orders", DefaultBranch: "main", Status: "ready", WebhookSecretRef: "secret:1"},
		{ID: 6, Provider: "bitbucket", FullName: "acme-ws/orders", DefaultBranch: "", Status: "ready"}, // no secret → never matched
	}
}

type hookReq struct {
	fixture string
	body    string // overrides fixture
	headers map[string]string
	sign    string // bitbucket: HMAC secret
}

func (q hookReq) do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(q.body)
	if q.fixture != "" {
		body = fixture(t, q.fixture)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range q.headers {
		req.Header.Set(k, v)
	}
	if q.sign != "" {
		req.Header.Set("X-Hub-Signature", githubapp.Sign([]byte(q.sign), body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func requests(calls []call) []call {
	var out []call
	for _, c := range calls {
		if c.op == "request" {
			out = append(out, c)
		}
	}
	return out
}

func TestGitLabWebhook(t *testing.T) {
	gl := func(token, uuid string) map[string]string {
		return map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": token, "X-Gitlab-Event-UUID": uuid, "X-Gitlab-Webhook-UUID": "hook-a"}
	}
	for _, tc := range []struct {
		name   string
		req    hookReq
		code   int
		result string
		want   []call
		seen   bool
	}{
		{"push queues changed files", hookReq{fixture: "gitlab_push.json", headers: gl("s3cret-one", "e1")}, 202, "queued",
			[]call{{op: "request", id: 1, paths: []string{"cmd/api/main.go", "internal/refunds/handler.go", "internal/v0/orders.go"}}}, true},
		{"other project's secret touches only its repo", hookReq{fixture: "gitlab_push.json", headers: gl("s3cret-two", "e2")}, 202, "queued",
			[]call{{op: "request", id: 2, paths: []string{"cmd/api/main.go", "internal/refunds/handler.go", "internal/v0/orders.go"}}}, true},
		{"truncated commit list → full scan", hookReq{fixture: "gitlab_push_truncated.json", headers: gl("s3cret-one", "e3")}, 202, "queued",
			[]call{{op: "request", id: 1, paths: []string{store.FullScan}}}, true},
		{"other branch ignored", hookReq{fixture: "gitlab_push_feature.json", headers: gl("s3cret-one", "e4")}, 200, "ignored", nil, true},
		{"branch deletion ignored", hookReq{fixture: "gitlab_branch_delete.json", headers: gl("s3cret-one", "e5")}, 200, "ignored", nil, true},
		{"tag push ignored", hookReq{fixture: "gitlab_push.json", headers: map[string]string{"X-Gitlab-Event": "Tag Push Hook", "X-Gitlab-Token": "s3cret-one"}}, 200, "ignored", nil, true},
		{"wrong token", hookReq{fixture: "gitlab_push.json", headers: gl("s3cret-nope", "e6")}, 401, "unauthorized", nil, false},
		{"token prefix is not enough", hookReq{fixture: "gitlab_push.json", headers: gl("s3cret-on", "e7")}, 401, "unauthorized", nil, false},
		{"no token", hookReq{fixture: "gitlab_push.json", headers: map[string]string{"X-Gitlab-Event": "Push Hook"}}, 401, "unauthorized", nil, false},
		{"unknown project", hookReq{body: `{"object_kind":"push","project":{"path_with_namespace":"x/y"}}`, headers: gl("s3cret-one", "e8")}, 401, "unauthorized", nil, false},
		{"invalid json", hookReq{body: `{`, headers: gl("s3cret-one", "e9")}, 400, "bad_request", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{deliveries: map[string]bool{}, repos: hookRepos()}
			h := repoHook("gitlab", st)
			rec := tc.req.do(t, h, "/v1/webhooks/gitlab")
			if rec.Code != tc.code {
				t.Fatalf("code %d %s", rec.Code, rec.Body)
			}
			if got := requests(st.calls); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("calls %+v want %+v", got, tc.want)
			}
			if seen := len(st.calls) > 0 && st.calls[0].op == "seen"; seen != tc.seen {
				t.Fatalf("webhook_received_at %v (%+v)", seen, st.calls)
			}
			label := "gitlab:push"
			if tc.req.headers["X-Gitlab-Event"] != "Push Hook" {
				label = "gitlab:other"
			}
			if h.Metrics.Get("codeintel_webhooks_total", "event", label, "result", tc.result) != 1 {
				t.Fatalf("metric %s/%s missing", label, tc.result)
			}
		})
	}
}

func TestGitLabWebhookIdempotency(t *testing.T) {
	st := &fakeStore{deliveries: map[string]bool{}, repos: hookRepos()}
	h := repoHook("gitlab", st)
	q := hookReq{fixture: "gitlab_push.json", headers: map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "s3cret-one",
		"Idempotency-Key": "k-1", "X-Gitlab-Event-UUID": "ev-1", "X-Gitlab-Webhook-UUID": "hook-a"}}
	if rec := q.do(t, h, "/"); rec.Code != 202 {
		t.Fatalf("%d", rec.Code)
	}
	if !st.deliveries["gitlab:hook-a:k-1"] {
		t.Fatalf("delivery ids %v", st.deliveries)
	}
	if rec := q.do(t, h, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// The same event delivered to another project's hook (own secret, own webhook uuid) is processed.
	q2 := hookReq{fixture: "gitlab_push.json", headers: map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "s3cret-two",
		"Idempotency-Key": "k-1", "X-Gitlab-Webhook-UUID": "hook-b"}}
	if rec := q2.do(t, h, "/"); rec.Code != 202 {
		t.Fatalf("%d", rec.Code)
	}
	if got := requests(st.calls); len(got) != 2 || got[0].id != 1 || got[1].id != 2 {
		t.Fatalf("calls %+v", got)
	}
	// A processing failure forgets the delivery so GitLab's retry goes through.
	st.failNext = true
	q3 := hookReq{fixture: "gitlab_push.json", headers: map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "s3cret-one", "X-Gitlab-Event-UUID": "ev-9"}}
	if rec := q3.do(t, h, "/"); rec.Code != 500 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := q3.do(t, h, "/"); rec.Code != 202 {
		t.Fatalf("retry %d %s", rec.Code, rec.Body)
	}
}

func TestBitbucketWebhook(t *testing.T) {
	bb := func(event, req string) map[string]string {
		return map[string]string{"X-Event-Key": event, "X-Request-UUID": req, "X-Hook-UUID": "{hook-1}"}
	}
	for _, tc := range []struct {
		name   string
		req    hookReq
		code   int
		result string
		want   []call
	}{
		{"push → full scan", hookReq{fixture: "bitbucket_push.json", headers: bb("repo:push", "r1"), sign: "s3cret-one"}, 202, "queued",
			[]call{{op: "request", id: 5, paths: []string{store.FullScan}}}},
		{"other branch ignored", hookReq{fixture: "bitbucket_push_feature.json", headers: bb("repo:push", "r2"), sign: "s3cret-one"}, 200, "ignored", nil},
		{"branch deletion ignored", hookReq{fixture: "bitbucket_branch_delete.json", headers: bb("repo:push", "r3"), sign: "s3cret-one"}, 200, "ignored", nil},
		{"other event ignored", hookReq{fixture: "bitbucket_push.json", headers: bb("pullrequest:created", "r4"), sign: "s3cret-one"}, 200, "ignored", nil},
		{"bad signature", hookReq{fixture: "bitbucket_push.json", headers: bb("repo:push", "r5"), sign: "s3cret-two"}, 401, "unauthorized", nil},
		{"unsigned", hookReq{fixture: "bitbucket_push.json", headers: bb("repo:push", "r6")}, 401, "unauthorized", nil},
		{"tampered body", hookReq{body: strings.Replace(string(fixture(t, "bitbucket_push.json")), "add refunds", "evil", 1), headers: map[string]string{
			"X-Event-Key": "repo:push", "X-Hub-Signature": githubapp.Sign([]byte("s3cret-one"), fixture(t, "bitbucket_push.json"))}}, 401, "unauthorized", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{deliveries: map[string]bool{}, repos: hookRepos()}
			h := repoHook("bitbucket", st)
			rec := tc.req.do(t, h, "/v1/webhooks/bitbucket")
			if rec.Code != tc.code {
				t.Fatalf("code %d %s", rec.Code, rec.Body)
			}
			if got := requests(st.calls); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("calls %+v want %+v", got, tc.want)
			}
			label := "bitbucket:repo:push"
			if tc.req.headers["X-Event-Key"] != "repo:push" {
				label = "bitbucket:other"
			}
			if h.Metrics.Get("codeintel_webhooks_total", "event", label, "result", tc.result) != 1 {
				t.Fatalf("metric %s/%s missing", label, tc.result)
			}
		})
	}

	// duplicate delivery
	st := &fakeStore{deliveries: map[string]bool{}, repos: hookRepos()}
	h := repoHook("bitbucket", st)
	q := hookReq{fixture: "bitbucket_push.json", headers: bb("repo:push", "dup-1"), sign: "s3cret-one"}
	if rec := q.do(t, h, "/"); rec.Code != 202 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := q.do(t, h, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "duplicate") || len(requests(st.calls)) != 1 {
		t.Fatalf("%d %s %+v", rec.Code, rec.Body, st.calls)
	}
	if !st.deliveries["bitbucket:{hook-1}:dup-1"] {
		t.Fatalf("delivery ids %v", st.deliveries)
	}
}

func TestRepoHookTooLarge(t *testing.T) {
	h := repoHook("gitlab", &fakeStore{deliveries: map[string]bool{}})
	req := httptest.NewRequest(http.MethodPost, "/", io.LimitReader(zeros{}, MaxBody+2))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatal(rec.Code)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}
