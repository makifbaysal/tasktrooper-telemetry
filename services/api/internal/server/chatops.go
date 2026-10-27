package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/slack"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/githubapp"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// ChatOpsStore is the Slack App / GitHub App part of Store (ADR-017).
type ChatOpsStore interface {
	SlackInstallOf(ctx context.Context, orgID int64) (*store.SlackInstall, error)
	SaveSlackInstall(ctx context.Context, orgID int64, teamID, teamName, botUserID, scopes, token string) (int64, error)
	DeleteSlackInstall(ctx context.Context, orgID int64) error
	SlackStatus(ctx context.Context, orgID int64) (model.SlackStatus, error)
	TenantOfIncident(ctx context.Context, id int64) (tenant.Tenant, error)
	TenantOfMonitor(ctx context.Context, id int64) (tenant.Tenant, error)
	MemberByEmail(ctx context.Context, orgID int64, email string) (int64, string, error)
	SaveGitHubInstall(ctx context.Context, installationID int64, account, accountType string) (int64, error)
	GitHubInstalls(ctx context.Context) ([]model.GitHubInstall, error)
	GitHubInstallation(ctx context.Context, integrationID string) (int64, error)
	RepoNames(ctx context.Context, provider string) (map[string]bool, error)
}

// SlackApp configures the Slack App (TT_SLACK_CLIENT_ID / _SECRET / TT_SLACK_SIGNING_SECRET).
type SlackApp struct {
	ClientID      string
	ClientSecret  string
	SigningSecret string
	API           *slack.Client
	AuthorizeURL  string // default https://slack.com/oauth/v2/authorize
}

// GitHubApp configures the GitHub App install flow (TT_GITHUB_APP_*).
type GitHubApp struct {
	Client *githubapp.Client
	Slug   string // TT_GITHUB_APP_SLUG
	WebURL string // https://github.com (GHE: https://ghe.example.com)
}

// ---------------------------------------------------------------- signed state

// installState is carried through the provider's OAuth/install redirect (HMAC-signed, 15 minutes).
type installState struct {
	Kind     string `json:"k"`
	Org      int64  `json:"o"`
	Project  int64  `json:"p"`
	User     int64  `json:"u,omitempty"`
	Redirect string `json:"r,omitempty"`
	Exp      int64  `json:"e"`
	Nonce    string `json:"n"`
}

func (s *Server) stateKey() []byte {
	h := sha256.Sum256(append([]byte("tasktrooper-install-state:"), s.StateKey...))
	return h[:]
}

func (s *Server) signState(st installState) string {
	st.Exp, st.Nonce = s.Now().Add(15*time.Minute).Unix(), store.RandomToken(9)
	b, _ := json.Marshal(st)
	payload := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, s.stateKey())
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

var errBadState = errors.New("invalid or expired install state")

func (s *Server) verifyState(raw, kind string) (installState, error) {
	var st installState
	payload, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return st, errBadState
	}
	m := hmac.New(sha256.New, s.stateKey())
	m.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return st, errBadState
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(b, &st) != nil || st.Kind != kind || s.Now().Unix() > st.Exp || st.Project == 0 {
		return installState{}, errBadState
	}
	return st, nil
}

func (s *Server) publicAPI() string {
	if s.PublicAPIURL == "" {
		return "http://localhost:8080"
	}
	return strings.TrimRight(s.PublicAPIURL, "/")
}

// withQuery appends k=v to a URL.
func withQuery(raw string, kv ...string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) installRedirect(r *http.Request, def string) string {
	if v := qs(r, "redirect"); v != "" {
		return s.safeRedirect(v)
	}
	return s.safeRedirect(def)
}

// ---------------------------------------------------------------- routes

