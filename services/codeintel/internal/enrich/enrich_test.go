package enrich

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func TestStatic(t *testing.T) {
	tests := []struct {
		item model.Item
		want string
	}{
		{model.Item{Kind: "endpoint", Key: "POST /v1/checkout"}, model.Critical},
		{model.Item{Kind: "endpoint", Key: "DELETE /v1/orders/:id"}, model.Critical},
		{model.Item{Kind: "endpoint", Key: "GET /v1/orders/:id"}, model.High},
		{model.Item{Kind: "endpoint", Key: "POST /v1/reviews"}, model.Medium},
		{model.Item{Kind: "endpoint", Key: "GET /v1/products"}, model.Low},
		{model.Item{Kind: "endpoint", Key: "GET /healthz"}, model.Low},
		{model.Item{Kind: "endpoint", Key: "POST /auth/login"}, model.Critical},
		{model.Item{Kind: "endpoint", Key: "ANY /api/orders/:pk/refund", Snippet: "order.save()"}, model.Critical},
		{model.Item{Kind: "endpoint", Key: "ANY /reports", Snippet: "return render()"}, model.Low},
		{model.Item{Kind: "endpoint", Key: "GRPC /payments.v1.Ledger/PostEntry"}, model.Critical},
		{model.Item{Kind: "endpoint", Key: "GRPC /payments.v1.Ledger/GetBalance"}, model.High},
		{model.Item{Kind: "consumer", Key: "replayer", Source: "payments.dlq"}, model.High},
		{model.Item{Kind: "consumer", Key: "fulfilment", Source: "orders.created"}, model.High},
		{model.Item{Kind: "consumer", Key: "emails", Source: "emails"}, model.Medium},
		{model.Item{Kind: "cron", Key: "nightly-settlement"}, model.High},
		{model.Item{Kind: "cron", Key: "rebuildSearchIndex"}, model.Medium},
		{model.Item{Kind: "view", Key: "/checkout"}, model.High},
		{model.Item{Kind: "view", Key: "/"}, model.Medium},
		{model.Item{Kind: "dependency", Key: "http · api.stripe.com"}, model.High},
		{model.Item{Kind: "dependency", Key: "db · postgres"}, model.Medium},
	}
	for _, tt := range tests {
		it := tt.item
		Static(&it)
		if it.Criticality != tt.want {
			t.Errorf("%s %s: got %s (%s), want %s", it.Kind, it.Key, it.Criticality, it.Rationale, tt.want)
		}
	}
	it := model.Item{Kind: "endpoint", Key: "POST /v1/reviews", Stats: &model.Stats{RPM: 250}}
	Static(&it)
	BumpForTraffic(&it)
	if it.Criticality != model.High {
		t.Fatal("traffic bump")
	}
}

func TestNewLLMDisabledWithoutConfig(t *testing.T) {
	if NewLLM("", "m") != nil || NewLLM("k", "") != nil {
		t.Fatal("LLM must be disabled without key and model")
	}
}

func TestLLMEnrichStructuredOutput(t *testing.T) {
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		reqs = append(reqs, req)
		out := `{"items":[{"id":0,"criticality":"critical","business_domain":"payments","summary":"Captures a payment.","failure_modes":["provider timeout"],"suggested_monitors":[{"template":"latency","reason":"provider calls"}],"loadtest_hint":{"payload_example":"{\"amount\":100}","idempotent":false,"notes":""}},{"id":1,"criticality":"low","business_domain":"internal","summary":"Health.","failure_modes":[],"suggested_monitors":[],"loadtest_hint":{"payload_example":"","idempotent":true,"notes":""}}]}`
		resp := map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": req["model"],
			"content": []any{map[string]any{"type": "text", "text": out}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 10, "cache_read_input_tokens": 0}}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	l := NewLLM("test-key", "test-model-from-config", option.WithBaseURL(srv.URL))
	items := []*model.Item{
		{Kind: "endpoint", Key: "POST /v1/payments/:id/capture", Snippet: "func capture() {}", Criticality: model.Medium},
		{Kind: "endpoint", Key: "GET /healthz", Criticality: model.Low},
	}
	if _, err := l.Enrich(context.Background(), RepoSummary{Name: "acme/shop", Inventory: []string{"endpoint shop GET /healthz"}}, items, nil); err != nil {
		t.Fatal(err)
	}
	if items[0].Criticality != model.Critical || items[0].LLM == nil || items[0].Summary != "Captures a payment." ||
		string(items[0].LLM.LoadtestHint.PayloadExample) != `{"amount":100}` || items[0].LLM.Model != "test-model-from-config" {
		t.Fatalf("item0 %+v %+v", items[0], items[0].LLM)
	}
	if items[1].LLM == nil || !items[1].LLM.LoadtestHint.Idempotent {
		t.Fatalf("item1 %+v", items[1])
	}
	req := reqs[0]
	if req["model"] != "test-model-from-config" {
		t.Fatalf("model %v", req["model"])
	}
	oc, _ := req["output_config"].(map[string]any)
	if f, _ := oc["format"].(map[string]any); f["type"] != "json_schema" {
		t.Fatalf("output_config %v", oc)
	}
	sys, _ := req["system"].([]any)
	last, _ := sys[len(sys)-1].(map[string]any)
	if last["cache_control"] == nil || !strings.Contains(last["text"].(string), "acme/shop") {
		t.Fatalf("system %v", sys)
	}
	if _, ok := req["thinking"]; ok {
		t.Fatal("thinking must not be set")
	}
}
