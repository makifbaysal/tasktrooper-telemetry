package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/blob"
	"github.com/tasktrooper/telemetry/services/api/internal/llmprice"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// ObsStore: session replay (ADR-021, contracts §23), continuous profiling (ADR-022, §24), AI monitoring (ADR-023, §25).
type ObsStore interface {
	ListReplays(ctx context.Context, q store.Q, f store.ReplayFilter) (model.ReplayList, error)
	GetReplay(ctx context.Context, sessionID string) (*model.ReplayDetail, error)
	ReplaySegmentKey(ctx context.Context, sessionID string, segment int) (string, error)
	ProfileTypes(ctx context.Context, f store.ProfileFilter) (model.ProfileTypes, error)
	Flamegraph(ctx context.Context, f store.ProfileFilter) (*model.Flamegraph, error)
	FlameDiff(ctx context.Context, f, base store.ProfileFilter) (*model.FlameDiff, error)
	TopFunctions(ctx context.Context, f store.ProfileFilter, limit int) (*model.TopFunctions, error)
	ProfileSeries(ctx context.Context, w timerange.Window, f store.ProfileFilter) (model.SeriesSet, error)
	AIOverview(ctx context.Context, q store.Q, f store.AIFilter) (*model.AIOverview, error)
	AICalls(ctx context.Context, q store.Q, f store.AIFilter) (model.AICalls, error)
	AICall(ctx context.Context, traceID, spanID string) (*model.AICallDetail, error)
	ListLLMPrices(ctx context.Context) (model.AIPrices, error)
	SetLLMPrice(ctx context.Context, in model.AIPriceInput) (*llmprice.Price, error)
	DeleteLLMPrice(ctx context.Context, provider, mdl string) error
	ServiceHasAI(ctx context.Context, q store.Q, service string) bool
}

func profileFilter(r *http.Request, w timerange.Window) store.ProfileFilter {
	return store.ProfileFilter{Service: qs(r, "service"), Type: qs(r, "type"), Env: qs(r, "env"), Version: qs(r, "version"), Host: qs(r, "host"),
		Route: qs(r, "route"), Method: strings.ToUpper(qs(r, "method")), From: w.From, To: w.To}
}

func qms(r *http.Request, k string) (time.Time, bool) {
	v, err := strconv.ParseInt(qs(r, k), 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(v).UTC(), true
}

// diffFilters parses /profiles/diff: baseVersion & version (window = range, default 24h) or baseFrom, baseTo, from, to.
func (s *Server) diffFilters(r *http.Request) (store.ProfileFilter, store.ProfileFilter, error) {
	var target, base store.ProfileFilter
	if qs(r, "baseVersion") != "" || qs(r, "baseFrom") == "" {
		if qs(r, "baseVersion") == "" || qs(r, "version") == "" {
			return target, base, store.Invalid("diff needs baseVersion & version, or baseFrom, baseTo, from, to (unix ms)")
		}
		rg := timerange.R24h
		if qs(r, "range") != "" {
			var err error
			if rg, err = timerange.Parse(qs(r, "range")); err != nil {
				return target, base, store.Invalid(err.Error())
			}
		}
		w := rg.Window(s.Now())
		target = profileFilter(r, w)
		base = target
		base.Version = qs(r, "baseVersion")
		return target, base, nil
	}
	bf, ok1 := qms(r, "baseFrom")
	bt, ok2 := qms(r, "baseTo")
	f, ok3 := qms(r, "from")
	t, ok4 := qms(r, "to")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return target, base, store.Invalid("baseFrom, baseTo, from and to must be unix ms")
	}
	target = profileFilter(r, timerange.Window{From: f, To: t})
	target.Version = ""
	base = target
	base.From, base.To = bf, bt
	return target, base, nil
}