// chatOpsRoutes are the authenticated Slack/GitHub App endpoints.
func (s *Server) chatOpsRoutes(r chi.Router) {
	r.Get("/integrations/slack", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.SlackStatus(r.Context(), principalOf(r.Context()).OrgID)
		s.reply(w, r, v, err)
	})
	r.Get("/integrations/slack/channels", s.slackChannels)
	r.Get("/integrations/github", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GitHubInstalls(r.Context())
		s.reply(w, r, v, err)
	})
	r.Get("/integrations/github/{id}/repos", s.githubRepos)
	r.Group(func(r chi.Router) {
		r.Use(s.adminOnly)
		r.Get("/integrations/slack/install", func(w http.ResponseWriter, r *http.Request) {
			if u, ok := s.slackInstallURL(w, r); ok {
				http.Redirect(w, r, u, http.StatusFound)
			}
		})
		r.Post("/integrations/slack/install-url", func(w http.ResponseWriter, r *http.Request) {
			if u, ok := s.slackInstallURL(w, r); ok {
				writeJSON(w, http.StatusOK, map[string]string{"url": u})
			}
		})
		r.Delete("/integrations/slack", func(w http.ResponseWriter, r *http.Request) {
			org := principalOf(r.Context()).OrgID
			err := s.Store.DeleteSlackInstall(r.Context(), org)
			s.forgetChannels(org)
			s.noContent(w, r, err)
		})
		r.Get("/integrations/github/install", func(w http.ResponseWriter, r *http.Request) {
			if u, ok := s.githubInstallURL(w, r); ok {
				http.Redirect(w, r, u, http.StatusFound)
			}
		})
		r.Post("/integrations/github/install-url", func(w http.ResponseWriter, r *http.Request) {
			if u, ok := s.githubInstallURL(w, r); ok {
				writeJSON(w, http.StatusOK, map[string]string{"url": u})
			}
		})
	})
}

// chatOpsPublicRoutes are reached by Slack / GitHub / the browser without our auth (signed state or signature).
func (s *Server) chatOpsPublicRoutes(r chi.Router) {
	r.Get("/integrations/slack/callback", s.slackCallback)
	r.Get("/integrations/github/callback", s.githubCallback)
	r.Post("/slack/interactions", s.slackInteractions)
}

func notConfigured(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotImplemented, apiError{Error: what + " is not configured on this server"})
}

func (s *Server) slackInstallURL(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.Slack == nil {
		notConfigured(w, "the Slack App (TT_SLACK_CLIENT_ID)")
		return "", false
	}
	p := principalOf(r.Context())
	state := s.signState(installState{Kind: "slack", Org: p.OrgID, Project: p.ProjectID, User: p.UserID,
		Redirect: s.installRedirect(r, "/settings/integrations")})
	base := s.Slack.AuthorizeURL
	if base == "" {
		base = "https://slack.com/oauth/v2/authorize"
	}
	return withQuery(base, "client_id", s.Slack.ClientID, "scope", slack.Scopes, "state", state,
		"redirect_uri", s.publicAPI()+"/api/v1/integrations/slack/callback"), true
}

func (s *Server) githubInstallURL(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.GitHub == nil || s.GitHub.Client == nil || s.GitHub.Slug == "" {
		notConfigured(w, "the GitHub App (TT_GITHUB_APP_ID / TT_GITHUB_APP_SLUG)")
		return "", false
	}
	p := principalOf(r.Context())
	state := s.signState(installState{Kind: "github", Org: p.OrgID, Project: p.ProjectID, User: p.UserID,
		Redirect: s.installRedirect(r, "/settings/integrations")})
	base := strings.TrimRight(s.GitHub.WebURL, "/")
	if base == "" {
		base = "https://github.com"
	}
	return withQuery(base+"/apps/"+url.PathEscape(s.GitHub.Slug)+"/installations/new", "state", state), true
}

