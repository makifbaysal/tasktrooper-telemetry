package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/slack"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/githubapp"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

type chatFake struct {
	*identityFake
	mu          sync.Mutex
	slackSaved  []string
	ghSaved     []int64
	acked       []string
	ackActor    store.Actor
	ackTenant   tenant.Tenant
	muted       map[string]int64
	slackDelete int
}

func (f *chatFake) SlackInstallOf(_ context.Context, org int64) (*store.SlackInstall, error) {
	if org != 7 {
		return nil, store.ErrNotFound
	}
	return &store.SlackInstall{ID: 1, ProjectID: 42, TeamID: "T1", TeamName: "Acme", Token: "xoxb-1"}, nil
}

func (f *chatFake) SaveSlackInstall(_ context.Context, org int64, team, name, bot, scopes, token string) (int64, error) {
	f.slackSaved = append(f.slackSaved, strings.Join([]string{strconv.FormatInt(org, 10), team, name, bot, token}, "|"))
	return 11, nil
}

func (f *chatFake) DeleteSlackInstall(context.Context, int64) error {
	f.slackDelete++
	return nil
}

func (f *chatFake) TenantOfIncident(_ context.Context, id int64) (tenant.Tenant, error) {
	if id == 404 {
		return tenant.Tenant{}, store.ErrNotFound
	}
	if id == 900 {
		return tenant.Tenant{OrgID: 9, ProjectID: 90}, nil
	}
	return tenant.Tenant{OrgID: 7, ProjectID: 42}, nil
}

func (f *chatFake) TenantOfMonitor(context.Context, int64) (tenant.Tenant, error) {
	return tenant.Tenant{OrgID: 7, ProjectID: 42}, nil
}

func (f *chatFake) MemberByEmail(_ context.Context, org int64, email string) (int64, string, error) {
	switch email {
	case "ada@example.com":
		return 5, "member", nil
	case "view@example.com":
		return 6, "viewer", nil
	}
	return 0, "", store.ErrNotFound
}

func (f *chatFake) TransitionIncident(ctx context.Context, id, action string) (*model.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, id+":"+action)
	f.ackActor = store.ActorOf(ctx)
	f.ackTenant, _ = tenant.From(ctx)
	return &model.Incident{ID: id}, nil
}

func (f *chatFake) PatchMonitor(_ context.Context, id string, p model.MonitorPatch) (*model.Monitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.muted == nil {
		f.muted = map[string]int64{}
	}
	f.muted[id] = *p.MutedUntil
	return &model.Monitor{ID: id}, nil
}

func (f *chatFake) SaveGitHubInstall(_ context.Context, inst int64, account, typ string) (int64, error) {
	f.ghSaved = append(f.ghSaved, inst)
	return 21, nil
}

func (f *chatFake) GitHubInstallation(_ context.Context, id string) (int64, error) {
	if id != "21" {
		return 0, store.ErrNotFound
	}
	return 555, nil
}

func (f *chatFake) RepoNames(context.Context, string) (map[string]bool, error) {
	return map[string]bool{"acme/shop": true}, nil
}

// fakeSlack serves oauth.v2.access, conversations.list (2 pages), users.info and a response_url sink.
func fakeSlack(t *testing.T, responses *[]string) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth.v2.access":
			if r.PostForm.Get("code") != "c0de" || r.PostForm.Get("client_secret") != "cs" {
				_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_code"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"access_token":"xoxb-new","scope":"chat:write","bot_user_id":"UBOT","team":{"id":"T1","name":"Acme"}}`))
		case "/conversations.list":
			if r.Header.Get("Authorization") != "Bearer xoxb-1" {
				_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
				return
			}
			if r.PostForm.Get("cursor") == "" {
				_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C2","name":"payments-alerts","is_member":true}],"response_metadata":{"next_cursor":"p2"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"G1","name":"oncall","is_private":true}],"response_metadata":{"next_cursor":""}}`))
		case "/users.info":
			email := map[string]string{"U1": "ada@example.com", "U2": "view@example.com", "U3": "stranger@example.com"}[r.PostForm.Get("user")]
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"profile": map[string]any{"email": email}}})
		case "/respond":
			var m struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			*responses = append(*responses, m.Text)
			mu.Unlock()
		default:
			w.WriteHeader(404)
		}
	}))
}

