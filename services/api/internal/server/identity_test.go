package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// identityFake: user 5 is admin of org 7 (projects 42, 43), viewer of org 8 (project 80); user 6 has no orgs.
type identityFake struct {
	fakeStore
	mu       sync.Mutex
	sessions map[string]int64
	states   map[string][3]string
	upserts  []store.UserClaims
	created  []model.MonitorInput
}

func newIdentityFake() *identityFake {
	return &identityFake{sessions: map[string]int64{"sess-5": 5, "sess-6": 6}, states: map[string][3]string{}}
}

func (f *identityFake) SessionUser(_ context.Context, token string, _ time.Duration) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uid, ok := f.sessions[token]; ok {
		return uid, token == "sess-5", nil
	}
	return 0, false, store.ErrNotFound
}

func (f *identityFake) CreateSession(_ context.Context, uid int64, _ string, _ time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := "new-" + strconv.FormatInt(uid, 10)
	f.sessions[tok] = uid
	return tok, nil
}

func (f *identityFake) DeleteSession(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, token)
	return nil
}

func (f *identityFake) SaveLoginState(_ context.Context, state, verifier, nonce, redirect string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[state] = [3]string{verifier, nonce, redirect}
	return nil
}

func (f *identityFake) TakeLoginState(_ context.Context, state string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.states[state]
	if !ok {
		return "", "", "", store.ErrNotFound
	}
	delete(f.states, state)
	return v[0], v[1], v[2], nil
}

func (f *identityFake) UpsertUser(_ context.Context, c store.UserClaims) (int64, error) {
	f.upserts = append(f.upserts, c)
	return 5, nil
}

func (f *identityFake) ProjectAccess(_ context.Context, uid, pid int64) (int64, string, error) {
	if uid == 5 {
		switch pid {
		case 42, 43:
			return 7, "admin", nil
		case 80:
			return 8, "viewer", nil
		}
	}
	return 0, "", store.ErrNotFound
}

func (f *identityFake) DefaultProject(_ context.Context, uid int64) (int64, int64, string, error) {
	if uid == 5 {
		return 42, 7, "admin", nil
	}
	return 0, 0, "", store.ErrNotFound
}

func (f *identityFake) OrgRole(_ context.Context, uid, org int64) (string, error) {
	if uid == 5 && org == 7 {
		return "admin", nil
	}
	if uid == 5 && org == 8 {
		return "viewer", nil
	}
	return "", store.ErrNotFound
}

func (f *identityFake) MemberRole(_ context.Context, org, uid int64) (string, error) {
	if uid == 1 {
		return "owner", nil
	}
	return "member", nil
}

func (f *identityFake) Me(_ context.Context, uid int64) (model.Me, error) {
	return model.Me{User: &model.User{ID: strconv.FormatInt(uid, 10), Email: "a@b.c"}, Orgs: []model.MeOrg{}}, nil
}

func (f *identityFake) ListMembers(context.Context, int64) ([]model.Member, error) {
	return []model.Member{{UserID: "5", Role: "admin"}}, nil
}

func (f *identityFake) UpdateMember(_ context.Context, _ int64, uid int64, role string) (*model.Member, error) {
	return &model.Member{UserID: strconv.FormatInt(uid, 10), Role: role}, nil
}

func (f *identityFake) ListProjects(context.Context, int64) ([]model.Project, error) {
	return []model.Project{}, nil
}

func (f *identityFake) CreateMonitor(_ context.Context, in model.MonitorInput) (*model.Monitor, error) {
	f.created = append(f.created, in)
	return &model.Monitor{ID: "77", Name: in.Name}, nil
}

func (f *identityFake) CreateAPIKey(_ context.Context, in model.APIKeyInput, by int64) (*model.APIKey, error) {
	return &model.APIKey{ID: "3", Name: in.Name, Kind: in.Kind, Key: "tt_api_secret", CreatedBy: strconv.FormatInt(by, 10)}, nil
}

func (f *identityFake) RevokeAPIKey(context.Context, string) ([]byte, error) {
	return make([]byte, 32), nil
}