func stateCtx(ctx context.Context, st installState) context.Context {
	ctx = tenant.With(ctx, tenant.Tenant{OrgID: uint32(st.Org), ProjectID: uint32(st.Project)})
	return withPrincipal(ctx, &principal{UserID: st.User, OrgID: st.Org, ProjectID: st.Project, Role: "admin", Session: st.User > 0})
}

func (s *Server) slackCallback(w http.ResponseWriter, r *http.Request) {
	if s.Slack == nil {
		notConfigured(w, "the Slack App")
		return
	}
	st, err := s.verifyState(qs(r, "state"), "slack")
	if err != nil {
		http.Redirect(w, r, withQuery(s.safeRedirect("/settings/integrations"), "slack", "error", "message", err.Error()), http.StatusFound)
		return
	}
	back := s.safeRedirect(st.Redirect)
	if e := qs(r, "error"); e != "" { // access_denied when the user cancels
		http.Redirect(w, r, withQuery(back, "slack", "error", "message", e), http.StatusFound)
		return
	}
	ctx := stateCtx(r.Context(), st)
	res, err := s.Slack.API.OAuthAccess(ctx, s.Slack.ClientID, s.Slack.ClientSecret, qs(r, "code"), s.publicAPI()+"/api/v1/integrations/slack/callback")
	if err != nil {
		s.Log.Warn("slack oauth failed", "err", err)
		http.Redirect(w, r, withQuery(back, "slack", "error", "message", "Slack did not accept the installation"), http.StatusFound)
		return
	}
	id, err := s.Store.SaveSlackInstall(ctx, st.Org, res.Team.ID, res.Team.Name, res.BotUserID, res.Scope, res.AccessToken)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.forgetChannels(st.Org)
	s.writeAudit(ctx, store.AuditRecord{OrgID: st.Org, ProjectID: st.Project, UserID: st.User, Action: "slack.install",
		Target: strconv.FormatInt(id, 10), Meta: map[string]any{"teamId": res.Team.ID, "teamName": res.Team.Name}})
	http.Redirect(w, r, withQuery(back, "slack", "connected"), http.StatusFound)
}

func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	if s.GitHub == nil || s.GitHub.Client == nil {
		notConfigured(w, "the GitHub App")
		return
	}
	st, err := s.verifyState(qs(r, "state"), "github")
	if err != nil {
		http.Redirect(w, r, withQuery(s.safeRedirect("/settings/integrations"), "github", "error", "message", err.Error()), http.StatusFound)
		return
	}
	back := s.safeRedirect(st.Redirect)
	if qs(r, "setup_action") == "request" { // an org owner must approve the installation first
		http.Redirect(w, r, withQuery(back, "github", "pending"), http.StatusFound)
		return
	}
	inst, err := strconv.ParseInt(qs(r, "installation_id"), 10, 64)
	if err != nil || inst <= 0 {
		http.Redirect(w, r, withQuery(back, "github", "error", "message", "missing installation_id"), http.StatusFound)
		return
	}
	ctx := stateCtx(r.Context(), st)
	info, err := s.GitHub.Client.Installation(ctx, inst) // also proves the installation belongs to our app
	if err != nil {
		s.Log.Warn("github installation lookup failed", "err", err, "installation", inst)
		http.Redirect(w, r, withQuery(back, "github", "error", "message", "installation not found for this app"), http.StatusFound)
		return
	}
	id, err := s.Store.SaveGitHubInstall(ctx, inst, info.Account.Login, info.Account.Type)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeAudit(ctx, store.AuditRecord{OrgID: st.Org, ProjectID: st.Project, UserID: st.User, Action: "github.install",
		Target: strconv.FormatInt(id, 10), Meta: map[string]any{"installationId": inst, "account": info.Account.Login}})
	http.Redirect(w, r, withQuery(back, "github", strconv.FormatInt(id, 10)), http.StatusFound)
}

// ---------------------------------------------------------------- channel picker

type channelCache struct {
	mu sync.Mutex
	by map[int64]cachedChannels
}

