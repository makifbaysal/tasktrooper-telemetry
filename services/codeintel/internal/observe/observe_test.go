package observe

import (
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func TestMatch(t *testing.T) {
	tr := &Traffic{
		Endpoints: []Endpoint{
			{Service: "orders", Route: "/v1/orders/{id}", Method: "GET", Count: 10080, Errors: 101, P95: 120}, // Go http.route
			{Service: "orders", Route: "/v1/orders", Method: "POST", Count: 2016, Errors: 0, P95: 300},
			{Service: "orders", Route: "/v1/orders", Method: "GET", Count: 5, P95: 10}, // other method → shadow
			{Service: "orders", Route: "/legacy/export/:id", Method: "GET", Count: 7},  // not in code → shadow
			{Service: "orders", Route: "/legacy/export/123", Method: "GET", Count: 3},  // raw path, same template
			{Service: "orders", Route: "/django", Method: "PUT", Count: 4},
			{Service: "billing", Route: "/x", Method: "GET", Count: 9}, // service not in repo → no shadow
			{Service: "orders", Route: "SELECT orders", Method: "", Count: 9},
		},
		Consumers: []Consumer{
			{Service: "orders", Process: "order-events.fulfilment", Topic: "orders.created", Count: 100, Failed: 1},
			{Service: "orders", Process: "other", Topic: "payments.dlq", Count: 4},
		},
		Seen30d:     map[string]bool{"orders\x00r\x00/v1/carts/:": true},
		Services30d: map[string]bool{"orders": true},
	}
	items := []*model.Item{
		{Kind: "endpoint", Service: "orders", Key: "GET /v1/orders/:orderId"},
		{Kind: "endpoint", Service: "orders", Key: "POST /v1/orders"},
		{Kind: "endpoint", Service: "orders", Key: "ANY /django"},
		{Kind: "endpoint", Service: "orders", Key: "DELETE /v1/orders/:id"}, // dead: no traffic in 30 d
		{Kind: "endpoint", Service: "orders", Key: "GET /v1/carts/:id"},     // quiet this week, seen in 30 d → not dead
		{Kind: "endpoint", Service: "search", Key: "GET /q"},                // uninstrumented service → never dead
		{Kind: "consumer", Service: "orders", Key: "order-events.fulfilment", Source: "orders.created"},
		{Kind: "consumer", Service: "orders", Key: "replayer", Source: "payments.dlq"},
		{Kind: "consumer", Service: "orders", Key: "idle", Source: "idle.topic"},
	}
	shadows := Match(items, tr, []string{"orders"})
	want := []struct {
		observed, dead bool
	}{{true, false}, {true, false}, {true, false}, {false, true}, {false, false}, {false, false}, {true, false}, {true, false}, {false, true}}
	for i, w := range want {
		if items[i].Observed != w.observed || items[i].Dead != w.dead {
			t.Errorf("%s: observed=%v dead=%v, want %v %v", items[i].Key, items[i].Observed, items[i].Dead, w.observed, w.dead)
		}
	}
	if s := items[0].Stats; s == nil || s.Count != 10080 || s.RPM != 1 || s.ErrorRate < 0.01 || s.P95 != 120 {
		t.Fatalf("stats %+v", items[0].Stats)
	}
	keys := map[string]float64{}
	for _, s := range shadows {
		if !s.Shadow || !s.Observed || s.Service != "orders" {
			t.Fatalf("shadow %+v", s)
		}
		keys[s.Key] = s.Stats.Count
	}
	if len(keys) != 2 || keys["GET /v1/orders"] != 5 || keys["GET /legacy/export/:id"] != 10 {
		t.Fatalf("shadows %v", keys)
	}
}

func TestMarkMonitored(t *testing.T) {
	items := []*model.Item{
		{Kind: "endpoint", Service: "orders", Key: "POST /v1/orders"},
		{Kind: "endpoint", Service: "orders", Key: "GET /v1/orders/:id"},
		{Kind: "endpoint", Service: "orders", Key: "ANY /admin"},
		{Kind: "consumer", Service: "orders", Key: "fulfilment"},
		{Kind: "endpoint", Service: "billing", Key: "POST /v1/orders"},
	}
	MarkMonitored(items, []MonitorRef{
		{Service: "orders", Endpoint: "POST /v1/orders"},
		{Service: "orders", Route: "/v1/orders/{id}"},
		{Service: "orders", Endpoint: "GET /admin"},
		{Service: "orders", Process: "fulfilment"},
	})
	want := []bool{true, true, true, true, false}
	for i, w := range want {
		if items[i].Monitored != w {
			t.Errorf("%s %s monitored=%v", items[i].Service, items[i].Key, items[i].Monitored)
		}
	}
}
