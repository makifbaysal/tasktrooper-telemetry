package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/symbolicate"
)

// nonBreaching is a value op/th never fires on (used for low-volume buckets in previews).
func nonBreaching(op string, th float64) float64 {
	switch op {
	case ">=":
		return th - math.Abs(th) - 1
	case "<=":
		return th + math.Abs(th) + 1
	}
	return th // strict > / < never fire at the threshold itself
}

// decodeMonitorPatch reads a PATCH body and records whether mutedUntil was present (null = unmute).
func decodeMonitorPatch(r *http.Request) (model.MonitorPatch, error) {
	var p model.MonitorPatch
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return p, store.Invalid("invalid body")
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&p); err != nil {
		return p, store.Invalid("invalid JSON body: " + err.Error())
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(body, &keys)
	_, p.MutedUntilSet = keys["mutedUntil"]
	return p, nil
}

// symbolicate asks the symbolicator for stack (contract §12, 2 s budget); any failure keeps the raw stack.
func (s *Server) symbolicate(r *http.Request, app, release, platform, stack string) (string, []model.Frame, bool) {
	out, frames, ok, err := s.Symbolicator.Symbolicate(r.Context(), r.Header.Get("Authorization"),
		symbolicate.Request{App: app, Release: release, Platform: platform, Stack: stack})
	if err != nil {
		s.Log.Warn("symbolication failed, returning the raw stack", "err", err, "app", app, "release", release)
	}
	if frames == nil {
		frames = []model.Frame{}
	}
	return out, frames, ok
}

func (s *Server) adminRoutes(r chi.Router) {
	// ---- maintenance windows
	r.Get("/maintenance-windows", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListMaintenanceWindows(r.Context(), s.Now(), store.MaintenanceFilter{Only: qs(r, "only")})
		s.reply(w, r, v, err)
	})
	r.Post("/maintenance-windows", func(w http.ResponseWriter, r *http.Request) {
		var in model.MaintenanceWindowInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateMaintenanceWindow(r.Context(), s.Now(), in)
		s.created(w, r, v, err)
	})
	r.Get("/maintenance-windows/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetMaintenanceWindow(r.Context(), s.Now(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Put("/maintenance-windows/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in model.MaintenanceWindowInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.UpdateMaintenanceWindow(r.Context(), s.Now(), param(r, "id"), in)
		s.reply(w, r, v, err)
	})
	r.Delete("/maintenance-windows/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.noContent(w, r, s.Store.DeleteMaintenanceWindow(r.Context(), param(r, "id")))
	})

	// ---- notification channels
	r.Get("/notification-channels", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListNotificationChannels(r.Context())
		s.reply(w, r, v, err)
	})
	r.Post("/notification-channels", func(w http.ResponseWriter, r *http.Request) {
		var in model.NotificationChannelInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateNotificationChannel(r.Context(), in)
		s.created(w, r, v, err)
	})
	r.Get("/notification-channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetNotificationChannel(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Put("/notification-channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in model.NotificationChannelInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.UpdateNotificationChannel(r.Context(), param(r, "id"), in)
		s.reply(w, r, v, err)
	})
	r.Delete("/notification-channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.noContent(w, r, s.Store.DeleteNotificationChannel(r.Context(), param(r, "id")))
	})

	// ---- cloud integrations (ADR-012): admin for mutations
	r = r.With(s.adminWrites)
	r.Get("/integrations", func(w http.ResponseWriter, r *http.Request) {
		p := qs(r, "provider")
		if p != "" && !store.Providers[p] && p != "slack" && p != "github" {
			s.fail(w, r, store.Invalid("provider must be aws|gcp|vercel|neon|azure|slack|github"))
			return
		}
		v, err := s.Store.ListIntegrations(store.WithProviderFilter(r.Context(), p))
		s.reply(w, r, v, err)
	})
	r.Post("/integrations", func(w http.ResponseWriter, r *http.Request) {
		var in model.IntegrationInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateIntegration(r.Context(), in)
		s.created(w, r, v, err)
	})
	r.Post("/integrations/test", func(w http.ResponseWriter, r *http.Request) {
		var in model.IntegrationInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		if !store.Providers[in.Provider] {
			s.fail(w, r, store.Invalid("provider must be aws|gcp|vercel|neon|azure"))
			return
		}
		if in.Secret == "" && in.Credentials != nil {
			in.Secret = in.Credentials.ClientSecret
		}
		if s.CloudTest == nil {
			writeJSON(w, http.StatusNotImplemented, apiError{Error: "integration tests are not configured"})
			return
		}
		writeJSON(w, http.StatusOK, s.CloudTest(r.Context(), in.Provider, in.Config, in.Secret))
	})
	r.Get("/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetIntegration(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Patch("/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p model.IntegrationPatch
		if err := decode(r, &p); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.UpdateIntegration(r.Context(), param(r, "id"), p)
		s.reply(w, r, v, err)
	})
	r.Delete("/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.noContent(w, r, s.Store.DeleteIntegration(r.Context(), param(r, "id")))
	})
	r.Post("/integrations/{id}/sync", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.RequestIntegrationSync(r.Context(), param(r, "id"), s.Now())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, v)
	})
}

// codeInsightRoutes: code intelligence (ADR-014). services/codeintel analyzes; the api registers repos, queues
// analyses and turns suggestions into monitors.
func (s *Server) codeInsightRoutes(r chi.Router) {
	r.Get("/repos", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListRepos(r.Context())
		s.reply(w, r, v, err)
	})
	r.Post("/repos", func(w http.ResponseWriter, r *http.Request) {
		var in model.RepoInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateRepo(r.Context(), in)
		s.created(w, r, v, err)
	})
	r.Get("/repos/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetRepo(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Delete("/repos/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.noContent(w, r, s.Store.DeleteRepo(r.Context(), param(r, "id")))
	})
	r.Post("/repos/{id}/analyze", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.AnalyzeRepo(r.Context(), param(r, "id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, v)
	})
	r.Get("/inventory", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListInventory(r.Context(), store.InventoryFilter{RepoID: qs(r, "repoId"), Kind: qs(r, "kind"), Service: qs(r, "service")})
		s.reply(w, r, v, err)
	})
	r.Get("/suggestions", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListSuggestions(r.Context(), qs(r, "state"))
		s.reply(w, r, v, err)
	})
	r.Post("/suggestions/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
		var o model.SuggestionAccept
		if r.ContentLength != 0 {
			if err := decode(r, &o); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		v, err := s.Store.AcceptSuggestion(r.Context(), param(r, "id"), o)
		s.reply(w, r, v, err)
	})
	r.Post("/suggestions/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.RejectSuggestion(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
}