func chatServer(t *testing.T, api *httptest.Server, gh *githubapp.Client) (*Server, *chatFake) {
	f := &chatFake{identityFake: newIdentityFake()}
	s := identityServer(t, f.identityFake, nil)
	s.Store = f
	s.PublicAPIURL = "https://api.example.com"
	s.Slack = &SlackApp{ClientID: "cid", ClientSecret: "cs", SigningSecret: "sign", API: &slack.Client{BaseURL: api.URL}}
	if gh != nil {
		s.GitHub = &GitHubApp{Client: gh, Slug: "tasktrooper", WebURL: "https://github.example.com"}
	}
	return s, f
}

func TestSlackInstallFlow(t *testing.T) {
	var responses []string
	api := fakeSlack(t, &responses)
	defer api.Close()
	s, f := chatServer(t, api, nil)
	h := s.Handler()

	// key mode: POST install-url → signed URL; viewers cannot install
	w := do(h, "POST", "/api/v1/integrations/slack/install-url?redirect="+url.QueryEscape("http://localhost:5173/settings/integrations"), "good", "")
	var res struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	u, _ := url.Parse(res.URL)
	if w.Code != 200 || u.Host != "slack.com" || u.Query().Get("client_id") != "cid" || !strings.Contains(u.Query().Get("scope"), "users:read.email") ||
		u.Query().Get("redirect_uri") != "https://api.example.com/api/v1/integrations/slack/callback" {
		t.Fatalf("install-url: %d %s", w.Code, w.Body)
	}
	if w := cookieReq(h, "GET", "/api/v1/integrations/slack/install", "sess-5", "", projectHeader, "80"); w.Code != 403 {
		t.Errorf("viewer install: %d", w.Code)
	}
	// cookie mode: GET install redirects
	if w := cookieReq(h, "GET", "/api/v1/integrations/slack/install", "sess-5", ""); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "https://slack.com/oauth/v2/authorize?") {
		t.Errorf("install redirect: %d %s", w.Code, w.Header().Get("Location"))
	}

	state := u.Query().Get("state")
	// tampered state
	w = do(h, "GET", "/api/v1/integrations/slack/callback?code=c0de&state="+url.QueryEscape(state+"x"), "", "")
	if !strings.Contains(w.Header().Get("Location"), "slack=error") || len(f.slackSaved) != 0 {
		t.Errorf("tampered state: %s", w.Header().Get("Location"))
	}
	// github state is not a slack state
	ghState := s.signState(installState{Kind: "github", Org: 7, Project: 42})
	if w := do(h, "GET", "/api/v1/integrations/slack/callback?code=c0de&state="+url.QueryEscape(ghState), "", ""); !strings.Contains(w.Header().Get("Location"), "slack=error") {
		t.Errorf("cross-kind state accepted")
	}
	w = do(h, "GET", "/api/v1/integrations/slack/callback?code=c0de&state="+url.QueryEscape(state), "", "")
	if w.Code != 302 || w.Header().Get("Location") != "http://localhost:5173/settings/integrations?slack=connected" {
		t.Fatalf("callback: %d %s %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if len(f.slackSaved) != 1 || f.slackSaved[0] != "7|T1|Acme|UBOT|xoxb-new" {
		t.Errorf("saved: %v", f.slackSaved)
	}
	// expired state
	s.Now = func() time.Time { return testNow.Add(time.Hour) }
	if w := do(h, "GET", "/api/v1/integrations/slack/callback?code=c0de&state="+url.QueryEscape(state), "", ""); !strings.Contains(w.Header().Get("Location"), "slack=error") {
		t.Errorf("expired state accepted")
	}
	s.Now = func() time.Time { return testNow }

	// channels: all pages, filtered, cached
	w = do(h, "GET", "/api/v1/integrations/slack/channels?q=%23pay", "good", "")
	var chans []model.SlackChannel
	_ = json.Unmarshal(w.Body.Bytes(), &chans)
	if w.Code != 200 || len(chans) != 1 || chans[0].ID != "C2" || !chans[0].IsMember {
		t.Errorf("channels: %d %s", w.Code, w.Body)
	}
	api.Config.Handler = http.NotFoundHandler() // cached for 5 minutes: no Slack call
	w = do(h, "GET", "/api/v1/integrations/slack/channels", "good", "")
	_ = json.Unmarshal(w.Body.Bytes(), &chans)
	if w.Code != 200 || len(chans) != 2 || chans[0].Name != "oncall" {
		t.Errorf("cached channels: %d %s", w.Code, w.Body)
	}
	if w := do(h, "DELETE", "/api/v1/integrations/slack", "good", ""); w.Code != 204 || f.slackDelete != 1 {
		t.Errorf("delete: %d", w.Code)
	}
}

