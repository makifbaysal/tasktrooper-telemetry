// Package webhook handles `POST /v1/webhooks/github` (contracts §18) — and, in repohook.go, the per-repository GitLab
// and Bitbucket webhooks (contracts §18a). GitHub: App deliveries (and classic repository
// webhooks) signed with X-Hub-Signature-256. push on the default branch → analyze_requested_at + pending_paths;
// installation_repositories removed/added → repository disconnected/reconnected; installation deleted/suspended →
// github integration error + repositories disconnected. Idempotent by X-GitHub-Delivery.
package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tasktrooper/telemetry/services/pkg/githubapp"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/obs"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

// MaxBody is GitHub's payload cap.
const MaxBody = 25 << 20

// GitHub lists at most this many commits in a push payload; a push with more → full scan.
const maxPushCommits = 20

// Store is what the handler writes (implemented by store.Store).
type Store interface {
	RecordDelivery(ctx context.Context, id, event string) (bool, error)
	ForgetDelivery(ctx context.Context, id string) error
	ReposByFullName(ctx context.Context, fullName string) ([]model.Repo, error)
	RequestAnalysis(ctx context.Context, id int64, paths []string) error
	SetInstallationRepos(ctx context.Context, installationID int64, fullNames []string, connected bool) (int64, error)
	InstallationGone(ctx context.Context, installationID int64, reason string) error
	InstallationBack(ctx context.Context, installationID int64) error
	WebhookReceived(ctx context.Context, ids []int64) error
}

// Handler serves the webhook.
type Handler struct {
	Store Store
	// Secret is the GitHub App webhook secret (TT_GITHUB_WEBHOOK_SECRET); verifies every event.
	Secret []byte
	// RepoSecret resolves a repository's webhook_secret_ref (classic repository webhooks; push only).
	RepoSecret func(ctx context.Context, repo model.Repo) ([]byte, error)
	Metrics    *obs.Registry
	Log        *slog.Logger
}

type payload struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	RepositoriesAdded []struct {
		FullName string `json:"full_name"`
	} `json:"repositories_added"`
	RepositoriesRemoved []struct {
		FullName string `json:"full_name"`
	} `json:"repositories_removed"`
}

func (h *Handler) count(event, result string) {
	if h.Metrics != nil {
		h.Metrics.Add("codeintel_webhooks_total", 1, "event", event, "result", result)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		h.count(event, "too_large")
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		h.count(event, "bad_request")
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	sig := r.Header.Get("X-Hub-Signature-256")
	appSigned := githubapp.VerifySignature(h.Secret, body, sig)
	var repos []model.Repo // push targets this delivery may touch
	if event == "push" || !appSigned {
		all, err := h.Store.ReposByFullName(ctx, p.Repository.FullName)
		if err != nil {
			log.Error("webhook repo lookup", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, repo := range all {
			if appSigned {
				repos = append(repos, repo)
			} else if repo.WebhookSecretRef != "" && h.RepoSecret != nil {
				if sec, err := h.RepoSecret(ctx, repo); err == nil && githubapp.VerifySignature(sec, body, sig) {
					repos = append(repos, repo)
				}
			}
		}
		// Only push may be authorized by a repository secret; App-level events need the App secret.
		if !appSigned && (event != "push" || len(repos) == 0) {
			h.count(event, "unauthorized")
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if !appSigned { // classic repository webhook verified: the repo now shows an active webhook
			ids := make([]int64, len(repos))
			for i, repo := range repos {
				ids[i] = repo.ID
			}
			if err := h.Store.WebhookReceived(ctx, ids); err != nil {
				log.Warn("webhook received_at", "err", err)
			}
		}
	}
	if delivery != "" {
		fresh, err := h.Store.RecordDelivery(ctx, delivery, event)
		if err != nil {
			log.Error("webhook delivery record", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !fresh {
			h.count(event, "duplicate")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("duplicate delivery\n"))
			return
		}
	}
	status, msg, err := h.handle(ctx, event, &p, repos)
	if err != nil {
		if delivery != "" {
			_ = h.Store.ForgetDelivery(context.WithoutCancel(ctx), delivery) // let GitHub's redelivery through
		}
		log.Error("webhook", "event", event, "delivery", delivery, "err", err)
		h.count(event, "error")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.count(event, msg)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

func (h *Handler) handle(ctx context.Context, event string, p *payload, repos []model.Repo) (int, string, error) {
	switch event {
	case "ping":
		return http.StatusOK, "pong", nil
	case "push":
		return h.push(ctx, p, repos)
	case "installation_repositories":
		if p.Installation.ID == 0 {
			return http.StatusOK, "ignored", nil
		}
		var n int64
		for _, set := range []struct {
			list []struct {
				FullName string `json:"full_name"`
			}
			connected bool
		}{{p.RepositoriesRemoved, false}, {p.RepositoriesAdded, true}} {
			names := make([]string, 0, len(set.list))
			for _, r := range set.list {
				names = append(names, r.FullName)
			}
			c, err := h.Store.SetInstallationRepos(ctx, p.Installation.ID, names, set.connected)
			if err != nil {
				return 0, "", err
			}
			n += c
		}
		if n == 0 {
			return http.StatusOK, "ignored", nil
		}
		return http.StatusOK, "updated", nil
	case "installation":
		if p.Installation.ID == 0 {
			return http.StatusOK, "ignored", nil
		}
		switch p.Action {
		case "deleted":
			return http.StatusOK, "updated", h.Store.InstallationGone(ctx, p.Installation.ID, "GitHub App installation was removed")
		case "suspend":
			return http.StatusOK, "updated", h.Store.InstallationGone(ctx, p.Installation.ID, "GitHub App installation is suspended")
		case "unsuspend":
			return http.StatusOK, "updated", h.Store.InstallationBack(ctx, p.Installation.ID)
		}
		return http.StatusOK, "ignored", nil
	}
	return http.StatusOK, "ignored", nil
}

func (h *Handler) push(ctx context.Context, p *payload, repos []model.Repo) (int, string, error) {
	if p.Deleted || strings.Trim(p.After, "0") == "" {
		return http.StatusOK, "ignored", nil
	}
	paths := ChangedPaths(p)
	queued := 0
	for _, repo := range repos {
		if repo.Status == "disconnected" {
			continue
		}
		branch := repo.DefaultBranch
		if branch == "" {
			branch = p.Repository.DefaultBranch
		}
		if p.Ref != "refs/heads/"+branch {
			continue
		}
		if err := h.Store.RequestAnalysis(ctx, repo.ID, paths); err != nil {
			return 0, "", err
		}
		queued++
	}
	if queued == 0 {
		return http.StatusOK, "ignored", nil
	}
	return http.StatusAccepted, "queued", nil
}

// ChangedPaths is the union of added/modified/removed files; a truncated commit list (or none) → full scan.
func ChangedPaths(p *payload) []string {
	if len(p.Commits) == 0 || len(p.Commits) >= maxPushCommits {
		return []string{store.FullScan}
	}
	var all []string
	for _, c := range p.Commits {
		all = append(all, c.Added...)
		all = append(all, c.Modified...)
		all = append(all, c.Removed...)
	}
	return store.MergePaths(nil, all)
}
