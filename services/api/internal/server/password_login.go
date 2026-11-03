package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

// Failed password logins allowed per window (ADR-035). Per IP stops guessing across accounts, per email stops a
// distributed guess at one account; the email limit is looser so one client cannot lock a shared account for everyone.
const (
	loginWindow     = 15 * time.Minute
	loginMaxPerIP   = 10
	loginMaxPerUser = 30
)

type loginLimits struct {
	once  sync.Once
	ip    *auth.Limiter
	email *auth.Limiter
}

func (s *Server) limits() *loginLimits {
	l := &s.loginLimits
	l.once.Do(func() {
		l.ip, l.email = auth.NewLimiter(loginMaxPerIP, loginWindow), auth.NewLimiter(loginMaxPerUser, loginWindow)
		l.ip.Now, l.email.Now = s.Now, s.Now
	})
	return l
}

const invalidCredentials = "invalid email or password"

// passwordLogin is POST /auth/password-login (TT_AUTH_PASSWORD=true): email + password → the same tt_session cookie
// as the OIDC callback. Unknown email, a user without a password and a wrong password are indistinguishable (same
// 401, same bcrypt work). The password is never logged.
func (s *Server) passwordLogin(w http.ResponseWriter, r *http.Request) {
	if !s.PasswordLogin {
		writeJSON(w, http.StatusNotFound, apiError{Error: "password login is disabled (TT_AUTH_PASSWORD)"})
		return
	}
	// login CSRF: a foreign page must not sign the browser into an account of its choosing
	if o := r.Header.Get("Origin"); o != "" && !OriginAllowed(s.Origins, o) {
		forbidden(w, "origin not allowed")
		return
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || in.Password == "" {
		s.fail(w, r, store.Invalid("email and password are required"))
		return
	}
	l := s.limits()
	ip := "ip:" + auth.ClientIP(r, s.ClientIPHeader)
	blockedIP, retryIP := l.ip.Blocked(ip)
	blockedEmail, retryEmail := l.email.Blocked(email)
	if blockedIP || blockedEmail {
		retry := max(retryIP, retryEmail)
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, apiError{Error: "too many failed sign-in attempts, try again later"})
		return
	}
	uid, hash, err := s.Store.PasswordHash(r.Context(), email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if len(in.Password) > auth.MaxPasswordLen { // bcrypt would refuse it; keep the same work and answer
		hash = ""
	}
	if auth.CheckPassword(hash, in.Password) != nil {
		l.ip.Fail(ip)
		l.email.Fail(email)
		s.Log.WarnContext(r.Context(), "password login rejected", "email", email, "known", uid > 0)
		writeJSON(w, http.StatusUnauthorized, apiError{Error: invalidCredentials})
		return
	}
	// The per-IP count is kept: knowing one (shared) account's password must not reset guessing at others.
	l.email.Reset(email)
	if err := s.startSession(w, r, uid, "password"); err != nil {
		s.fail(w, r, err)
		return
	}
	me, err := s.Store.Me(r.Context(), uid)
	if err == nil {
		if pid, _, role, perr := s.Store.DefaultProject(r.Context(), uid); perr == nil {
			me.CurrentProjectID, me.Role = idPtr(pid), role
		}
		me.Auth = "session"
	}
	s.reply(w, r, me, err)
}

// passwordAccount reports whether the user signs in with a password (a shared demo account, ADR-035).
func (s *Server) passwordAccount(ctx context.Context, userID int64) (bool, error) {
	me, err := s.Store.Me(ctx, userID)
	if err != nil || me.User == nil {
		return false, err
	}
	_, hash, err := s.Store.PasswordHash(ctx, me.User.Email)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return hash != "", err
}