func identityServer(t *testing.T, f *identityFake, mut func(*Server)) *Server {
	f.t = t
	s := &Server{Store: f, Auth: auth.Static{"good": {OrgID: 7, ProjectID: 42}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Origins: []string{"http://localhost:*"}, Now: func() time.Time { return testNow }, WebURL: "http://localhost:5173", StateKey: []byte("k")}
	if mut != nil {
		mut(s)
	}
	return s
}

func cookieReq(h http.Handler, method, path, session, body string, hdr ...string) *httptest.ResponseRecorder {
	return do(h, method, path, "", body, append([]string{"Cookie", sessionCookie + "=" + session}, hdr...)...)
}

func TestSessionAuthAndRoles(t *testing.T) {
	f := newIdentityFake()
	h := identityServer(t, f, nil).Handler()
	tests := []struct {
		name, method, path, session, body string
		hdr                               []string
		want                              int
	}{
		{name: "no credentials", method: "GET", path: "/api/v1/services", want: 401},
		{name: "unknown session", method: "GET", path: "/api/v1/services", session: "nope", want: 401},
		{name: "session default project", method: "GET", path: "/api/v1/services", session: "sess-5", want: 200},
		{name: "foreign project", method: "GET", path: "/api/v1/services", session: "sess-5", hdr: []string{projectHeader, "99"}, want: 403},
		{name: "bad project header", method: "GET", path: "/api/v1/services", session: "sess-5", hdr: []string{projectHeader, "x"}, want: 403},
		{name: "viewer reads", method: "GET", path: "/api/v1/services", session: "sess-5", hdr: []string{projectHeader, "80"}, want: 200},
		{name: "viewer cannot write", method: "POST", path: "/api/v1/monitors", session: "sess-5", hdr: []string{projectHeader, "80"},
			body: `{"name":"x","kind":"latency","severity":"info"}`, want: 403},
		{name: "member writes", method: "POST", path: "/api/v1/monitors", session: "sess-5", hdr: []string{projectHeader, "43"},
			body: `{"name":"x","kind":"latency","severity":"info"}`, want: 201},
		{name: "foreign origin mutation", method: "POST", path: "/api/v1/monitors", session: "sess-5", hdr: []string{"Origin", "https://evil.example"},
			body: `{"name":"x","kind":"latency","severity":"info"}`, want: 403},
		{name: "no project user: services", method: "GET", path: "/api/v1/services", session: "sess-6", want: 403},
		{name: "no project user: me", method: "GET", path: "/api/v1/me", session: "sess-6", want: 200},
		{name: "no project user: projects", method: "GET", path: "/api/v1/projects", session: "sess-6", want: 200},
		{name: "members of own org", method: "GET", path: "/api/v1/orgs/7/members", session: "sess-5", want: 200},
		{name: "members of foreign org", method: "GET", path: "/api/v1/orgs/9/members", session: "sess-5", want: 404},
		{name: "viewer org cannot change roles", method: "PATCH", path: "/api/v1/orgs/8/members/2", session: "sess-5", body: `{"role":"admin"}`, want: 403},
		{name: "admin changes member role", method: "PATCH", path: "/api/v1/orgs/7/members/2", session: "sess-5", body: `{"role":"viewer"}`, want: 200},
		{name: "admin cannot grant owner", method: "PATCH", path: "/api/v1/orgs/7/members/2", session: "sess-5", body: `{"role":"owner"}`, want: 403},
		{name: "admin cannot demote owner", method: "PATCH", path: "/api/v1/orgs/7/members/1", session: "sess-5", body: `{"role":"admin"}`, want: 403},
		{name: "viewer cannot list api keys", method: "GET", path: "/api/v1/api-keys", session: "sess-5", hdr: []string{projectHeader, "80"}, want: 403},
		{name: "api key creates api key", method: "POST", path: "/api/v1/api-keys", body: `{"name":"ci","kind":"api"}`, hdr: []string{"Authorization", "Bearer good"}, want: 201},
		{name: "api key revokes", method: "DELETE", path: "/api/v1/api-keys/3", hdr: []string{"Authorization", "Bearer good"}, want: 204},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w *httptest.ResponseRecorder
			if tt.session != "" {
				w = cookieReq(h, tt.method, tt.path, tt.session, tt.body, tt.hdr...)
			} else {
				w = do(h, tt.method, tt.path, "", tt.body, tt.hdr...)
			}
			if w.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.want, w.Body)
			}
		})
	}
	// sliding expiry re-sends the cookie
	w := cookieReq(h, "GET", "/api/v1/services", "sess-5", "")
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != sessionCookie || !c[0].HttpOnly || c[0].SameSite != http.SameSiteLaxMode || c[0].MaxAge != 7*86400 {
		t.Errorf("refreshed cookie: %+v", c)
	}
	// audit: action is the route pattern, target the path id / created id
	var actions []string
	for _, a := range f.audit {
		actions = append(actions, a.Action+" "+a.Target)
	}
	got := strings.Join(actions, "|")
	for _, want := range []string{"POST /monitors 77", "PATCH /orgs/{org}/members/{user} 2", "POST /api-keys 3", "DELETE /api-keys/{id} 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit %q missing %q", got, want)
		}
	}
	for _, a := range f.audit {
		if a.Action == "POST /monitors" && (a.OrgID != 7 || a.ProjectID != 43 || a.UserID != 5) {
			t.Errorf("audit tenant: %+v", a)
		}
		if a.Action == "POST /api-keys" && (a.UserID != 0 || a.OrgID != 7) {
			t.Errorf("api key audit: %+v", a)
		}
	}
}

