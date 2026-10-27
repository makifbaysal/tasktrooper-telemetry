package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tasktrooper/telemetry/services/pkg/githubapp"
	"github.com/tasktrooper/telemetry/services/pkg/vcs"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/obs"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

// HookStore is what the GitLab / Bitbucket handler reads and writes (implemented by store.Store).
type HookStore interface {
	RecordDelivery(ctx context.Context, id, event string) (bool, error)
	ForgetDelivery(ctx context.Context, id string) error
	ReposByName(ctx context.Context, provider, fullName string) ([]model.Repo, error)
	RequestAnalysis(ctx context.Context, id int64, paths []string) error
	WebhookReceived(ctx context.Context, ids []int64) error
}

// RepoHook serves the per-repository webhooks of GitLab (`POST /v1/webhooks/gitlab`) and Bitbucket Cloud
// (`POST /v1/webhooks/bitbucket`), contracts §18a. Every delivery must be authenticated by the webhook secret of a
// repository with that provider + full name (GitLab: X-Gitlab-Token, constant-time; Bitbucket: X-Hub-Signature
// HMAC-SHA256); only those repositories are touched. A push to the default branch queues an analysis with the
// changed files (GitLab) or a full-scan marker (Bitbucket lists no files; the git-diff incremental narrows it).
type RepoHook struct {
	Provider string // vcs.GitLab | vcs.Bitbucket
	Store    HookStore
	// RepoSecret resolves a repository's webhook_secret_ref.
	RepoSecret func(ctx context.Context, repo model.Repo) ([]byte, error)
	Metrics    *obs.Registry
	Log        *slog.Logger
}

// pushEvent is the provider-neutral view of a delivery.
type pushEvent struct {
	push          bool     // a branch push (other events are verified, then ignored)
	fullName      string   // repository full name
	branches      []string // branches that now point to a new commit
	defaultBranch string   // from the payload, when it carries one
	paths         []string // changed files, or [FullScan]
}

type gitlabPayload struct {
	ObjectKind        string  `json:"object_kind"`
	Ref               string  `json:"ref"`
	After             string  `json:"after"`
	CheckoutSHA       *string `json:"checkout_sha"`
	TotalCommitsCount int     `json:"total_commits_count"`
	Project           struct {
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
	} `json:"project"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
}

type bitbucketPayload struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Push struct {
		Changes []struct {
			New *struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"new"`
		} `json:"changes"`
	} `json:"push"`
}

// gitlabEvent reads a GitLab delivery; ok=false when the body is not JSON.
func gitlabEvent(header string, body []byte) (pushEvent, bool) {
	var p gitlabPayload
	if json.Unmarshal(body, &p) != nil {
		return pushEvent{}, false
	}
	ev := pushEvent{fullName: p.Project.PathWithNamespace, defaultBranch: p.Project.DefaultBranch}
	if header != "Push Hook" || p.ObjectKind != "push" {
		return ev, true
	}
	ev.push = true
	branch, isBranch := strings.CutPrefix(p.Ref, "refs/heads/")
	deleted := strings.Trim(p.After, "0") == "" || p.CheckoutSHA == nil
	if isBranch && !deleted {
		ev.branches = []string{branch}
	}
	// GitLab lists at most 20 commits; total_commits_count tells when the list was cut.
	if len(p.Commits) == 0 || p.TotalCommitsCount > len(p.Commits) {
		ev.paths = []string{store.FullScan}
		return ev, true
	}
	var all []string
	for _, c := range p.Commits {
		all = append(all, c.Added...)
		all = append(all, c.Modified...)
		all = append(all, c.Removed...)
	}
	ev.paths = store.MergePaths(nil, all)
	if len(ev.paths) == 0 {
		ev.paths = []string{store.FullScan}
	}
	return ev, true
}

func bitbucketEvent(header string, body []byte) (pushEvent, bool) {
	var p bitbucketPayload
	if json.Unmarshal(body, &p) != nil {
		return pushEvent{}, false
	}
	ev := pushEvent{fullName: p.Repository.FullName}
	if header != "repo:push" {
		return ev, true
	}
	ev.push = true
	for _, c := range p.Push.Changes {
		if c.New != nil && c.New.Type == "branch" && c.New.Name != "" { // new == null: branch deleted
			ev.branches = append(ev.branches, c.New.Name)
		}
	}
	ev.paths = []string{store.FullScan} // push payloads carry no file list
	return ev, true
}

