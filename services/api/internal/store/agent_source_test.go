package store

import (
	"encoding/json"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// Monitors, KPIs and load test scenarios accept source "agent" (MCP write tools, ADR-032).
func TestAgentSource(t *testing.T) {
	tests := []struct {
		name    string
		check   func(source string) error
		source  string
		wantErr bool
	}{
		{"monitor agent", monitorWithSource, "agent", false},
		{"monitor manual", monitorWithSource, "manual", false},
		{"monitor code refused", monitorWithSource, "code", true},
		{"monitor unknown", monitorWithSource, "robot", true},
		{"kpi agent", kpiWithSource, "agent", false},
		{"kpi unknown", kpiWithSource, "robot", true},
		{"scenario agent", scenarioWithSource, "agent", false},
		{"scenario generated", scenarioWithSource, "generated", false},
		{"scenario unknown", scenarioWithSource, "robot", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check(tc.source)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func monitorWithSource(source string) error {
	in := model.MonitorInput{Name: "p95", Kind: "latency", Severity: "warning", Source: source,
		Target: model.MonitorTarget{Service: "checkout"}, Query: json.RawMessage(`{"percentile":"p95"}`),
		Condition: model.MonitorCondition{Op: ">", Threshold: 800}}
	_, _, _, got, err := normalizeMonitor(&in)
	if err == nil && got != source {
		return Invalid("source " + got)
	}
	return err
}

func kpiWithSource(source string) error {
	in := model.KpiInput{Name: "Checkout", Sli: "availability", Objective: 0.99, Window: "28d", Source: source,
		Target: model.KpiTarget{Kind: "endpoint", ServiceID: "checkout", Ref: "POST /orders"}}
	return validateKpi(&in)
}

func scenarioWithSource(source string) error {
	in := model.ScenarioInput{Name: "s", Source: source, Spec: json.RawMessage(`{"target":"https://x.example"}`)}
	return validateScenario(&in)
}