func TestMeAndCORSCredentials(t *testing.T) {
	h := identityServer(t, newIdentityFake(), nil).Handler()
	w := cookieReq(h, "GET", "/api/v1/me", "sess-5", "", projectHeader, "43")
	var me model.Me
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if w.Code != 200 || me.User == nil || me.CurrentProjectID != "43" || me.Role != "admin" || me.Auth != "session" {
		t.Errorf("me: %d %s", w.Code, w.Body)
	}
	w = do(h, "OPTIONS", "/api/v1/me", "", "", "Origin", "http://localhost:5173", "Access-Control-Request-Method", "GET")
	hd := w.Header()
	if hd.Get("Access-Control-Allow-Origin") != "http://localhost:5173" || hd.Get("Access-Control-Allow-Credentials") != "true" ||
		!strings.Contains(hd.Get("Access-Control-Allow-Headers"), "X-Trooper-Project") {
		t.Errorf("cors: %v", hd)
	}
}

func TestDevLoginAndLogout(t *testing.T) {
	f := newIdentityFake()
	h := identityServer(t, f, nil).Handler()
	if w := do(h, "POST", "/api/v1/auth/dev-login", "", `{"email":"x@y.z"}`); w.Code != 404 {
		t.Errorf("dev login must be off by default: %d", w.Code)
	}
	h = identityServer(t, f, func(s *Server) { s.DevLogin = true }).Handler()
	w := do(h, "GET", "/api/v1/auth/config", "", "")
	if strings.TrimSpace(w.Body.String()) != `{"oidc":false,"devLogin":true}` {
		t.Errorf("config: %s", w.Body)
	}
	w = do(h, "POST", "/api/v1/auth/dev-login", "", `{"email":"dev@tasktrooper.local"}`)
	c := w.Result().Cookies()
	if w.Code != 200 || len(c) != 1 || c[0].Value != "new-5" || c[0].Secure {
		t.Fatalf("dev login: %d %v %s", w.Code, c, w.Body)
	}
	w = do(h, "POST", "/api/v1/auth/dev-login", "", `{"email":"x"}`, "X-Forwarded-Proto", "https")
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Errorf("https cookie must be Secure: %v", c)
	}
	w = cookieReq(h, "POST", "/api/v1/auth/logout", "new-5", "")
	if w.Code != 204 || w.Result().Cookies()[0].MaxAge >= 0 {
		t.Errorf("logout: %d", w.Code)
	}
	if w := cookieReq(h, "GET", "/api/v1/me", "new-5", ""); w.Code != 401 {
		t.Errorf("session survived logout: %d", w.Code)
	}
}

func TestSafeRedirect(t *testing.T) {
	s := identityServer(t, newIdentityFake(), nil)
	for in, want := range map[string]string{
		"":                               "http://localhost:5173/",
		"/settings":                      "http://localhost:5173/settings",
		"//evil.com/x":                   "http://localhost:5173/",
		"http://localhost:3000/x":        "http://localhost:3000/x",
		"https://evil.com/x":             "http://localhost:5173/",
		"javascript:alert(1)":            "http://localhost:5173/",
		"http://localhost:5173/a?b=c":    "http://localhost:5173/a?b=c",
		"http://localhost.evil.com:80/x": "http://localhost:5173/",
	} {
		if got := s.safeRedirect(in); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeIssuer is an OIDC provider: discovery, JWKS, and a token endpoint that checks the PKCE verifier.
type fakeIssuer struct {
	*httptest.Server
	key       *rsa.PrivateKey
	challenge string
	nonce     string
	email     string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key, email: "Ada@Example.com"}
	mux := http.NewServeMux()
	fi.Server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": fi.URL, "authorization_endpoint": fi.URL + "/authorize",
			"token_endpoint": fi.URL + "/token", "jwks_uri": fi.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != fi.challenge {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims, _ := json.Marshal(map[string]any{"iss": fi.URL, "sub": "sub-1", "aud": "client-1", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(), "nonce": fi.nonce, "email": fi.email, "email_verified": true, "name": "Ada"})
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
		jws, _ := signer.Sign(claims)
		idt, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idt})
	})
	return fi
}

