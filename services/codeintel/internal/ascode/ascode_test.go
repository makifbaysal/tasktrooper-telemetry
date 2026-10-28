package ascode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const checkout = `name: Checkout error rate
kind: metric_threshold
query:
  source: red
  service: orders-api
  route: POST /checkout
  metric: error_rate
condition:
  op: '>'
  threshold: 0.02
  for: 5m
  min_requests: 50
severity: critical
notify: [slack:#payments-alerts, pagerduty:payments]
labels: { team: payments }
`

func TestTranslateSchemaExample(t *testing.T) {
	ms, err := Parse(".trooper/monitors/checkout.yaml", []byte(checkout))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatal(len(ms))
	}
	m := ms[0]
	if m.Name != "Checkout error rate" || m.Kind != "metric_threshold" || m.Severity != "critical" || m.Status != "active" {
		t.Fatalf("%+v", m)
	}
	if m.Target["service"] != "orders-api" || m.Target["endpoint"] != "POST /checkout" {
		t.Fatalf("target %v", m.Target)
	}
	if m.Query["metric"] != "error_rate" || m.Query["source"] != nil || m.Query["service"] != nil {
		t.Fatalf("query %v", m.Query)
	}
	if m.Condition["op"] != ">" || m.Condition["threshold"] != 0.02 || m.Condition["forMin"] != 5.0 || m.Condition["minRequests"] != 50.0 {
		t.Fatalf("condition %v", m.Condition)
	}
	if len(m.Channels) != 2 || m.Channels[0] != "slack:#payments-alerts" {
		t.Fatalf("channels %v", m.Channels)
	}
}

func TestParseShapesAndErrors(t *testing.T) {
	multi := `- name: A
  kind: latency
  query: { service: s, route: GET /a, percentile: p95 }
  condition: { threshold: 400, for: 10m }
- name: B
  kind: consumer_lag
  query: { service: s, clusterId: kafka-main, group: g, metric: time_behind, threshold: 300 }
  condition: { for: 5m }
  enabled: false
`
	ms, err := Parse("x.yaml", []byte(multi))
	if err != nil || len(ms) != 2 {
		t.Fatal(err, len(ms))
	}
	if ms[0].Severity != "warning" || ms[0].Condition["forMin"] != 10.0 {
		t.Fatalf("A %+v", ms[0])
	}
	b := ms[1]
	if b.Status != "draft" || b.Query["clusterId"] != "kafka-main" || b.Query["forMin"] != 5.0 || b.Condition["threshold"] != 300.0 {
		t.Fatalf("B %+v", b)
	}
	docs := "name: one\nkind: error_new\n---\nmonitors:\n  - name: two\n    kind: job_missed\n    query: { process: nightly, intervalMin: 1440 }\n"
	if ms, err := Parse("d.yaml", []byte(docs)); err != nil || len(ms) != 2 || ms[1].Target["process"] != "nightly" {
		t.Fatal(err, ms)
	}
	bad := map[string]string{
		"unknown kind": "name: x\nkind: nope\n",
		"no name":      "kind: latency\n",
		"bad severity": "name: x\nkind: latency\nseverity: page\n",
		"bad lag":      "name: x\nkind: consumer_lag\nquery: { group: g }\n",
		"duplicate":    "- {name: x, kind: latency}\n- {name: x, kind: latency}\n",
		"invalid yaml": "name: [x\n",
		"bad op":       "name: x\nkind: latency\ncondition: { op: '!=' }\n",
	}
	for name, src := range bad {
		if _, err := Parse("b.yaml", []byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, Dir)
	_ = os.MkdirAll(filepath.Join(dir, "team"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "checkout.yaml"), []byte(checkout), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "team", "b.yml"), []byte("name: B\nkind: error_new\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("name: x\nkind: nope\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# docs"), 0o644)
	ms, broken := Load(root)
	if len(ms) != 2 || ms[0].Path != ".trooper/monitors/checkout.yaml" || ms[1].Path != ".trooper/monitors/team/b.yml" {
		t.Fatalf("%+v", ms)
	}
	if len(broken) != 1 || broken[0].Path != ".trooper/monitors/broken.yaml" || !strings.Contains(broken[0].Error(), "unknown kind") {
		t.Fatalf("broken %v", broken)
	}
}