func (s *Server) obsRoutes(r chi.Router) {
	// ---- session replay (§23)
	r.Get("/replays", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		minDur, _ := strconv.ParseFloat(qs(r, "minDurationS"), 64)
		v, err := s.Store.ListReplays(r.Context(), q, store.ReplayFilter{App: qs(r, "app"), Q: qs(r, "q"), HasErrors: qs(r, "hasErrors") == "true",
			MinDurationS: minDur, Limit: qint(r, "limit")})
		s.reply(w, r, v, err)
	}))
	r.Get("/replays/{sessionId}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetReplay(r.Context(), param(r, "sessionId"))
		s.reply(w, r, v, err)
	})
	r.Get("/replays/{sessionId}/segments/{segment}", func(w http.ResponseWriter, r *http.Request) {
		if s.Replays == nil {
			writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "replay blob store is not configured (TT_S3_ENDPOINT)"})
			return
		}
		seg, err := strconv.Atoi(param(r, "segment"))
		if err != nil || seg < 0 {
			s.fail(w, r, store.ErrNotFound)
			return
		}
		key, err := s.Store.ReplaySegmentKey(r.Context(), param(r, "sessionId"), seg)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		body, err := s.Replays.Open(r.Context(), key)
		if errors.Is(err, blob.ErrNotFound) {
			err = store.ErrNotFound
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer body.Close()
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Encoding", "gzip") // stored gzip'd by the writer; served as is
		h.Set("Cache-Control", "private, max-age=86400, immutable")
		h.Add("Vary", "Authorization, Cookie")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, body)
	})

	// ---- continuous profiling (§24)
	r.Get("/profiles/types", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ProfileTypes(r.Context(), profileFilter(r, q.W))
		s.reply(w, r, v, err)
	}))
	r.Get("/profiles/flamegraph", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.Flamegraph(r.Context(), profileFilter(r, q.W))
		s.reply(w, r, v, err)
	}))
	r.Get("/profiles/diff", func(w http.ResponseWriter, r *http.Request) {
		f, b, err := s.diffFilters(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.FlameDiff(r.Context(), f, b)
		s.reply(w, r, v, err)
	})
	r.Get("/profiles/top", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.TopFunctions(r.Context(), profileFilter(r, q.W), qint(r, "limit"))
		s.reply(w, r, v, err)
	}))
	r.Get("/profiles/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		f := profileFilter(r, q.W)
		f.Route, f.Method = "", "" // profiles (totals) has no route
		v, err := s.Store.ProfileSeries(r.Context(), q.W, f)
		s.reply(w, r, v, err)
	}))

	// ---- AI / LLM monitoring (§25)
	aiFilter := func(r *http.Request) store.AIFilter {
		return store.AIFilter{Service: qs(r, "service"), Provider: qs(r, "provider"), Model: qs(r, "model"), Route: qs(r, "route"),
			ErrorsOnly: qs(r, "errorsOnly") == "true", Limit: qint(r, "limit")}
	}
	r.Get("/ai/overview", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.AIOverview(r.Context(), q, aiFilter(r))
		s.reply(w, r, v, err)
	}))
	r.Get("/ai/calls", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.AICalls(r.Context(), q, aiFilter(r))
		s.reply(w, r, v, err)
	}))
	r.Get("/ai/calls/{traceId}/{spanId}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.AICall(r.Context(), param(r, "traceId"), param(r, "spanId"))
		s.reply(w, r, v, err)
	})
	r.Get("/ai/prices", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListLLMPrices(r.Context())
		s.reply(w, r, v, err)
	})
	r.Put("/ai/prices", func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		if !store.RoleAtLeast(p.Role, "admin") {
			forbidden(w, "admin role required")
			return
		}
		var in model.AIPriceInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.SetLLMPrice(r.Context(), in)
		if err == nil {
			s.auditAction(r, p.OrgID, "ai_prices.update", v.Provider+"/"+v.Model, map[string]any{"inputPerMTok": v.InputPerMTok,
				"outputPerMTok": v.OutputPerMTok, "cacheReadPerMTok": v.CacheReadPerMTok, "cacheWritePerMTok": v.CacheWritePerMTok})
		}
		s.reply(w, r, v, err)
	})
	r.Delete("/ai/prices", func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		if !store.RoleAtLeast(p.Role, "admin") {
			forbidden(w, "admin role required")
			return
		}
		err := s.Store.DeleteLLMPrice(r.Context(), qs(r, "provider"), qs(r, "model"))
		if err == nil {
			s.auditAction(r, p.OrgID, "ai_prices.delete", qs(r, "provider")+"/"+strings.ToLower(qs(r, "model")), nil)
		}
		s.noContent(w, r, err)
	})
}
