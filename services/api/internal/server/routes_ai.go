package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

// incidentSummary is an explicit, authenticated action; no telemetry or alert
// ingestion path invokes a model automatically. With a real model (TT_ASSISTANT_*, ADR-034) every call reserves and
// records AI credits (ADR-018, feature rca) like the assistant; the offline mock spends nothing.
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
	if err := ai.ValidateInput(in); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	modelID := s.AI.Model()
	p := principalOf(r.Context())
	if modelID != "" {
		d, err := s.Store.AssistantReserve(r.Context(), p.OrgID, p.ProjectID, p.UserID,
			s.Store.AssistantEstimate(modelID, ai.PromptChars(in), ai.IncidentSummaryMaxOutputTokens))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !d.Allowed {
			writeJSON(w, http.StatusTooManyRequests, apiError{Error: "AI quota exceeded (" + d.Reason + ")"})
			return
		}
	}
	result, err := s.AI.Summarize(r.Context(), in)
	if modelID != "" && result.Usage != (ai.Usage{}) { // billed, even when the answer is unusable
		in, out := int64(result.Usage.InputTokens), int64(result.Usage.OutputTokens)
		u := aiquota.Usage{OrgID: p.OrgID, ProjectID: p.ProjectID, Feature: aiquota.FeatureRCA, Model: modelID,
			InputTokens: in, OutputTokens: out, Credits: s.Store.AssistantCredits(modelID, in, out), Ref: "incident-summary"}
		if p.UserID > 0 {
			uid := p.UserID
			u.UserID = &uid
		}
		if rerr := s.Store.AssistantRecord(context.WithoutCancel(r.Context()), u); rerr != nil {
			s.Log.Warn("AI usage not recorded", "err", rerr)
		}
	}
	if err != nil {
		s.Log.Error("AI summary failed", "err", err)
		writeJSON(w, http.StatusBadGateway, apiError{Error: "AI provider failed"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
