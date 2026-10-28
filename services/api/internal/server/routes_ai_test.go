package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
)

func TestIncidentSummaryMock(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	path := "/api/v1/ai/incident-summary"
	body := `{"title":"Checkout errors","evidence":["5xx rate rose to 8%"]}`
	if got := do(h, http.MethodPost, path, "", body); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got.Code)
	}
	got := do(h, http.MethodPost, path, "good", body)
	if got.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
	}
	var result ai.Result
	if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil || result.Provider != "mock" || result.Usage != (ai.Usage{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := do(h, http.MethodPost, path, "good", `{"title":"","evidence":[]}`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d", got.Code)
	}
}
