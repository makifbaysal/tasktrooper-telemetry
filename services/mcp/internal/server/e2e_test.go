package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// TestE2E drives the real server through a real MCP client over the SDK's
// in-memory transports: list the tool surface, then three representative calls
// (list services, search logs with truncation, get incident with its two
// upstream calls) against a fake services/api.
func TestE2E(t *testing.T) {
	var mu sync.Mutex
	var paths []string

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/services":
			_, _ = io.WriteString(w, `[
				{"name":"checkout","type":"http","throughput":120.5,"errorRate":0.021,"p95Ms":312},
				{"name":"payments-worker","type":"worker","throughput":44.1,"errorRate":0.0,"p95Ms":88}]`)
		case r.URL.Path == "/api/v1/logs":
			// 60 lines × ~1.5 KB ≈ 90 KB: over the 64 KB cap, so truncation kicks in.
			line := map[string]any{
				"id": "l1", "ts": 1730000000000, "level": "error", "service": "checkout",
				"message": "payment failed for alice@example.com — " + strings.Repeat("x", 1500),
			}
			b, _ := json.Marshal(line)
			var sb strings.Builder
			sb.WriteString(`{"lines":[`)
			for i := 0; i < 60; i++ {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.Write(b)
			}
			sb.WriteString(`],"nextBefore":1730000000000}`)
			_, _ = io.WriteString(w, sb.String())
		case r.URL.Path == "/api/v1/incidents/42":
			_, _ = io.WriteString(w, `{"id":"42","monitorId":"m1","title":"checkout error rate high",
				"severity":"critical","state":"firing","startedAt":1730000000000,"value":"2.1%","service":"checkout",
				"rootCause":{"note":"unstructured, shape may vary"}}`)
		case r.URL.Path == "/api/v1/incidents/42/events":
			_, _ = io.WriteString(w, `[{"id":"e1","ts":1730000000001,"kind":"fired","data":{"note":"opened"}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	ctx := context.Background()
	clientSession := connect(t, api.URL, Options{})

	// ---- tools/list: exactly the read-only names ----
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listed.Tools) != ToolCount(false) {
		t.Fatalf("got %d tools, want %d", len(listed.Tools), ToolCount(false))
	}
	want := append([]string(nil), readTools...)
	sort.Strings(want)
	got := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		got = append(got, tool.Name)
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tool names:\n got %v\nwant %v", got, want)
	}

	// ---- tt_list_services ----
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "tt_list_services",
		Arguments: map[string]any{"range": "1h"},
	})
	if err != nil {
		t.Fatalf("call tt_list_services: %v", err)
	}
	if res.IsError {
		t.Fatalf("tt_list_services returned error: %s", resultText(t, res))
	}
	if text := resultText(t, res); !strings.Contains(text, "checkout") {
		t.Errorf("services result missing checkout: %s", text)
	}

	// ---- tt_search_logs: PII masked and the 64 KB cap enforced ----
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "tt_search_logs",
		Arguments: map[string]any{"service": "checkout", "level": "error"},
	})
	if err != nil {
		t.Fatalf("call tt_search_logs: %v", err)
	}
	if res.IsError {
		t.Fatalf("tt_search_logs returned error: %s", resultText(t, res))
	}
	text := resultText(t, res)
	if len(text) > apiclient.MaxResultBytes {
		t.Errorf("log result %d bytes exceeds the %d cap", len(text), apiclient.MaxResultBytes)
	}
	if strings.Contains(text, "alice@example.com") {
		t.Errorf("PII leaked into log result")
	}
	var page struct {
		Truncated bool `json:"truncated"`
		Total     int  `json:"total"`
		Hint      string
	}
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatalf("log result is not JSON: %v", err)
	}
	if !page.Truncated || page.Total != 60 || page.Hint == "" {
		t.Errorf("truncation markers wrong: truncated=%v total=%d hint=%q", page.Truncated, page.Total, page.Hint)
	}

	// ---- tt_get_incident: two upstream calls merged ----
	mu.Lock()
	paths = nil
	mu.Unlock()
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "tt_get_incident",
		Arguments: map[string]any{"id": "42"},
	})
	if err != nil {
		t.Fatalf("call tt_get_incident: %v", err)
	}
	if res.IsError {
		t.Fatalf("tt_get_incident returned error: %s", resultText(t, res))
	}
	var merged struct {
		Incident map[string]any `json:"incident"`
		Events   []any          `json:"events"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &merged); err != nil {
		t.Fatalf("incident result is not JSON: %v", err)
	}
	if merged.Incident["id"] != "42" {
		t.Errorf("incident not merged: %v", merged.Incident)
	}
	if len(merged.Events) != 1 {
		t.Errorf("events not merged: %v", merged.Events)
	}
	mu.Lock()
	seen := append([]string(nil), paths...)
	mu.Unlock()
	if len(seen) != 2 || seen[0] != "/api/v1/incidents/42" || seen[1] != "/api/v1/incidents/42/events" {
		t.Errorf("tt_get_incident upstream calls = %v, want the incident and its events", seen)
	}
}

