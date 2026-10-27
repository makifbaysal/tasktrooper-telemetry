package suggest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func TestBuild(t *testing.T) {
	items := []*model.Item{
		{ID: 1, Kind: "endpoint", Service: "orders", Key: "POST /v1/orders", Criticality: "critical", File: "o/main.go", Line: 16,
			Stats: &model.Stats{Count: 20160, Errors: 40, P95: 300, RPM: 2, ErrorRate: 0.002}},
		{ID: 2, Kind: "endpoint", Service: "orders", Key: "GET /v1/orders/:id", Criticality: "high"},
		{ID: 3, Kind: "endpoint", Service: "orders", Key: "GET /v1/products", Criticality: "low"},
		{ID: 4, Kind: "endpoint", Service: "orders", Key: "POST /v1/refunds", Criticality: "critical", Monitored: true},
		{ID: 5, Kind: "consumer", Service: "orders", Key: "fulfilment", Source: "orders.created", Framework: "sarama", Criticality: "high"},
		{ID: 6, Kind: "consumer", Service: "jobs", Key: "emails", Source: "emails", Framework: "bullmq", Criticality: "high",
			Stats: &model.Stats{Count: 1008}},
		{ID: 7, Kind: "cron", Service: "jobs", Key: "nightly-settlement", Source: "0 2 * * *", Criticality: "high"},
		{ID: 0, Kind: "endpoint", Service: "orders", Key: "POST /unsaved", Criticality: "critical"},
		{ID: 8, Kind: "endpoint", Service: "orders", Key: "POST /legacy", Criticality: "critical", Dead: true},
	}
	got := Build(items, []Cluster{{Slug: "kafka-main", Kind: "kafka"}, {Slug: "rmq", Kind: "rabbitmq"}})
	byKey := map[string]Suggestion{}
	for _, s := range got {
		byKey[strings.Split(s.Target, " · ")[1]+"|"+s.Kind] = s
	}
	if len(got) != 7 {
		for _, s := range got {
			t.Log(s.Target, s.Kind)
		}
		t.Fatalf("want 7 suggestions, got %d", len(got))
	}
	er := byKey["POST /v1/orders|metric_threshold"]
	if !er.Calibrated || er.Draft.Condition.Threshold != 0.01 || er.Draft.Severity != "critical" || er.CodeRef != "o/main.go:16" ||
		er.Draft.Target.Endpoint != "POST /v1/orders" || er.Draft.Query["metric"] != "error_rate" || er.Condition != "error rate > 1% for 5m" {
		t.Fatalf("error rate %+v", er)
	}
	lat := byKey["POST /v1/orders|latency"]
	if !lat.Calibrated || lat.Draft.Condition.Threshold != 450 || lat.Condition != "p95 > 450 ms for 10m" {
		t.Fatalf("latency %+v", lat)
	}
	if _, ok := byKey["POST /v1/orders|error_new"]; !ok {
		t.Fatal("error_new for critical endpoint")
	}
	high := byKey["GET /v1/orders/:id|metric_threshold"]
	if high.Calibrated || high.Draft.Condition.Threshold != 0.05 || high.Draft.Severity != "warning" {
		t.Fatalf("uncalibrated high %+v", high)
	}
	if _, ok := byKey["GET /v1/orders/:id|latency"]; ok {
		t.Fatal("high endpoints get error rate only")
	}
	lag := byKey["fulfilment|consumer_lag"]
	if lag.Draft.Queue == nil || lag.Draft.Queue.ClusterID != "kafka-main" || lag.Draft.Queue.Group != "fulfilment" || lag.Draft.Queue.Metric != "time_behind" {
		t.Fatalf("lag %+v", lag)
	}
	stalled := byKey["emails|job_missed"]
	if !stalled.Calibrated || stalled.Draft.Query["intervalMin"] != 100.0 || stalled.Draft.Target.Process != "emails" {
		t.Fatalf("stalled %+v", stalled.Draft)
	}
	cron := byKey["nightly-settlement|job_missed"]
	if cron.Draft.Query["intervalMin"] != 1440.0 || cron.Draft.Condition.Threshold != 1584 {
		t.Fatalf("cron %+v", cron.Draft)
	}
	// The draft is the api POST /monitors body.
	b, _ := json.Marshal(er.Draft)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	for _, k := range []string{"name", "kind", "target", "query", "condition", "severity", "channels"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("draft lacks %s: %s", k, b)
		}
	}
	if c := body["condition"].(map[string]any); c["forMin"] != 5.0 || c["op"] != ">" {
		t.Fatalf("condition %v", c)
	}
}

