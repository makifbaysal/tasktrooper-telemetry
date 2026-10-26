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

	// ---- cloud integrations (ADR-012)
	r.Get("/integrations", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListIntegrations(r.Context())
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
			s.fail(w, r, store.Invalid("provider must be aws|gcp|vercel|neon"))
			return
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

// codeInsightRoutes: code intelligence (repo analysis → inventory → monitor suggestions) has no backend yet. The
// endpoints exist with their final shapes so the UI can call them; they return empty lists.
func (s *Server) codeInsightRoutes(r chi.Router) {
	r.Get("/suggestions", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, []model.CodeSuggestion{}) })
	r.Get("/repos", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, []model.Repo{}) })
	r.Get("/inventory", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, []model.InventoryItem{}) })
}
