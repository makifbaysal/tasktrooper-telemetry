package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
	"golang.org/x/oauth2"
)

// IdentityStore is the identity part of Store (ADR-016, contracts §15).
type IdentityStore interface {
	CreateSession(ctx context.Context, userID int64, userAgent string, ttl time.Duration) (string, error)
	SessionUser(ctx context.Context, token string, ttl time.Duration) (userID int64, refreshed bool, err error)
	DeleteSession(ctx context.Context, token string) error
	SaveLoginState(ctx context.Context, state, verifier, nonce, redirect string) error
	TakeLoginState(ctx context.Context, state string) (verifier, nonce, redirect string, err error)
	UpsertUser(ctx context.Context, c store.UserClaims) (int64, error)
	ProjectAccess(ctx context.Context, userID, projectID int64) (orgID int64, role string, err error)
	DefaultProject(ctx context.Context, userID int64) (projectID, orgID int64, role string, err error)
	OrgRole(ctx context.Context, userID, orgID int64) (string, error)
	Me(ctx context.Context, userID int64) (model.Me, error)
	ProjectMe(ctx context.Context, projectID int64, role string) (model.Me, error)
	ListMembers(ctx context.Context, orgID int64) ([]model.Member, error)
	MemberRole(ctx context.Context, orgID, userID int64) (string, error)
	AddMember(ctx context.Context, orgID int64, in model.MemberInput) (*model.Member, error)
	UpdateMember(ctx context.Context, orgID, userID int64, role string) (*model.Member, error)
	RemoveMember(ctx context.Context, orgID, userID int64) error
	ListProjects(ctx context.Context, userID int64) ([]model.Project, error)
	GetProject(ctx context.Context, projectID int64, role string) ([]model.Project, error)
	CreateProject(ctx context.Context, orgID int64, in model.ProjectInput) (*model.Project, error)
	ListAPIKeys(ctx context.Context) ([]model.APIKey, error)
	CreateAPIKey(ctx context.Context, in model.APIKeyInput, createdBy int64) (*model.APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) ([]byte, error)
	WriteAudit(ctx context.Context, a store.AuditRecord) error
	ListAudit(ctx context.Context, orgID int64, limit int) ([]model.AuditEntry, error)
}

const (
	sessionCookie    = "tt_session"
	oidcStateCookie  = "tt_oidc_state"
	projectHeader    = "X-Trooper-Project"
	defaultSessionTT = 7 * 24 * time.Hour
)

// principal is who is calling: a user (session cookie) or an api key. Role is the caller's role in the org of the
// current project (api keys act as admin of their project's org; they never get owner rights).
type principal struct {
	UserID    int64
	APIKeyID  int64
	OrgID     int64
	ProjectID int64
	Role      string
	Session   bool
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p *principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalOf(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey{}).(*principal)
	if p == nil {
		return &principal{}
	}
	return p
}

var errForbidden = errors.New("forbidden")

func forbidden(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusForbidden, apiError{Error: msg})
}

func (s *Server) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return defaultSessionTT
}