// readTools / writeTools are the approved tool surface (.ai/mcp.md § Tool yüzeyi).
var (
	readTools = []string{
		"tt_endpoint_rca", "tt_get_incident", "tt_get_service_kpis", "tt_get_trace",
		"tt_list_deploys", "tt_list_endpoints", "tt_list_incidents", "tt_list_loadtest_runs",
		"tt_list_monitors", "tt_list_services", "tt_search_logs", "tt_search_traces",
		"tt_list_errors", "tt_get_error", "tt_list_kpis", "tt_list_alert_suggestions", "tt_list_inventory",
		"tt_list_monitor_templates", "tt_list_notify_channels", "tt_get_app_summary", "tt_get_ai_overview",
		"tt_list_hosts", "tt_list_databases", "tt_list_consumer_groups", "tt_preview_monitor",
		"tt_list_loadtest_scenarios", "tt_get_loadtest_run",
	}
	writeTools = []string{
		"tt_create_monitor", "tt_update_monitor", "tt_delete_monitor", "tt_accept_suggestion",
		"tt_create_kpi", "tt_update_kpi", "tt_delete_kpi", "tt_send_slack_test",
		"tt_create_loadtest_scenario", "tt_run_loadtest", "tt_stop_loadtest",
	}
)

// connect starts the server against apiURL and returns a connected MCP client session.
func connect(t *testing.T, apiURL string, opts Options) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := New(apiclient.New(apiURL, "tt_api_test_key", 2*time.Second), slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestE2EWrite covers the write surface (ADR-032): registered only with AllowWrite, dry run without confirm,
// source forced to agent, delete limited to agent-created monitors.
func TestE2EWrite(t *testing.T) {
	type call struct {
		method, path string
		body         map[string]any
	}
	var mu sync.Mutex
	var calls []call
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/monitors":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"m9","name":"x","source":"agent"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/monitors/m1":
			_, _ = io.WriteString(w, `{"id":"m1","source":"manual"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/monitors/m9":
			_, _ = io.WriteString(w, `{"id":"m9","source":"agent"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/monitors/m9":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loadtests/scenarios/s1":
			_, _ = io.WriteString(w, `{"id":"s1","name":"checkout","target":"https://api.example.com","state":"ready","peakVus":20,"duration":420}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loadtests/scenarios/s2":
			_, _ = io.WriteString(w, `{"id":"s2","state":"draft"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/loadtests/scenarios/s1/runs":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"r7","state":"queued"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/loadtests/scenarios":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"s3"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/kpis":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"objective out of range"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	ctx := context.Background()

	// read-only by default
	ro := connect(t, api.URL, Options{})
	if res, _ := ro.CallTool(ctx, &mcp.CallToolParams{Name: "tt_create_monitor", Arguments: map[string]any{}}); res != nil && !res.IsError {
		t.Fatalf("tt_create_monitor callable without AllowWrite")
	}

	cs := connect(t, api.URL, Options{AllowWrite: true})
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := append(append([]string(nil), readTools...), writeTools...)
	sort.Strings(want)
	got := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	if len(got) != ToolCount(true) || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("write tools:\n got %v\nwant %v", got, want)
	}

	monitor := map[string]any{
		"name": "checkout p95", "kind": "latency", "target": map[string]any{"service": "checkout"},
		"query": map[string]any{"percentile": "p95"}, "condition": map[string]any{"op": ">", "threshold": 800, "forMin": 5},
		"severity": "warning", "source": "manual", "channels": []any{},
	}
	tests := []struct {
		name      string
		tool      string
		args      map[string]any
		wantErr   bool
		wantText  string
		wantCalls []string // "METHOD path"
		check     func(t *testing.T, cs []call)
	}{
		{name: "create dry run", tool: "tt_create_monitor", args: map[string]any{"monitor": monitor},
			wantText: `"dryRun":true`},
		{name: "create confirmed", tool: "tt_create_monitor", args: map[string]any{"monitor": monitor, "confirm": true},
			wantText: `"m9"`, wantCalls: []string{"POST /api/v1/monitors"},
			check: func(t *testing.T, cs []call) {
				if cs[0].body["source"] != "agent" {
					t.Errorf("source = %v, want agent", cs[0].body["source"])
				}
				if ch, ok := cs[0].body["channels"].([]any); !ok || len(ch) != 0 {
					t.Errorf("channels = %v, want []", cs[0].body["channels"])
				}
				if cond, _ := cs[0].body["condition"].(map[string]any); cond["window"] != "5m" {
					t.Errorf("condition = %v, want the default 5m window stated", cond)
				}
			}},
		{name: "create invalid severity", tool: "tt_create_monitor", wantErr: true,
			args: map[string]any{"monitor": map[string]any{"name": "x", "kind": "latency", "condition": map[string]any{"op": ">"}, "severity": "high"}}},
		{name: "delete manual refused", tool: "tt_delete_monitor", args: map[string]any{"id": "m1", "confirm": true},
			wantErr: true, wantCalls: []string{"GET /api/v1/monitors/m1"}},
		{name: "delete agent dry run", tool: "tt_delete_monitor", args: map[string]any{"id": "m9"},
			wantText: `"dryRun":true`, wantCalls: []string{"GET /api/v1/monitors/m9"}},
		{name: "delete agent confirmed", tool: "tt_delete_monitor", args: map[string]any{"id": "m9", "confirm": true},
			wantText: `"deleted"`, wantCalls: []string{"GET /api/v1/monitors/m9", "DELETE /api/v1/monitors/m9"}},
		{name: "kpi endpoint ref must be METHOD route", tool: "tt_create_kpi", wantErr: true,
			args: map[string]any{"kpi": map[string]any{"name": "Checkout", "sli": "availability", "objective": 0.99, "window": "28d", "description": "d", "owner": "team", "alerts": []any{},
				"target": map[string]any{"kind": "endpoint", "serviceId": "checkout", "ref": "post-orders"}}}},
		{name: "kpi api error surfaced", tool: "tt_create_kpi", wantErr: true, wantText: "objective out of range",
			wantCalls: []string{"POST /api/v1/kpis"},
			args: map[string]any{"confirm": true, "kpi": map[string]any{"name": "Checkout", "sli": "availability", "objective": 0.99, "window": "28d", "description": "d", "owner": "team", "alerts": []any{},
				"target": map[string]any{"kind": "endpoint", "serviceId": "checkout", "ref": "POST /orders"}}}},
		{name: "scenario needs a URL target", tool: "tt_create_loadtest_scenario", wantErr: true,
			args: map[string]any{"name": "x", "spec": map[string]any{"target": "api.example.com", "steps": []any{"GET /"}, "levels": []any{}}}},
		{name: "scenario confirmed adds gate and source", tool: "tt_create_loadtest_scenario", wantText: `"s3"`,
			wantCalls: []string{"POST /api/v1/loadtests/scenarios"},
			args: map[string]any{"name": "x", "confirm": true, "spec": map[string]any{"target": "https://api.example.com", "steps": []any{"GET /"},
				"levels": []any{map[string]any{"name": "a", "ramp": 10, "hold": 30, "target": 5}}}},
			check: func(t *testing.T, cs []call) {
				spec, _ := cs[0].body["spec"].(map[string]any)
				if cs[0].body["source"] != "agent" || spec["gate"] == nil {
					t.Errorf("body = %v, want source agent and a default gate", cs[0].body)
				}
			}},
		{name: "run dry run sends no traffic", tool: "tt_run_loadtest", args: map[string]any{"scenarioId": "s1"},
			wantText: `"dryRun":true`, wantCalls: []string{"GET /api/v1/loadtests/scenarios/s1"}},
		{name: "run confirmed", tool: "tt_run_loadtest", args: map[string]any{"scenarioId": "s1", "confirm": true},
			wantText: `"r7"`, wantCalls: []string{"GET /api/v1/loadtests/scenarios/s1", "POST /api/v1/loadtests/scenarios/s1/runs"}},
		{name: "draft scenario refused", tool: "tt_run_loadtest", args: map[string]any{"scenarioId": "s2", "confirm": true},
			wantErr: true, wantCalls: []string{"GET /api/v1/loadtests/scenarios/s2"}},
		{name: "slack test dry run strips target", tool: "tt_send_slack_test", args: map[string]any{"channel": "slack:C123#alerts"},
			wantText: `"channel":"C123"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			calls = nil
			mu.Unlock()
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			text := resultText(t, res)
			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v: %s", res.IsError, tc.wantErr, text)
			}
			if tc.wantText != "" && !strings.Contains(text, tc.wantText) {
				t.Errorf("result %s missing %s", text, tc.wantText)
			}
			mu.Lock()
			seen := append([]call(nil), calls...)
			mu.Unlock()
			var names []string
			for _, c := range seen {
				names = append(names, c.method+" "+c.path)
			}
			if strings.Join(names, ",") != strings.Join(tc.wantCalls, ",") {
				t.Errorf("upstream calls = %v, want %v", names, tc.wantCalls)
			}
			if tc.check != nil {
				tc.check(t, seen)
			}
		})
	}
}