func slackPost(h http.Handler, secret string, ts time.Time, payload map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(payload)
	body := "payload=" + url.QueryEscape(string(b))
	tss := strconv.FormatInt(ts.Unix(), 10)
	return do(h, "POST", "/api/v1/slack/interactions", "", body, "Content-Type", "application/x-www-form-urlencoded",
		"X-Slack-Request-Timestamp", tss, "X-Slack-Signature", slack.Sign(secret, tss, []byte(body)))
}

func TestSlackInteractions(t *testing.T) {
	var responses []string
	api := fakeSlack(t, &responses)
	defer api.Close()
	s, f := chatServer(t, api, nil)
	h := s.Handler()
	action := func(user, actionID, team string) map[string]any {
		return map[string]any{"type": "block_actions", "team": map[string]any{"id": team}, "user": map[string]any{"id": user},
			"response_url": api.URL + "/respond", "actions": []map[string]any{{"action_id": actionID, "value": "x"}}}
	}
	if w := slackPost(h, "wrong", testNow, action("U1", "tt_ack:12", "T1")); w.Code != 401 || len(f.acked) != 0 {
		t.Errorf("bad signature: %d", w.Code)
	}
	if w := slackPost(h, "sign", testNow.Add(-10*time.Minute), action("U1", "tt_ack:12", "T1")); w.Code != 401 {
		t.Errorf("stale timestamp: %d", w.Code)
	}
	if w := slackPost(h, "sign", testNow, action("U1", "tt_ack:12", "T1")); w.Code != 200 {
		t.Fatalf("ack: %d %s", w.Code, w.Body)
	}
	if len(f.acked) != 1 || f.acked[0] != "12:ack" || f.ackActor.UserID != 5 || f.ackActor.Via != "slack" || f.ackTenant.ProjectID != 42 {
		t.Errorf("ack: %v %+v %+v", f.acked, f.ackActor, f.ackTenant)
	}
	slackPost(h, "sign", testNow, action("U1", "tt_mute1h:3", "T1"))
	if f.muted["3"] != testNow.Add(time.Hour).UnixMilli() {
		t.Errorf("mute: %v", f.muted)
	}
	slackPost(h, "sign", testNow, action("U2", "tt_ack:12", "T1"))     // viewer
	slackPost(h, "sign", testNow, action("U3", "tt_ack:12", "T1"))     // not a member
	slackPost(h, "sign", testNow, action("U1", "tt_ack:12", "TOTHER")) // other workspace
	slackPost(h, "sign", testNow, action("U1", "tt_ack:900", "T1"))    // incident of an org without this install
	slackPost(h, "sign", testNow, action("U1", "tt_open", "T1"))       // link button: no reply
	if len(f.acked) != 1 {
		t.Errorf("unauthorized acks went through: %v", f.acked)
	}
	want := []string{"Acknowledged by ada@example.com.", "Muted for 1 hour", "Viewers cannot", "No TaskTrooper member", "not connected", "not connected"}
	if len(responses) != len(want) {
		t.Fatalf("responses: %q", responses)
	}
	for i, w := range want {
		if !strings.Contains(responses[i], w) {
			t.Errorf("response %d = %q, want %q", i, responses[i], w)
		}
	}
	var audit []string
	for _, a := range f.audit {
		audit = append(audit, a.Action)
	}
	if !strings.Contains(strings.Join(audit, ","), "slack.ack") {
		t.Errorf("audit: %v", audit)
	}
}