func (s *Server) secure(r *http.Request) bool {
	return s.CookieSecure || r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure(r),
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

// noTenantPaths work for a signed-in user who has no project yet.
func noTenantPath(path string) bool {
	p := strings.TrimPrefix(path, "/api/v1")
	return p == "/me" || p == "/projects" || strings.HasPrefix(p, "/orgs/")
}

// authenticate resolves the caller: `Authorization: Bearer <api key>` first, then the tt_session cookie (the project
// comes from X-Trooper-Project, which must belong to one of the user's orgs, else the user's first project).
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			key = strings.TrimSpace(key)
			var t tenant.Tenant
			var keyID int64
			var err error
			if kr, ok := s.Auth.(auth.KeyResolver); ok {
				t, keyID, err = kr.ResolveKey(ctx, key)
			} else {
				t, err = s.Auth.Resolve(ctx, key)
			}
			if key == "" || errors.Is(err, auth.ErrUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, apiError{Error: "invalid api key"})
				return
			}
			if err != nil {
				s.fail(w, r, err)
				return
			}
			p := &principal{APIKeyID: keyID, OrgID: int64(t.OrgID), ProjectID: int64(t.ProjectID), Role: "admin"}
			next.ServeHTTP(w, r.WithContext(withPrincipal(tenant.With(ctx, t), p)))
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			writeJSON(w, http.StatusUnauthorized, apiError{Error: "missing bearer api key or session"})
			return
		}
		uid, refreshed, err := s.Store.SessionUser(ctx, c.Value, s.sessionTTL())
		if errors.Is(err, store.ErrNotFound) {
			s.setSessionCookie(w, r, "", -1)
			writeJSON(w, http.StatusUnauthorized, apiError{Error: "session expired"})
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if refreshed {
			s.setSessionCookie(w, r, c.Value, int(s.sessionTTL()/time.Second))
		}
		// Cookies ride along on cross-site requests; mutations must come from an allowed origin.
		if o := r.Header.Get("Origin"); o != "" && !isSafe(r.Method) && !OriginAllowed(s.Origins, o) {
			forbidden(w, "origin not allowed")
			return
		}
		p := &principal{UserID: uid, Session: true}
		ctx = store.WithActor(ctx, store.Actor{UserID: uid})
		if h := strings.TrimSpace(r.Header.Get(projectHeader)); h != "" {
			pid, perr := strconv.ParseInt(h, 10, 64)
			var org int64
			var role string
			if perr == nil && pid > 0 {
				org, role, err = s.Store.ProjectAccess(ctx, uid, pid)
			}
			if perr != nil || pid <= 0 || errors.Is(err, store.ErrNotFound) {
				forbidden(w, "no access to project "+strconv.Quote(h))
				return
			}
			if err != nil {
				s.fail(w, r, err)
				return
			}
			p.ProjectID, p.OrgID, p.Role = pid, org, role
		} else {
			pid, org, role, err := s.Store.DefaultProject(ctx, uid)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.fail(w, r, err)
				return
			}
			p.ProjectID, p.OrgID, p.Role = pid, org, role
		}
		if p.ProjectID == 0 {
			if !noTenantPath(r.URL.Path) {
				forbidden(w, "no project: ask an org admin for access or create one (POST /projects)")
				return
			}
		} else {
			ctx = tenant.With(ctx, tenant.Tenant{OrgID: uint32(p.OrgID), ProjectID: uint32(p.ProjectID)})
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, p)))
	})
}

func isSafe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// guard enforces "viewer = read only" and writes the audit log for every mutation. /orgs/* routes check the role
// of the org in their path themselves.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafe(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		p := principalOf(r.Context())
		if p.Role == "viewer" && !strings.HasPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1"), "/orgs/") {
			forbidden(w, "viewers have read-only access")
			return
		}
		rec := &bodyCapture{WrapResponseWriter: middleware.NewWrapResponseWriter(w, r.ProtoMajor)}
		rec.Tee(&rec.buf)
		next.ServeHTTP(rec.WrapResponseWriter, r)
		s.audit(r, p, rec)
	})
}

type bodyCapture struct {
	middleware.WrapResponseWriter
	buf limitedBuffer
}

// limitedBuffer keeps the first 8 KiB of a response (enough to read a created entity's id).
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 8192 - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (s *Server) audit(r *http.Request, p *principal, rec *bodyCapture) {
	rc := chi.RouteContext(r.Context())
	pattern := ""
	target := ""
	org := p.OrgID
	if rc != nil {
		pattern = strings.TrimPrefix(rc.RoutePattern(), "/api/v1")
		for i, k := range rc.URLParams.Keys {
			if k == "*" {
				continue
			}
			target = rc.URLParams.Values[i]
			if k == "org" {
				if n, err := strconv.ParseInt(target, 10, 64); err == nil {
					org = n
				}
			}
		}
	}
	status := rec.Status()
	if status == 0 {
		status = http.StatusOK
	}
	var body struct {
		ID     string `json:"id"`
		UserID string `json:"userId"`
		OrgID  string `json:"orgId"`
	}
	if status < 300 {
		_ = json.Unmarshal(rec.buf.Bytes(), &body)
		if target == "" {
			target = body.ID
			if target == "" {
				target = body.UserID
			}
		}
		if org == 0 && body.OrgID != "" {
			org, _ = strconv.ParseInt(body.OrgID, 10, 64)
		}
	}
	if org == 0 || pattern == "" {
		return
	}
	a := store.AuditRecord{OrgID: org, ProjectID: p.ProjectID, UserID: p.UserID, APIKeyID: p.APIKeyID,
		Action: r.Method + " " + pattern, Target: target, Meta: map[string]any{"status": status, "requestId": middleware.GetReqID(r.Context())}}
	s.writeAudit(r.Context(), a)
}