type cachedChannels struct {
	at       time.Time
	channels []model.SlackChannel
}

func (s *Server) forgetChannels(org int64) {
	s.channels.mu.Lock()
	delete(s.channels.by, org)
	s.channels.mu.Unlock()
}

func (s *Server) slackChannels(w http.ResponseWriter, r *http.Request) {
	if s.Slack == nil {
		notConfigured(w, "the Slack App")
		return
	}
	org := principalOf(r.Context()).OrgID
	s.channels.mu.Lock()
	c, ok := s.channels.by[org]
	s.channels.mu.Unlock()
	if !ok || s.Now().Sub(c.at) > 5*time.Minute || qs(r, "refresh") == "true" {
		in, err := s.Store.SlackInstallOf(r.Context(), org)
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, store.Conflict("Slack is not connected: install the Slack App first"))
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		list, err := s.Slack.API.Channels(r.Context(), in.Token)
		if err != nil {
			s.Log.Warn("slack conversations.list failed", "err", err)
			writeJSON(w, http.StatusBadGateway, apiError{Error: "Slack: " + err.Error()})
			return
		}
		c = cachedChannels{at: s.Now(), channels: make([]model.SlackChannel, 0, len(list))}
		for _, ch := range list {
			c.channels = append(c.channels, model.SlackChannel{ID: ch.ID, Name: ch.Name, IsPrivate: ch.IsPrivate, IsMember: ch.IsMember})
		}
		sort.Slice(c.channels, func(i, j int) bool { return c.channels[i].Name < c.channels[j].Name })
		s.channels.mu.Lock()
		if s.channels.by == nil {
			s.channels.by = map[int64]cachedChannels{}
		}
		s.channels.by[org] = c
		s.channels.mu.Unlock()
	}
	q := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(qs(r, "q")), "#"))
	out := make([]model.SlackChannel, 0, len(c.channels))
	for _, ch := range c.channels {
		if q == "" || strings.Contains(strings.ToLower(ch.Name), q) {
			out = append(out, ch)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- GitHub repo picker

func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request) {
	if s.GitHub == nil || s.GitHub.Client == nil {
		notConfigured(w, "the GitHub App")
		return
	}
	inst, err := s.Store.GitHubInstallation(r.Context(), param(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	repos, err := s.GitHub.Client.InstallationRepos(r.Context(), inst)
	if err != nil {
		s.Log.Warn("github installation repos failed", "err", err, "installation", inst)
		writeJSON(w, http.StatusBadGateway, apiError{Error: "GitHub: " + err.Error()})
		return
	}
	added, err := s.Store.RepoNames(r.Context(), "github")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(qs(r, "q")))
	out := []model.GitHubRepo{}
	for _, x := range repos {
		if q != "" && !strings.Contains(strings.ToLower(x.FullName), q) {
			continue
		}
		out = append(out, model.GitHubRepo{FullName: x.FullName, DefaultBranch: x.DefaultBranch, Private: x.Private, Language: x.Language,
			Added: added[strings.ToLower(x.FullName)]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- interactivity

// Action ids sent by the notifier (services/alerting/internal/notify/slackapp.go).
const (
	slackActionAck    = "tt_ack:"    // + incident id
	slackActionMute1h = "tt_mute1h:" // + monitor id
)

type slackInteraction struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// slackInteractions handles Ack / Mute 1h button presses: signature check, tenant from the incident / monitor,
// the Slack team must be the org's installation, and the Slack user (by email) must be a member (not viewer).
// Slack gets 200 right away; the outcome goes back as an ephemeral message through response_url.
func (s *Server) slackInteractions(w http.ResponseWriter, r *http.Request) {
	if s.Slack == nil {
		notConfigured(w, "the Slack App")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid body"})
		return
	}
	if err := slack.Verify(s.Slack.SigningSecret, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body, s.Now()); err != nil {
		writeJSON(w, http.StatusUnauthorized, apiError{Error: err.Error()})
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid form body"})
		return
	}
	var in slackInteraction
	if err := json.Unmarshal([]byte(form.Get("payload")), &in); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid payload"})
		return
	}
	if in.Type != "block_actions" || len(in.Actions) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	msg := s.slackAction(r.Context(), in)
	if msg != "" {
		if err := s.Slack.API.Respond(r.Context(), in.ResponseURL, msg); err != nil {
			s.Log.Warn("slack response_url failed", "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) slackAction(ctx context.Context, in slackInteraction) string {
	a := in.Actions[0]
	var kind string
	var id int64
	switch {
	case strings.HasPrefix(a.ActionID, slackActionAck):
		kind = "ack"
		id, _ = strconv.ParseInt(strings.TrimPrefix(a.ActionID, slackActionAck), 10, 64)
	case strings.HasPrefix(a.ActionID, slackActionMute1h):
		kind = "mute"
		id, _ = strconv.ParseInt(strings.TrimPrefix(a.ActionID, slackActionMute1h), 10, 64)
	default:
		return "" // link buttons (tt_open) need nothing
	}
	if id <= 0 {
		return "This button is no longer valid."
	}
	var t tenant.Tenant
	var err error
	if kind == "ack" {
		t, err = s.Store.TenantOfIncident(ctx, id)
	} else {
		t, err = s.Store.TenantOfMonitor(ctx, id)
	}
	if errors.Is(err, store.ErrNotFound) {
		return "That incident or monitor no longer exists."
	}
	if err != nil {
		s.Log.Error("slack action", "err", err)
		return "TaskTrooper could not process this action, try again."
	}
	org := int64(t.OrgID)
	inst, err := s.Store.SlackInstallOf(ctx, org)
	if err != nil || inst.TeamID != in.Team.ID {
		return "This Slack workspace is not connected to that TaskTrooper org."
	}
	email, err := s.Slack.API.UserEmail(ctx, inst.Token, in.User.ID)
	if err != nil || email == "" {
		return "TaskTrooper could not read your Slack email (the app needs the users:read.email scope; reinstall it)."
	}
	uid, role, err := s.Store.MemberByEmail(ctx, org, email)
	if errors.Is(err, store.ErrNotFound) {
		return "No TaskTrooper member has the email " + email + ": ask an org admin to invite you."
	}
	if err != nil {
		s.Log.Error("slack action member lookup", "err", err)
		return "TaskTrooper could not process this action, try again."
	}
	if !store.RoleAtLeast(role, "member") {
		return "Viewers cannot acknowledge or mute alerts."
	}
	ctx = tenant.With(ctx, t)
	ctx = store.WithActor(ctx, store.Actor{UserID: uid, Email: email, Via: "slack"})
	p := &principal{UserID: uid, OrgID: org, ProjectID: int64(t.ProjectID), Role: role, Session: true}
	target := strconv.FormatInt(id, 10)
	switch kind {
	case "ack":
		_, err = s.Store.TransitionIncident(ctx, target, "ack")
		if errors.Is(err, store.ErrConflict) {
			return "This incident is already acknowledged or resolved."
		}
	case "mute":
		until := s.Now().Add(time.Hour).UnixMilli()
		_, err = s.Store.PatchMonitor(ctx, target, model.MonitorPatch{MutedUntil: &until, MutedUntilSet: true})
	}
	if err != nil {
		s.Log.Error("slack action failed", "err", err, "action", kind)
		return "TaskTrooper could not process this action, try again."
	}
	s.writeAudit(ctx, store.AuditRecord{OrgID: org, ProjectID: p.ProjectID, UserID: uid, Action: "slack." + kind, Target: target,
		Meta: map[string]any{"slackUser": in.User.ID}})
	if kind == "ack" {
		return "Acknowledged by " + email + "."
	}
	return "Muted for 1 hour by " + email + "."
}
