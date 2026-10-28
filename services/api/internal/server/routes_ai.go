package server

import (
	"net/http"
	"strings"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
)

// incidentSummary is an explicit, authenticated action; no telemetry or alert
// ingestion path invokes a model automatically.
func (s *Server) incidentSummary(w http.ResponseWriter, r *http.Request) {
	if s.AI == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "AI provider unavailable"})
		return
	}
	var in ai.Input
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if len(in.Title) == 0 || len(in.Title) > 200 || len(in.Evidence) == 0 || len(in.Evidence) > 10 {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "title must be 1-200 characters and evidence must contain 1-10 items"})
		return
	}
	for i, evidence := range in.Evidence {
		in.Evidence[i] = strings.TrimSpace(evidence)
		if len(in.Evidence[i]) == 0 || len(in.Evidence[i]) > 500 {
			writeJSON(w, http.StatusBadRequest, apiError{Error: "each evidence item must be 1-500 characters"})
			return
		}
	}
	result, err := s.AI.Summarize(r.Context(), in)
	if err != nil {
		s.Log.Error("AI summary failed", "err", err)
		writeJSON(w, http.StatusBadGateway, apiError{Error: "AI provider failed"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