func (s *Server) writeAudit(ctx context.Context, a store.AuditRecord) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.Store.WriteAudit(ctx, a); err != nil {
		s.Log.Error("audit log write failed", "err", err, "action", a.Action)
	}
}

// adminWrites requires admin (or owner) for mutations of the routes it wraps (members, api keys, integrations, domains).
func (s *Server) adminWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafe(r.Method) && !store.RoleAtLeast(principalOf(r.Context()).Role, "admin") {
			forbidden(w, "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminOnly requires admin for every method.
func (s *Server) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !store.RoleAtLeast(principalOf(r.Context()).Role, "admin") {
			forbidden(w, "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- anonymous /auth routes

func (s *Server) authRoutes(r chi.Router) {
	r.Get("/auth/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, model.AuthConfig{OIDC: s.OIDC != nil, DevLogin: s.DevLogin})
	})
	r.Get("/auth/login", s.oidcLogin)
	r.Get("/auth/callback", s.oidcCallback)
	r.Post("/auth/dev-login", func(w http.ResponseWriter, r *http.Request) {
		if !s.DevLogin {
			writeJSON(w, http.StatusNotFound, apiError{Error: "dev login is disabled (TT_AUTH_DEV)"})
			return
		}
		var in struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		uid, err := s.Store.UpsertUser(r.Context(), store.UserClaims{Email: in.Email, Name: in.Name})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.startSession(w, r, uid, "dev-login"); err != nil {
			s.fail(w, r, err)
			return
		}
		me, err := s.Store.Me(r.Context(), uid)
		if err == nil {
			if pid, _, role, perr := s.Store.DefaultProject(r.Context(), uid); perr == nil {
				me.CurrentProjectID, me.Role = strconv.FormatInt(pid, 10), role
			}
			me.Auth = "session"
		}
		s.reply(w, r, me, err)
	})
	r.Post("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if err := s.Store.DeleteSession(r.Context(), c.Value); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		s.setSessionCookie(w, r, "", -1)
		w.WriteHeader(http.StatusNoContent)
	})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid int64, how string) error {
	token, err := s.Store.CreateSession(r.Context(), uid, r.UserAgent(), s.sessionTTL())
	if err != nil {
		return err
	}
	s.setSessionCookie(w, r, token, int(s.sessionTTL()/time.Second))
	if _, org, _, err := s.Store.DefaultProject(r.Context(), uid); err == nil {
		s.writeAudit(r.Context(), store.AuditRecord{OrgID: org, UserID: uid, Action: "login", Target: strconv.FormatInt(uid, 10),
			Meta: map[string]any{"method": how}})
	}
	return nil
}

// safeRedirect returns where to send the browser after login: a path on the web app, or an absolute URL whose
// origin is in the CORS allow list. Anything else → the web app root.
func (s *Server) safeRedirect(raw string) string {
	base := strings.TrimRight(s.WebURL, "/")
	if base == "" {
		base = "http://localhost:5173"
	}
	if raw == "" {
		return base + "/"
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.Contains(raw, `\`) {
		return base + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return base + "/"
	}
	if u.Scheme+"://"+u.Host == base || OriginAllowed(s.Origins, u.Scheme+"://"+u.Host) {
		return raw
	}
	return base + "/"
}

func (s *Server) loginError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, s.safeRedirect("/login?error="+url.QueryEscape(msg)), http.StatusFound)
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: "OIDC is not configured (TT_OIDC_ISSUER)"})
		return
	}
	state, nonce, verifier := store.RandomToken(24), store.RandomToken(24), oauth2.GenerateVerifier()
	target, err := s.OIDC.AuthURL(r.Context(), state, nonce, verifier)
	if err != nil {
		s.Log.Error("oidc login", "err", err)
		writeJSON(w, http.StatusBadGateway, apiError{Error: "identity provider unavailable"})
		return
	}
	if err := s.Store.SaveLoginState(r.Context(), state, verifier, nonce, s.safeRedirect(qs(r, "redirect"))); err != nil {
		s.fail(w, r, err)
		return
	}
	// Binds the login to this browser (login CSRF): the callback's state must match the cookie.
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: state, Path: "/api/v1/auth", HttpOnly: true, Secure: s.secure(r),
		SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: "OIDC is not configured (TT_OIDC_ISSUER)"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/api/v1/auth", HttpOnly: true, Secure: s.secure(r), MaxAge: -1})
	if e := qs(r, "error"); e != "" {
		s.loginError(w, r, e)
		return
	}
	state := qs(r, "state")
	c, err := r.Cookie(oidcStateCookie)
	if state == "" || err != nil || c.Value != state {
		s.loginError(w, r, "login state mismatch, try again")
		return
	}
	verifier, nonce, redirect, err := s.Store.TakeLoginState(r.Context(), state)
	if errors.Is(err, store.ErrNotFound) {
		s.loginError(w, r, "login expired, try again")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	claims, err := s.OIDC.Exchange(r.Context(), qs(r, "code"), verifier, nonce)
	if err != nil {
		s.Log.Warn("oidc callback rejected", "err", err)
		s.loginError(w, r, "sign-in failed")
		return
	}
	uid, err := s.Store.UpsertUser(r.Context(), store.UserClaims{Subject: claims.Subject, Email: claims.Email,
		EmailVerified: claims.EmailVerified, Name: claims.Name})
	var conflict store.ConflictMsg
	if errors.As(err, &conflict) {
		s.loginError(w, r, conflict.Msg)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.startSession(w, r, uid, "oidc"); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// ---------------------------------------------------------------- authenticated identity routes

func (s *Server) identityRoutes(r chi.Router) {
	r.Get("/me", func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		var me model.Me
		var err error
		if p.Session {
			me, err = s.Store.Me(r.Context(), p.UserID)
			me.Auth = "session"
		} else {
			me, err = s.Store.ProjectMe(r.Context(), p.ProjectID, p.Role)
			me.Auth = "apiKey"
		}
		if p.ProjectID > 0 {
			me.CurrentProjectID = strconv.FormatInt(p.ProjectID, 10)
		}
		me.Role = p.Role
		s.reply(w, r, me, err)
	})

	// ---- org members (role checked against the org in the path)
	r.Route("/orgs/{org}/members", func(r chi.Router) {
		r.Get("/", s.withOrg("viewer", func(w http.ResponseWriter, r *http.Request, org int64, _ string) {
			v, err := s.Store.ListMembers(r.Context(), org)
			s.reply(w, r, v, err)
		}))
		r.Post("/", s.withOrg("admin", func(w http.ResponseWriter, r *http.Request, org int64, role string) {
			var in model.MemberInput
			if err := decode(r, &in); err != nil {
				s.fail(w, r, err)
				return
			}
			if in.Role == "owner" && role != "owner" {
				forbidden(w, "only an owner can add owners")
				return
			}
			v, err := s.Store.AddMember(r.Context(), org, in)
			s.created(w, r, v, err)
		}))
		r.Patch("/{user}", s.withOrg("admin", func(w http.ResponseWriter, r *http.Request, org int64, role string) {
			var in model.MemberPatch
			if err := decode(r, &in); err != nil {
				s.fail(w, r, err)
				return
			}
			uid, err := s.memberParam(r, org, role)
			if err != nil {
				s.failAccess(w, r, err)
				return
			}
			if in.Role == "owner" && role != "owner" {
				forbidden(w, "only an owner can grant the owner role")
				return
			}
			v, err := s.Store.UpdateMember(r.Context(), org, uid, in.Role)
			s.reply(w, r, v, err)
		}))
		r.Delete("/{user}", s.withOrg("admin", func(w http.ResponseWriter, r *http.Request, org int64, role string) {
			uid, err := s.memberParam(r, org, role)
			if err != nil {
				s.failAccess(w, r, err)
				return
			}
			s.noContent(w, r, s.Store.RemoveMember(r.Context(), org, uid))
		}))
	})

	// ---- projects
	r.Get("/projects", func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		var v []model.Project
		var err error
		if p.Session {
			v, err = s.Store.ListProjects(r.Context(), p.UserID)
		} else {
			v, err = s.Store.GetProject(r.Context(), p.ProjectID, p.Role)
		}
		s.reply(w, r, v, err)
	})
	r.Post("/projects", func(w http.ResponseWriter, r *http.Request) {
		var in model.ProjectInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		p := principalOf(r.Context())
		org := p.OrgID
		if in.OrgID != "" {
			n, err := strconv.ParseInt(in.OrgID, 10, 64)
			if err != nil {
				s.fail(w, r, store.Invalid("orgId must be numeric"))
				return
			}
			org = n
		}
		if org == 0 {
			s.fail(w, r, store.Invalid("orgId is required"))
			return
		}
		role, err := s.orgRole(r, org)
		if err != nil {
			s.failAccess(w, r, err)
			return
		}
		if !store.RoleAtLeast(role, "admin") {
			forbidden(w, "admin role required")
			return
		}
		v, err := s.Store.CreateProject(r.Context(), org, in)
		if err == nil && p.Session {
			v.Role = role
		}
		s.created(w, r, v, err)
	})

	// ---- api keys (admin) and audit log (admin)
	r.Group(func(r chi.Router) {
		r.Use(s.adminOnly)
		r.Get("/api-keys", func(w http.ResponseWriter, r *http.Request) {
			v, err := s.Store.ListAPIKeys(r.Context())
			s.reply(w, r, v, err)
		})
		r.Post("/api-keys", func(w http.ResponseWriter, r *http.Request) {
			var in model.APIKeyInput
			if err := decode(r, &in); err != nil {
				s.fail(w, r, err)
				return
			}
			v, err := s.Store.CreateAPIKey(r.Context(), in, principalOf(r.Context()).UserID)
			s.created(w, r, v, err)
		})
		r.Delete("/api-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
			hash, err := s.Store.RevokeAPIKey(r.Context(), param(r, "id"))
			if err == nil {
				if f, ok := s.Auth.(auth.Forgetter); ok {
					f.Forget(hash)
				}
			}
			s.noContent(w, r, err)
		})
		r.Get("/audit-log", func(w http.ResponseWriter, r *http.Request) {
			v, err := s.Store.ListAudit(r.Context(), principalOf(r.Context()).OrgID, qint(r, "limit"))
			s.reply(w, r, v, err)
		})
	})
}

// orgRole is the caller's role in org: api keys are admin of their own org only.
func (s *Server) orgRole(r *http.Request, org int64) (string, error) {
	p := principalOf(r.Context())
	if !p.Session {
		if p.OrgID == org {
			return p.Role, nil
		}
		return "", store.ErrNotFound
	}
	return s.Store.OrgRole(r.Context(), p.UserID, org)
}

func (s *Server) failAccess(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errForbidden) {
		forbidden(w, "owner role required")
		return
	}
	s.fail(w, r, err)
}

type orgHandler func(w http.ResponseWriter, r *http.Request, org int64, role string)

// withOrg parses {org}, resolves the caller's role there (not a member → 404) and requires min.
func (s *Server) withOrg(min string, h orgHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org, err := strconv.ParseInt(param(r, "org"), 10, 64)
		if err != nil {
			s.fail(w, r, store.ErrNotFound)
			return
		}
		role, err := s.orgRole(r, org)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !store.RoleAtLeast(role, min) {
			forbidden(w, min+" role required")
			return
		}
		h(w, r, org, role)
	}
}

// memberParam parses {user}; changing an owner needs the owner role.
func (s *Server) memberParam(r *http.Request, org int64, role string) (int64, error) {
	uid, err := strconv.ParseInt(param(r, "user"), 10, 64)
	if err != nil {
		return 0, store.ErrNotFound
	}
	cur, err := s.Store.MemberRole(r.Context(), org, uid)
	if err != nil {
		return 0, err
	}
	if cur == "owner" && role != "owner" {
		return 0, errForbidden
	}
	return uid, nil
}