// fakeGitHub serves the App API: installation lookup, access token, installation repositories (2 pages).
func fakeGitHub(t *testing.T) (*httptest.Server, *githubapp.Client) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/app/installations/555" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"id":555,"account":{"login":"acme","type":"Organization"}}`))
		case r.URL.Path == "/app/installations/555/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/installation/repositories" && r.Header.Get("Authorization") == "token ghs_x":
			if r.URL.Query().Get("page") == "1" {
				repos := make([]map[string]any, 100)
				for i := range repos {
					repos[i] = map[string]any{"full_name": "acme/svc-" + strconv.Itoa(i), "default_branch": "main"}
				}
				repos[0] = map[string]any{"full_name": "acme/shop", "default_branch": "trunk", "private": true, "language": "Go"}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "repositories": repos})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "repositories": []map[string]any{{"full_name": "acme/web", "default_branch": "main"}}})
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	c := &githubapp.Client{AppID: 1, Key: key, BaseURL: srv.URL, HTTP: srv.Client()}
	return srv, c
}

func TestGitHubInstallAndRepos(t *testing.T) {
	var responses []string
	api := fakeSlack(t, &responses)
	defer api.Close()
	gh, client := fakeGitHub(t)
	defer gh.Close()
	s, f := chatServer(t, api, client)
	h := s.Handler()

	w := do(h, "POST", "/api/v1/integrations/github/install-url", "good", "")
	var res struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	u, _ := url.Parse(res.URL)
	if w.Code != 200 || u.Host != "github.example.com" || u.Path != "/apps/tasktrooper/installations/new" || u.Query().Get("state") == "" {
		t.Fatalf("install-url: %d %s", w.Code, w.Body)
	}
	state := url.QueryEscape(u.Query().Get("state"))
	w = do(h, "GET", "/api/v1/integrations/github/callback?installation_id=555&setup_action=install&state="+state, "", "")
	if w.Code != 302 || w.Header().Get("Location") != "http://localhost:5173/settings/integrations?github=21" || len(f.ghSaved) != 1 || f.ghSaved[0] != 555 {
		t.Fatalf("callback: %d %s %v", w.Code, w.Header().Get("Location"), f.ghSaved)
	}
	w = do(h, "GET", "/api/v1/integrations/github/callback?installation_id=666&setup_action=install&state="+state, "", "")
	if !strings.Contains(w.Header().Get("Location"), "github=error") || len(f.ghSaved) != 1 {
		t.Errorf("foreign installation accepted: %s", w.Header().Get("Location"))
	}
	w = do(h, "GET", "/api/v1/integrations/github/callback?setup_action=request&state="+state, "", "")
	if !strings.Contains(w.Header().Get("Location"), "github=pending") {
		t.Errorf("request: %s", w.Header().Get("Location"))
	}

	w = do(h, "GET", "/api/v1/integrations/github/21/repos", "good", "")
	var repos []model.GitHubRepo
	_ = json.Unmarshal(w.Body.Bytes(), &repos)
	if w.Code != 200 || len(repos) != 101 || repos[0].FullName != "acme/shop" || !repos[0].Added || !repos[0].Private || repos[0].DefaultBranch != "trunk" {
		t.Fatalf("repos: %d %d %+v", w.Code, len(repos), repos[:min(2, len(repos))])
	}
	w = do(h, "GET", "/api/v1/integrations/github/21/repos?q=WEB", "good", "")
	_ = json.Unmarshal(w.Body.Bytes(), &repos)
	if len(repos) != 1 || repos[0].FullName != "acme/web" || repos[0].Added {
		t.Errorf("filtered repos: %+v", repos)
	}
	if w := do(h, "GET", "/api/v1/integrations/github/99/repos", "good", ""); w.Code != 404 {
		t.Errorf("unknown integration: %d", w.Code)
	}
}

func TestChatOpsNotConfigured(t *testing.T) {
	h := identityServer(t, newIdentityFake(), nil).Handler()
	for _, p := range []string{"/api/v1/integrations/slack/install", "/api/v1/integrations/github/install", "/api/v1/integrations/slack/channels"} {
		if w := do(h, "GET", p, "good", ""); w.Code != 501 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}
