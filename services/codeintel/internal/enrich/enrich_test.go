package enrich

import (
	"testing"

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

func TestTrivial(t *testing.T) {
	for key, want := range map[string]bool{"GET /healthz": true, "ANY /metrics": true, "GET /static/app.js": true,
		"GET /v1/orders": false, "POST /status-updates": false} {
		if got := Trivial(&model.Item{Kind: model.KindEndpoint, Key: key}); got != want {
			t.Errorf("Trivial(%q) = %v, want %v", key, got, want)
		}
	}
	if Trivial(&model.Item{Kind: model.KindConsumer, Key: "healthz"}) {
		t.Error("only endpoints are trivial")
	}
}