func TestCronIntervalMin(t *testing.T) {
	tests := map[string]float64{
		"*/15 * * * *": 15, "0 * * * *": 60, "0 */6 * * *": 360, "0 2 * * *": 1440, "0 9 * * 1-5": 1440, "0 0 * * 0": 10080,
		"0 0 1 * *": 43200, "@hourly": 60, "@every 90s": 1.5, "30.0": 1, "0 */5 * * * *": 5, "0,30 * * * *": 30, "0 8,20 * * *": 720,
	}
	for expr, want := range tests {
		if got, ok := CronIntervalMin(expr); !ok || got != want {
			t.Errorf("CronIntervalMin(%q) = %v %v, want %v", expr, got, ok, want)
		}
	}
	if _, ok := CronIntervalMin("crontab(?)"); ok {
		t.Error("garbage must not parse")
	}
}

func TestLoadTests(t *testing.T) {
	items := []*model.Item{
		{Kind: "endpoint", Service: "orders", Key: "POST /v1/orders", Criticality: "critical", Stats: &model.Stats{Count: 604800, P95: 200},
			LLM: &model.LLMResult{LoadtestHint: &model.LoadtestHint{PayloadExample: json.RawMessage(`{"sku":"A1","qty":1}`), Idempotent: false}}},
		{Kind: "endpoint", Service: "orders", Key: "GET /v1/orders/:id", Criticality: "high", Stats: &model.Stats{Count: 1814400, P95: 80}},
		{Kind: "endpoint", Service: "orders", Key: "GET /healthz", Criticality: "low"},
		{Kind: "endpoint", Service: "catalog", Key: "GET /v1/items", Criticality: "high"}, // no critical endpoint → no draft
		{Kind: "endpoint", Service: "pay", Key: "POST /charge", Criticality: "critical"},
		{Kind: "endpoint", Service: "pay", Key: "GRPC /pay.Ledger/Post", Criticality: "critical"},
	}
	got := LoadTests(items, "acme/shop", "abc")
	if len(got) != 2 || got[0].Service != "orders" || got[1].Service != "pay" {
		t.Fatalf("scenarios %+v", got)
	}
	o := got[0].Spec
	if o.Mode != "rps" || o.Weights["POST /v1/orders"] != 0.25 || o.Weights["GET /v1/orders/:id"] != 0.75 {
		t.Fatalf("weights %v mode %s", o.Weights, o.Mode)
	}
	if o.Levels[0].Target != 4 || o.Levels[2].Target != 12 || o.Levels[0].DurationS != 180 || len(o.Stages) != 7 {
		t.Fatalf("levels %+v stages %d", o.Levels, len(o.Stages))
	}
	if r := o.Requests["GET /v1/orders/:id"]; r.Path != "/v1/orders/{{int 1-1000}}" || r.JSON != nil {
		t.Fatalf("request %+v", r)
	}
	if r := o.Requests["POST /v1/orders"]; string(r.JSON.(json.RawMessage)) != `{"sku":"A1","qty":1}` || r.Notes == "" {
		t.Fatalf("post %+v", r)
	}
	if o.Thresholds["p95Ms"] != 300 || o.Thresholds["errorRate"] != 0.01 {
		t.Fatalf("thresholds %v", o.Thresholds)
	}
	p := got[1].Spec
	if p.Mode != "vus" || len(p.Steps) != 1 || p.Weights["POST /charge"] != 1 {
		t.Fatalf("pay %+v", p)
	}
}