func TestOIDCLoginFlow(t *testing.T) {
	fi := newFakeIssuer(t)
	defer fi.Close()
	f := newIdentityFake()
	h := identityServer(t, f, func(s *Server) {
		s.OIDC = &auth.OIDC{Issuer: fi.URL, ClientID: "client-1", ClientSecret: "s", RedirectURL: "http://localhost:8080/api/v1/auth/callback"}
	}).Handler()

	// 1. login → provider authorize URL with PKCE S256 + nonce + state; state cookie set
	w := do(h, "GET", "/api/v1/auth/login?redirect=/services", "", "")
	if w.Code != 302 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), fi.URL+"/authorize") || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "client-1" ||
		q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("authorize url: %s", loc)
	}
	fi.challenge, fi.nonce = q.Get("code_challenge"), q.Get("nonce")
	var stateCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == oidcStateCookie {
			stateCookie = c
		}
	}
	if stateCookie == nil || stateCookie.Value != q.Get("state") || !stateCookie.HttpOnly {
		t.Fatalf("state cookie: %v", w.Result().Cookies())
	}

	// a callback without the browser's state cookie is rejected (login CSRF)
	w = do(h, "GET", "/api/v1/auth/callback?code=good-code&state="+url.QueryEscape(q.Get("state")), "", "")
	if w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatalf("callback without cookie: %d %s", w.Code, w.Header().Get("Location"))
	}

	// 2. callback (state still pending since the rejected attempt did not consume it? it did not reach TakeLoginState)
	w = do(h, "GET", "/api/v1/auth/callback?code=good-code&state="+url.QueryEscape(q.Get("state")), "", "",
		"Cookie", oidcStateCookie+"="+stateCookie.Value)
	if w.Code != 302 || w.Header().Get("Location") != "http://localhost:5173/services" {
		t.Fatalf("callback: %d %s %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	var sess string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			sess = c.Value
		}
	}
	if sess != "new-5" || len(f.upserts) != 1 || f.upserts[0].Subject != "sub-1" || f.upserts[0].Email != "Ada@Example.com" || !f.upserts[0].EmailVerified {
		t.Fatalf("session %q upserts %+v", sess, f.upserts)
	}
	if w := cookieReq(h, "GET", "/api/v1/me", sess, ""); w.Code != 200 {
		t.Errorf("me after login: %d", w.Code)
	}
	if len(f.audit) == 0 || f.audit[len(f.audit)-1].Action != "login" {
		t.Errorf("login audit: %+v", f.audit)
	}

	// 3. replaying the state fails (single use)
	w = do(h, "GET", "/api/v1/auth/callback?code=good-code&state="+url.QueryEscape(q.Get("state")), "", "",
		"Cookie", oidcStateCookie+"="+stateCookie.Value)
	if !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Errorf("state replay accepted: %s", w.Header().Get("Location"))
	}

	// 4. a wrong nonce (token minted for another login) is rejected
	w = do(h, "GET", "/api/v1/auth/login", "", "")
	loc, _ = url.Parse(w.Header().Get("Location"))
	fi.challenge, fi.nonce = loc.Query().Get("code_challenge"), "other-nonce"
	st := loc.Query().Get("state")
	w = do(h, "GET", "/api/v1/auth/callback?code=good-code&state="+url.QueryEscape(st), "", "", "Cookie", oidcStateCookie+"="+st)
	if !strings.Contains(w.Header().Get("Location"), "error=") || len(f.upserts) != 1 {
		t.Errorf("nonce mismatch accepted: %s", w.Header().Get("Location"))
	}
}

func TestOIDCExchangeRejectsBadPKCE(t *testing.T) {
	fi := newFakeIssuer(t)
	defer fi.Close()
	o := &auth.OIDC{Issuer: fi.URL, ClientID: "client-1", RedirectURL: "http://x/cb"}
	if _, err := o.AuthURL(context.Background(), "s", "n", "verifier-one-verifier-one-verifier-one-12345"); err != nil {
		t.Fatal(err)
	}
	fi.challenge, fi.nonce = "not-the-challenge", "n"
	if _, err := o.Exchange(context.Background(), "good-code", "verifier-one-verifier-one-verifier-one-12345", "n"); err == nil {
		t.Error("exchange with a wrong verifier succeeded")
	}
}

var _ = tenant.Tenant{}
