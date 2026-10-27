package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/tasktrooper/telemetry/services/pkg/githubapp"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/obs"
)

type call struct {
	op    string
	id    int64
	paths []string
	names []string
	ok    bool
}

type fakeStore struct {
	mu         sync.Mutex
	deliveries map[string]bool
	repos      []model.Repo
	calls      []call
	failNext   bool
}

func (s *fakeStore) RecordDelivery(_ context.Context, id, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deliveries[id] {
		return false, nil
	}
	s.deliveries[id] = true
	return true, nil
}
func (s *fakeStore) ForgetDelivery(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.deliveries, id)
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) ReposByFullName(_ context.Context, name string) ([]model.Repo, error) {
	var out []model.Repo
	for _, r := range s.repos {
		if strings.EqualFold(r.FullName, name) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *fakeStore) RequestAnalysis(_ context.Context, id int64, paths []string) error {
	if s.failNext {
		s.failNext = false
		return errors.New("db down")
	}
	s.calls = append(s.calls, call{op: "request", id: id, paths: paths})
	return nil
}
func (s *fakeStore) SetInstallationRepos(_ context.Context, inst int64, names []string, connected bool) (int64, error) {
	if len(names) == 0 {
		return 0, nil
	}
	s.calls = append(s.calls, call{op: "repos", id: inst, names: names, ok: connected})
	return int64(len(names)), nil
}
func (s *fakeStore) InstallationGone(_ context.Context, inst int64, _ string) error {
	s.calls = append(s.calls, call{op: "gone", id: inst})
	return nil
}
func (s *fakeStore) InstallationBack(_ context.Context, inst int64) error {
	s.calls = append(s.calls, call{op: "back", id: inst})
	return nil
}

var appSecret = []byte("app-secret")

func fixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func send(h http.Handler, event, delivery string, body, secret []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if secret != nil {
		req.Header.Set("X-Hub-Signature-256", githubapp.Sign(secret, body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newHandler(st *fakeStore) *Handler {
	return &Handler{Store: st, Secret: appSecret, Metrics: obs.NewRegistry(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		RepoSecret: func(_ context.Context, r model.Repo) ([]byte, error) {
			if r.WebhookSecretRef == "env:REPO_SECRET" {
				return []byte("repo-secret"), nil
			}
			return nil, fmt.Errorf("unknown ref")
		}}
}

func TestPushQueuesIncrementalAnalysis(t *testing.T) {
	st := &fakeStore{deliveries: map[string]bool{}, repos: []model.Repo{
		{ID: 1, FullName: "acme/shop", DefaultBranch: "main", Status: "ready"},
		{ID: 2, FullName: "acme/shop", DefaultBranch: "main", Status: "disconnected"},
		{ID: 3, FullName: "acme/shop", DefaultBranch: "release", Status: "ready"},
	}}
	h := newHandler(st)
	body := fixture(t, "push.json")
	if rec := send(h, "push", "d-1", body, appSecret); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	want := []call{{op: "request", id: 1, paths: []string{"services/orders/checkout.go", "services/orders/legacy.go", "services/orders/main.go"}}}
	if !reflect.DeepEqual(st.calls, want) {
		t.Fatalf("calls %+v", st.calls)
	}
	// Redelivery of the same id is a no-op.
	if rec := send(h, "push", "d-1", body, appSecret); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(st.calls) != 1 || h.Metrics.Get("codeintel_webhooks_total", "event", "push", "result", "duplicate") != 1 {
		t.Fatal("duplicate processed")
	}
	// Other branch: ignored.
	if rec := send(h, "push", "d-2", fixture(t, "push_feature.json"), appSecret); rec.Code != http.StatusOK || len(st.calls) != 1 {
		t.Fatalf("%d %+v", rec.Code, st.calls)
	}
}

func TestPushSignatureAndRepoSecret(t *testing.T) {
	st := &fakeStore{deliveries: map[string]bool{}, repos: []model.Repo{
		{ID: 1, FullName: "acme/shop", DefaultBranch: "main", WebhookSecretRef: "env:REPO_SECRET"},
		{ID: 2, FullName: "acme/shop", DefaultBranch: "main"}, // other project, no secret
	}}
	h := newHandler(st)
	body := fixture(t, "push.json")
	for name, sec := range map[string][]byte{"unsigned": nil, "wrong": []byte("nope")} {
		if rec := send(h, "push", "x-"+name, body, sec); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
	if len(st.deliveries) != 0 {
		t.Fatal("unauthorized delivery recorded")
	}
	// Repository webhook secret authorizes only the repo that owns it.
	// The verified classic webhook also records webhook_received_at for that repo.
	if rec := send(h, "push", "r-1", body, []byte("repo-secret")); rec.Code != http.StatusAccepted || len(st.calls) != 2 ||
		st.calls[0].op != "seen" || st.calls[0].names[0] != "[1]" || st.calls[1].id != 1 {
		t.Fatalf("%d %+v", rec.Code, st.calls)
	}
	// A repo secret never authorizes App-level events.
	if rec := send(h, "installation", "r-2", fixture(t, "installation_deleted.json"), []byte("repo-secret")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d", rec.Code)
	}
}

func TestInstallationEvents(t *testing.T) {
	st := &fakeStore{deliveries: map[string]bool{}}
	h := newHandler(st)
	if rec := send(h, "installation_repositories", "i-1", fixture(t, "installation_repositories.json"), appSecret); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := send(h, "installation", "i-2", fixture(t, "installation_deleted.json"), appSecret); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if rec := send(h, "ping", "i-3", fixture(t, "ping.json"), appSecret); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("%d", rec.Code)
	}
	want := []call{{op: "repos", id: 4242, names: []string{"acme/shop"}, ok: false}, {op: "gone", id: 4242}}
	if !reflect.DeepEqual(st.calls, want) {
		t.Fatalf("calls %+v", st.calls)
	}
}

func TestFailureForgetsDelivery(t *testing.T) {
	st := &fakeStore{deliveries: map[string]bool{}, repos: []model.Repo{{ID: 1, FullName: "acme/shop", DefaultBranch: "main"}}, failNext: true}
	h := newHandler(st)
	body := fixture(t, "push.json")
	if rec := send(h, "push", "f-1", body, appSecret); rec.Code != http.StatusInternalServerError {
		t.Fatalf("%d", rec.Code)
	}
	if rec := send(h, "push", "f-1", body, appSecret); rec.Code != http.StatusAccepted {
		t.Fatalf("redelivery after failure: %d", rec.Code)
	}
}

func TestChangedPaths(t *testing.T) {
	p := &payload{}
	if got := ChangedPaths(p); !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatal(got)
	}
	for range maxPushCommits {
		p.Commits = append(p.Commits, struct {
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		}{Added: []string{"a"}})
	}
	if got := ChangedPaths(p); !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatal("truncated commit list must force a full scan")
	}
	p.Commits = p.Commits[:1]
	p.Commits[0].Modified = make([]string, 250)
	for i := range p.Commits[0].Modified {
		p.Commits[0].Modified[i] = fmt.Sprintf("f%d.go", i)
	}
	if got := ChangedPaths(p); !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatal(">200 files must force a full scan")
	}
}