func (h *RepoHook) count(event, result string) {
	if h.Metrics != nil {
		h.Metrics.Add("codeintel_webhooks_total", 1, "event", event, "result", result)
	}
}

// headers returns the event header, the metric label (bounded) and the delivery id ("" = no idempotency).
func (h *RepoHook) headers(r *http.Request) (event, label, delivery string) {
	join := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) < 2 { // provider prefix only
			return ""
		}
		id := strings.Join(out, ":")
		if len(id) > 300 {
			id = id[:300]
		}
		return id
	}
	switch h.Provider {
	case vcs.GitLab:
		event = r.Header.Get("X-Gitlab-Event")
		label = "gitlab:other"
		if event == "Push Hook" {
			label = "gitlab:push"
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			key = r.Header.Get("X-Gitlab-Event-UUID")
		}
		if key != "" {
			delivery = join("gitlab", r.Header.Get("X-Gitlab-Webhook-UUID"), key)
		}
	default:
		event = r.Header.Get("X-Event-Key")
		label = "bitbucket:other"
		if event == "repo:push" {
			label = "bitbucket:repo:push"
		}
		if id := r.Header.Get("X-Request-UUID"); id != "" {
			delivery = join("bitbucket", r.Header.Get("X-Hook-UUID"), id)
		}
	}
	return event, label, delivery
}

// verify reports whether the delivery carries this repository's webhook secret.
func (h *RepoHook) verify(ctx context.Context, r *http.Request, body []byte, repo model.Repo) bool {
	if repo.WebhookSecretRef == "" || h.RepoSecret == nil {
		return false
	}
	sec, err := h.RepoSecret(ctx, repo)
	if err != nil || len(sec) == 0 {
		return false
	}
	if h.Provider == vcs.GitLab {
		got := []byte(r.Header.Get("X-Gitlab-Token"))
		return len(got) > 0 && subtle.ConstantTimeCompare(got, sec) == 1
	}
	return githubapp.VerifySignature(sec, body, r.Header.Get("X-Hub-Signature"))
}

func (h *RepoHook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	event, label, delivery := h.headers(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		h.count(label, "too_large")
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	parse := bitbucketEvent
	if h.Provider == vcs.GitLab {
		parse = gitlabEvent
	}
	ev, ok := parse(event, body)
	if !ok {
		h.count(label, "bad_request")
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var repos []model.Repo
	if ev.fullName != "" {
		all, err := h.Store.ReposByName(ctx, h.Provider, ev.fullName)
		if err != nil {
			log.Error("webhook repo lookup", "provider", h.Provider, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, repo := range all {
			if h.verify(ctx, r, body, repo) {
				repos = append(repos, repo)
			}
		}
	}
	if len(repos) == 0 {
		h.count(label, "unauthorized")
		http.Error(w, "invalid signature or token", http.StatusUnauthorized)
		return
	}
	ids := make([]int64, len(repos))
	for i, repo := range repos {
		ids[i] = repo.ID
	}
	if err := h.Store.WebhookReceived(ctx, ids); err != nil {
		log.Warn("webhook received_at", "provider", h.Provider, "err", err)
	}
	if delivery != "" {
		fresh, err := h.Store.RecordDelivery(ctx, delivery, label)
		if err != nil {
			log.Error("webhook delivery record", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !fresh {
			h.count(label, "duplicate")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("duplicate delivery\n"))
			return
		}
	}
	status, msg, err := h.push(ctx, ev, repos)
	if err != nil {
		if delivery != "" {
			_ = h.Store.ForgetDelivery(context.WithoutCancel(ctx), delivery) // let the provider's retry through
		}
		log.Error("webhook", "provider", h.Provider, "event", label, "err", err)
		h.count(label, "error")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.count(label, msg)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

func (h *RepoHook) push(ctx context.Context, ev pushEvent, repos []model.Repo) (int, string, error) {
	if !ev.push || len(ev.branches) == 0 {
		return http.StatusOK, "ignored", nil
	}
	queued := 0
	for _, repo := range repos {
		if repo.Status == "disconnected" {
			continue
		}
		branch := repo.DefaultBranch
		if branch == "" {
			branch = ev.defaultBranch
		}
		hit := false
		for _, b := range ev.branches {
			hit = hit || (branch != "" && b == branch)
		}
		if !hit {
			continue
		}
		if err := h.Store.RequestAnalysis(ctx, repo.ID, ev.paths); err != nil {
			return 0, "", err
		}
		queued++
	}
	if queued == 0 {
		return http.StatusOK, "ignored", nil
	}
	return http.StatusAccepted, "queued", nil
}
