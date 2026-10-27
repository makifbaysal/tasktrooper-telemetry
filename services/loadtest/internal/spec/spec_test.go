package spec

import (
	"math"
	"testing"
	"time"
)

// uiSpec is what apps/web saveScenario() POSTs (spec + target + regions).
const uiSpec = `{
  "services": ["orders-api"], "target": "https://staging.shop.com", "regions": ["eu-central", "us-east"],
  "stages": [{"duration": 30, "target": 50}, {"duration": 30, "target": 50}],
  "levels": [{"name": "Warm-up", "ramp": 30, "hold": 30, "target": 50}, {"name": "Peak", "ramp": 0, "hold": 60, "target": 100},
             {"name": "Cool-down", "ramp": 60, "hold": 0, "target": 0}],
  "steps": ["GET /v1/orders/:id", "POST /v1/orders"], "mode": "vus",
  "weights": {"GET /v1/orders/:id": 0.7, "POST /v1/orders": 0.3},
  "requests": {
    "GET /v1/orders/:id": {"params": "id: {{pool.order_ids}}", "headers": "Authorization: Bearer {{session.token}}\nX-Client: {{weighted web:54 ios:46}}", "body": "", "pools": [{"name": "order_ids", "detail": "x"}]},
    "POST /v1/orders": {"params": "", "headers": "Content-Type: application/json", "body": "{\n  \"qty\": \"{{int 1-3}}\"\n}", "pools": []}
  },
  "thinkTime": 1, "progression": "auto", "gate": {"latencyX": 3, "cpu": 0.85},
  "thresholds": ["http_req_failed rate < 1%", "http_req_duration p(95) < 500 ms"]
}`

func TestParseUISpec(t *testing.T) {
	p, err := Parse([]byte(uiSpec))
	if err != nil {
		t.Fatal(err)
	}
	if p.Host() != "staging.shop.com" || p.Mode != "vus" || p.Sequential {
		t.Fatalf("plan basics: %+v", p)
	}
	if got := Duration(p.Stages); got != 180*time.Second {
		t.Fatalf("duration = %v", got)
	}
	if Peak(p.Stages) != 100 || p.PeakVUs() != 100 {
		t.Fatalf("peak = %v / %d", Peak(p.Stages), p.PeakVUs())
	}
	// ramp 0 jumps: right after the warm-up the level is 100
	if v := TargetAt(p.Stages, 60*time.Second+time.Millisecond); v != 100 {
		t.Fatalf("after jump = %v", v)
	}
	if v := TargetAt(p.Stages, 15*time.Second); v != 25 {
		t.Fatalf("mid ramp = %v", v)
	}
	if len(p.Steps) != 2 || p.Steps[0].Method != "GET" || p.Steps[0].Path != "/v1/orders/:id" || math.Abs(p.Steps[0].Weight-0.7) > 1e-9 {
		t.Fatalf("steps: %+v", p.Steps)
	}
	if p.Steps[1].BodyJSON == nil || len(p.Steps[0].Headers) != 2 || p.Steps[0].Params[0].Name != "id" {
		t.Fatalf("request parse: %+v", p.Steps)
	}
	if len(p.Thresholds) != 2 || p.Thresholds[0].Metric != "error_rate" || p.Thresholds[0].Limit != 0.01 ||
		p.Thresholds[1].Metric != "p95" || p.Thresholds[1].Limit != 500 {
		t.Fatalf("thresholds: %+v", p.Thresholds)
	}
	if len(p.Regions) != 2 {
		t.Fatalf("regions: %v", p.Regions)
	}
}

func TestParseVariants(t *testing.T) {
	tests := []struct {
		name, spec string
		wantErr    bool
		check      func(t *testing.T, p *Plan)
	}{
		{"contracts shape", `{"target":"http://localhost:8080","mode":"rps","levels":[{"durationS":10,"target":50}],
		  "steps":["a"],"weights":{"a":100},"requests":{"a":{"method":"POST","path":"/x","json":{"k":"{{uuid}}"},"checks":["status == 201","latency < 300ms"]}},
		  "thresholds":{"p95Ms":200,"errorRate":0.05},"gate":{"abortErrorRate":0.2}}`, false, func(t *testing.T, p *Plan) {
			if p.Mode != "rps" || p.Steps[0].Method != "POST" || len(p.Steps[0].Checks) != 2 || p.Steps[0].Checks[1].Value != 300 {
				t.Fatalf("%+v", p.Steps[0])
			}
			if len(p.Thresholds) != 2 || p.Thresholds[0].Name != "http_req_failed rate < 5%" || p.Gate.AbortErrorRate != 0.2 {
				t.Fatalf("%+v %+v", p.Thresholds, p.Gate)
			}
			if p.PeakVUs() != 10 { // ceil(50 × 0.2)
				t.Fatalf("peak vus %d", p.PeakVUs())
			}
		}},
		{"no weights → sequential flow", `{"target":"http://a","stages":[{"duration":5,"target":1}],"steps":["GET /a","GET /b"]}`, false, func(t *testing.T, p *Plan) {
			if !p.Sequential || p.Steps[1].Weight != 0.5 {
				t.Fatalf("%+v", p)
			}
		}},
		{"script mode skips steps", `{"script":"export default function(){}","stages":[{"duration":5,"target":1}]}`, false, nil},
		{"missing target", `{"stages":[{"duration":5,"target":1}],"steps":["GET /a"]}`, true, nil},
		{"absolute step url", `{"target":"http://a","stages":[{"duration":5,"target":1}],"steps":["GET http://evil.com/x"]}`, true, nil},
		{"zero peak", `{"target":"http://a","stages":[{"duration":5,"target":0}],"steps":["GET /a"]}`, true, nil},
		{"bad threshold", `{"target":"http://a","stages":[{"duration":5,"target":1}],"steps":["GET /a"],"thresholds":["iterations > 3"]}`, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse([]byte(tc.spec))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

func TestIntegral(t *testing.T) {
	st := []Stage{{Duration: 10 * time.Second, Target: 10}, {Duration: 10 * time.Second, Target: 10}}
	if v := Integral(st, 20*time.Second); v != 150 {
		t.Fatalf("integral = %v", v)
	}
	if v := Integral(st, 5*time.Second); v != 12.5 {
		t.Fatalf("integral(5) = %v", v)
	}
}

func TestThresholdParse(t *testing.T) {
	tests := []struct {
		in     string
		metric string
		limit  float64
	}{
		{"http_req_failed rate < 1%", "error_rate", 0.01},
		{"http_req_failed rate < 0.02", "error_rate", 0.02},
		{"http_req_duration p(95) < 500 ms", "p95", 500},
		{"http_req_duration p(99) < 1.5s", "p99", 1500},
		{"http_req_duration avg < 100", "avg", 100},
	}
	for _, tc := range tests {
		th, err := ParseThreshold(tc.in)
		if err != nil || th.Metric != tc.metric || math.Abs(th.Limit-tc.limit) > 1e-9 {
			t.Errorf("%s → %+v %v", tc.in, th, err)
		}
	}
}
