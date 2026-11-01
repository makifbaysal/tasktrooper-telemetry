package store

import (
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

func TestDepEdge(t *testing.T) {
	tests := []struct {
		name                          string
		dep, kind, child              string
		wantTo, wantTier, wantSysName string
	}{
		{"db", "postgres · orders-db", "db", "", "dep:postgres · orders-db", "data", "postgres\x00orders-db"},
		{"instrumented http", "http · payments-api", "http", "payments-api", "payments-api", "service", ""},
		{"external http", "http · api.stripe.com", "http", "", "dep:http · api.stripe.com", "external", "http\x00api.stripe.com"},
		{"no system", "reco-cache", "cache", "", "dep:reco-cache", "data", "\x00reco-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := depEdge("orders-api", tt.dep, tt.kind, tt.child, 10, 0, 5)
			if r.to != tt.wantTo || r.toTier != tt.wantTier || r.toSystem != tt.wantSysName {
				t.Fatalf("got to=%q tier=%q sys=%q", r.to, r.toTier, r.toSystem)
			}
		})
	}
}

func TestBuildServiceMap(t *testing.T) {
	services := []model.Service{
		{ID: "shop-web", Name: "shop-web", Type: "web", Status: model.OK},
		{ID: "bff", Name: "bff", Type: "http", Status: model.Critical},
		{ID: "reco", Name: "reco", Type: "http", Status: model.Warning},
	}
	callers := []mapEdgeRow{
		{from: "shop-web", to: "bff", toKind: "web", toTier: "client", calls: 600, errors: 0, p95: 900},
		{from: "bff", to: "reco", toKind: "service", toTier: "service", calls: 300, errors: 30, p95: 400},
		{from: "acme", to: "bff", toKind: "partner", toTier: "client", calls: 60, errors: 0, p95: 100},
	}
	deps := []mapEdgeRow{
		depEdge("bff", "http · reco", "http", "reco", 300, 30, 410), // duplicate of the callers edge → dropped
		depEdge("reco", "postgres · reco-db", "db", "", 1200, 0, 700),
		depEdge("bff", "http · api.stripe.com", "http", "", 60, 6, 300),
		depEdge("bff", "http · api.iyzico.com", "http", "iyzico", 6, 0, 200), // peer.service without telemetry
	}
	inc := map[string]*model.MapIncident{"bff": {ID: "7", Title: "Homepage latency", Severity: "critical", State: "firing", Open: 1}}
	m := buildServiceMap(services, callers, deps, inc, 60)

	nodes := map[string]model.MapNode{}
	for _, n := range m.Nodes {
		nodes[n.ID] = n
	}
	edges := map[string]model.MapEdge{}
	for _, e := range m.Edges {
		edges[e.ID] = e
	}
	tests := []struct {
		name string
		ok   bool
	}{
		{"7 nodes", len(m.Nodes) == 7},
		{"6 edges (bff→reco merged once)", len(m.Edges) == 6},
		{"uninstrumented peer.service is external", nodes["dep:http · iyzico"].Tier == "external" && nodes["dep:http · iyzico"].Name == "iyzico"},
		{"web app is a client", nodes["shop-web"].Tier == "client"},
		{"partner caller node", nodes["acme"].Tier == "client" && nodes["acme"].Kind == "partner"},
		{"db node split system/name", nodes["dep:postgres · reco-db"].System == "postgres" && nodes["dep:postgres · reco-db"].Name == "reco-db"},
		{"db node rpm", nodes["dep:postgres · reco-db"].Rpm == 20},
		{"external node", nodes["dep:http · api.stripe.com"].Tier == "external"},
		{"external node health from errors", nodes["dep:http · api.stripe.com"].Status == model.Critical},
		{"incident attached", nodes["bff"].Incident != nil && nodes["bff"].Incident.ID == "7"},
		{"service status kept", nodes["bff"].Status == model.Critical},
		{"edge rate", edges["bff→reco"].CallsPerMin == 5 && edges["bff→reco"].ErrorRate == 0.1},
		{"edge health", edges["bff→reco"].Status == model.Critical && edges["shop-web→bff"].Status == model.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.ok {
				t.Fatalf("failed: nodes=%+v edges=%+v", m.Nodes, m.Edges)
			}
		})
	}
}

func TestIncidentRank(t *testing.T) {
	tests := []struct {
		sev, state string
		want       int
	}{
		{"critical", "firing", 3}, {"critical", "acknowledged", 2}, {"warning", "firing", 1}, {"warning", "acknowledged", 0},
	}
	for _, tt := range tests {
		if got := incidentRank(&model.MapIncident{Severity: tt.sev, State: tt.state}); got != tt.want {
			t.Errorf("%s/%s = %d, want %d", tt.sev, tt.state, got, tt.want)
		}
	}
}
