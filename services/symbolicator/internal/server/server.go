// Package server implements the symbolicator HTTP API (contracts.md §12).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/auth"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/jsmap"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/native"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/retrace"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/store"
)

// Limits (bytes).
const (
	DefaultMaxDebugFile = 200 << 20 // dSYM / Dart symbols (zipped or raw)
	DefaultMaxMap       = 50 << 20  // source map / minified JS / mapping.txt
	maxStack            = 1 << 20
	maxRequestOverhead  = 1 << 20
	resultTTL           = 24 * time.Hour
)

// Config wires the server.
type Config struct {
	Store        *store.Store
	Auth         *auth.Authenticator
	Cache        ResultCache // nil → no result caching
	CacheDir     string      // local copies of native debug files
	Log          *slog.Logger
	MaxDebugFile int64
	MaxMap       int64
	Demangle     func([]string) map[string]string // nil → native.DemangleSwift
}

// Server is the HTTP API.
type Server struct {
	cfg      Config
	maps     *lru[*jsmap.Bundle]
	mappings *lru[*retrace.Mapping]
	syms     *lru[*native.Symbolizer]
	files    *lru[string] // downloaded debug files (sha → path)
}

// New builds a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxDebugFile <= 0 {
		cfg.MaxDebugFile = DefaultMaxDebugFile
	}
	if cfg.MaxMap <= 0 {
		cfg.MaxMap = DefaultMaxMap
	}
	if cfg.Demangle == nil {
		cfg.Demangle = native.DemangleSwift
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = os.TempDir() + "/tt-symbolicator"
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	s := &Server{cfg: cfg}
	s.maps = newLRU[*jsmap.Bundle](64, nil)
	s.mappings = newLRU[*retrace.Mapping](16, nil)
	// close evicted symbolizers / delete evicted files later: a concurrent request may still use them
	s.syms = newLRU(16, func(_ string, v *native.Symbolizer) { time.AfterFunc(2*time.Minute, func() { v.Close() }) })
	s.files = newLRU(32, func(_ string, p string) { time.AfterFunc(3*time.Minute, func() { os.Remove(p) }) })
	return s, nil
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("POST /v1/artifacts", s.authed(s.upload))
	mux.Handle("GET /v1/artifacts", s.authed(s.list))
	mux.Handle("DELETE /v1/artifacts", s.authed(s.deleteRelease))
	mux.Handle("POST /v1/symbolicate", s.authed(s.symbolicate))
	return s.recoverer(mux)
}

type ctxKey struct{}

func projectFrom(ctx context.Context) int64 { p, _ := ctx.Value(ctxKey{}).(int64); return p }

func (s *Server) authed(h func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project, err := s.cfg.Auth.Project(r.Context(), auth.BearerKey(r))
		if errors.Is(err, auth.ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing api key (Authorization: Bearer <api key>)")
			return
		}
		if err != nil {
			s.cfg.Log.Error("auth lookup failed", "err", err)
			writeErr(w, http.StatusServiceUnavailable, "auth backend unavailable")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, project)))
	})
}

func (s *Server) recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.cfg.Log.Error("panic", "err", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.cfg.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "s3": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ixs, err := s.cfg.Store.ListIndexes(r.Context(), projectFrom(r.Context()), q.Get("app"), q.Get("release"))
	if err != nil {
		s.cfg.Log.Error("list artifacts", "err", err)
		writeErr(w, http.StatusBadGateway, "blob store error")
		return
	}
	type item struct {
		Kind    string `json:"kind"`
		App     string `json:"app"`
		Release string `json:"release"`
		store.File
	}
	out := []item{}
	for _, ix := range ixs {
		if k := q.Get("kind"); k != "" && k != ix.Kind {
			continue
		}
		for _, f := range ix.Files {
			out = append(out, item{Kind: ix.Kind, App: ix.App, Release: ix.Release, File: f})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out})
}

func (s *Server) deleteRelease(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	app, release := q.Get("app"), q.Get("release")
	if app == "" || release == "" {
		writeErr(w, http.StatusBadRequest, "app and release are required")
		return
	}
	kinds := []string{store.KindSourcemap, store.KindDSYM, store.KindR8, store.KindProguard, store.KindDart, store.KindNDK}
	if k := q.Get("kind"); k != "" {
		if !store.ValidKind(k) {
			writeErr(w, http.StatusBadRequest, "unknown kind")
			return
		}
		kinds = []string{k}
	}
	project := projectFrom(r.Context())
	total := 0
	for _, k := range kinds {
		n, err := s.cfg.Store.DeleteRelease(r.Context(), project, k, app, release)
		total += n
		if err != nil {
			writeErr(w, http.StatusBadGateway, "blob store error")
			return
		}
	}
	if s.cfg.Cache != nil {
		s.cfg.Cache.Bump(r.Context(), scope(project, app, release))
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": total})
}

func scope(project int64, app, release string) string {
	return fmt.Sprintf("%d:%s:%s", project, app, release)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// validLabel checks app / release values.
func validLabel(s string) bool {
	if s == "" || len(s) > 200 || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
