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

	srv := New(apiclient.New(api.URL, "tt_api_test_key", 2*time.Second),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "v0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	// ---- tools/list: exactly the 12 approved names ----
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listed.Tools) != ToolCount {
		t.Fatalf("got %d tools, want %d", len(listed.Tools), ToolCount)
	}
	want := []string{
		"tt_endpoint_rca", "tt_get_incident", "tt_get_service_kpis", "tt_get_trace",
		"tt_list_deploys", "tt_list_endpoints", "tt_list_incidents", "tt_list_loadtest_runs",
		"tt_list_monitors", "tt_list_services", "tt_search_logs", "tt_search_traces",
	}
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
