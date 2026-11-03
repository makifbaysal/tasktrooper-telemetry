package server

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// passwordFake: demo@example.com (user 5) has a password, oidc@example.com (user 6) has none.
type passwordFake struct {
	*identityFake
	hash string
}

func newPasswordFake(t *testing.T) *passwordFake {
	h, err := bcrypt.GenerateFromPassword([]byte("correct horse battery"), bcrypt.MinCost) // cheap cost: tests only
	if err != nil {
		t.Fatal(err)
	}
	return &passwordFake{identityFake: newIdentityFake(), hash: string(h)}
}

func (f *passwordFake) PasswordHash(_ context.Context, email string) (int64, string, error) {
	switch email {
	case "demo@example.com":
		return 5, f.hash, nil
	case "oidc@example.com":
		return 6, "", nil
	}
	return 0, "", store.ErrNotFound
}

func passwordServer(t *testing.T, f *passwordFake, mut func(*Server)) *Server {
	s := identityServer(t, f.identityFake, func(s *Server) {
		s.PasswordLogin = true
		s.ClientIPHeader = "CF-Connecting-IP"
		if mut != nil {
			mut(s)
		}
	})
	s.Store = f
	return s
}

func TestPasswordLogin(t *testing.T) {
	const ok = `{"email":"Demo@Example.com ","password":"correct horse battery"}`
	tests := []struct {
		name     string
		disabled bool
		body     string
		hdr      []string
		want     int
		wantErr  string
	}{
		{name: "disabled", disabled: true, body: ok, want: 404},
		{name: "success", body: ok, want: 200},
		{name: "wrong password", body: `{"email":"demo@example.com","password":"wrong horse battery"}`, want: 401, wantErr: invalidCredentials},
		{name: "unknown email", body: `{"email":"nobody@example.com","password":"correct horse battery"}`, want: 401, wantErr: invalidCredentials},
		{name: "oidc user without password", body: `{"email":"oidc@example.com","password":"correct horse battery"}`, want: 401, wantErr: invalidCredentials},
		{name: "over-long password", body: `{"email":"demo@example.com","password":"` + strings.Repeat("x", 80) + `"}`, want: 401, wantErr: invalidCredentials},
		{name: "missing password", body: `{"email":"demo@example.com"}`, want: 400},
		{name: "foreign origin", body: ok, hdr: []string{"Origin", "https://evil.example"}, want: 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPasswordFake(t)
			h := passwordServer(t, f, func(s *Server) { s.PasswordLogin = !tt.disabled }).Handler()
			w := do(h, "POST", "/api/v1/auth/password-login", "", tt.body, tt.hdr...)
			if w.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.want, w.Body)
			}
			if tt.wantErr != "" && !strings.Contains(w.Body.String(), tt.wantErr) {
				t.Errorf("body %s, want %q", w.Body, tt.wantErr)
			}
			if strings.Contains(w.Body.String(), "horse") {
				t.Errorf("response echoes the password: %s", w.Body)
			}
			c := w.Result().Cookies()
			if tt.want != 200 {
				if len(c) != 0 {
					t.Errorf("cookie on failure: %v", c)
				}
				return
			}
			if len(c) != 1 || c[0].Name != sessionCookie || c[0].Value != "new-5" || !c[0].HttpOnly {
				t.Fatalf("session cookie: %v", c)
			}
			if len(f.audit) != 1 || f.audit[0].Action != "login" || f.audit[0].Meta["method"] != "password" || f.audit[0].OrgID != 7 {
				t.Errorf("audit: %+v", f.audit)
			}
			if w := cookieReq(h, "GET", "/api/v1/me", "new-5", ""); w.Code != 200 {
				t.Errorf("session unusable: %d", w.Code)
			}
		})
	}
}

func TestPasswordLoginConfig(t *testing.T) {
	h := passwordServer(t, newPasswordFake(t), nil).Handler()
	if w := do(h, "GET", "/api/v1/auth/config", "", ""); strings.TrimSpace(w.Body.String()) != `{"oidc":false,"devLogin":false,"password":true}` {
		t.Errorf("config: %s", w.Body)
	}
}

func TestPasswordLoginRateLimit(t *testing.T) {
	const good, bad = `{"email":"demo@example.com","password":"correct horse battery"}`, `{"email":"demo@example.com","password":"wrong horse battery"}`
	tests := []struct {
		name string
		// failures before the probe: n attempts with body, each from ip(i)
		n    int
		body string
		ip   func(i int) string
		// probe (the correct password) from probeIP
		probeIP string
		advance time.Duration
		want    int
	}{
		{name: "per ip", n: loginMaxPerIP, body: bad, ip: func(int) string { return "203.0.113.1" }, probeIP: "203.0.113.1", want: 429},
		{name: "other ip not affected", n: loginMaxPerIP, body: bad, ip: func(int) string { return "203.0.113.1" }, probeIP: "203.0.113.2", want: 200},
		{name: "unknown emails count per ip", n: loginMaxPerIP, body: `{"email":"x@example.com","password":"guess guess guess"}`,
			ip: func(int) string { return "203.0.113.1" }, probeIP: "203.0.113.1", want: 429},
		{name: "per email across ips", n: loginMaxPerUser, body: bad, ip: func(i int) string { return "198.51.100." + strconv.Itoa(i) }, probeIP: "203.0.113.9", want: 429},
		{name: "below the limit", n: loginMaxPerIP - 1, body: bad, ip: func(int) string { return "203.0.113.1" }, probeIP: "203.0.113.1", want: 200},
		{name: "window expires", n: loginMaxPerIP, body: bad, ip: func(int) string { return "203.0.113.1" }, probeIP: "203.0.113.1", advance: loginWindow, want: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := testNow
			h := passwordServer(t, newPasswordFake(t), func(s *Server) { s.Now = func() time.Time { return now } }).Handler()
			for i := range tt.n {
				if w := do(h, "POST", "/api/v1/auth/password-login", "", tt.body, "CF-Connecting-IP", tt.ip(i)); w.Code != 401 {
					t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body)
				}
			}
			now = now.Add(tt.advance)
			w := do(h, "POST", "/api/v1/auth/password-login", "", good, "CF-Connecting-IP", tt.probeIP)
			if w.Code != tt.want {
				t.Fatalf("probe: %d, want %d: %s", w.Code, tt.want, w.Body)
			}
			if tt.want == 429 {
				if ra, _ := strconv.Atoi(w.Header().Get("Retry-After")); ra <= 0 || ra > int(loginWindow/time.Second) {
					t.Errorf("Retry-After %q", w.Header().Get("Retry-After"))
				}
			}
		})
	}
}

// Me: user 5 is demo@example.com (has a password), user 6 is oidc@example.com.
func (f *passwordFake) Me(ctx context.Context, uid int64) (model.Me, error) {
	me, err := f.identityFake.Me(ctx, uid)
	if me.User != nil {
		me.User.Email = map[int64]string{5: "demo@example.com", 6: "oidc@example.com"}[uid]
	}
	return me, err
}

func TestPasswordAccountCannotCreateOrg(t *testing.T) {
	const body = `{"name":"Acme","project":{"name":"Shop"}}`
	tests := []struct {
		name    string
		session string
		want    int
	}{
		{"shared password account", "sess-5", 403},
		{"oidc user", "sess-6", 201},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := passwordServer(t, newPasswordFake(t), nil).Handler()
			if w := cookieReq(h, "POST", "/api/v1/orgs", tt.session, body); w.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.want, w.Body)
			}
		})
	}
}
