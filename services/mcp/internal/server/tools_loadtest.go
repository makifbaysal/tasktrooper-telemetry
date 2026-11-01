package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// registerLoadtestTools adds load-test inspection (read-only). Creating, starting and stopping runs are write tools
// (registerLoadtestWriteTools, ADR-032).
func (s *Server) registerLoadtestTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_loadtest_runs",
		Description: "Load test runs with state, thresholds and summary; use to check a scenario's latest result.",
	}, s.listLoadtestRuns)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "tt_list_loadtest_scenarios",
		Description: "Load test scenarios (target, steps, peak VUs, duration, state draft | ready) and the target domains with their verification state. " +
			"A run only reaches a verified domain: check here before proposing a load test.",
	}, s.listLoadtestScenarios)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_get_loadtest_run",
		Description: "ONE load test run: state, progress, live VUs / RPS / p95 / error rate while running, and the final per-step summary (p50/p95/p99, errors, status codes) and thresholds.",
	}, s.getLoadtestRun)
}

type loadtestListInput struct {
	RangeInput
	ScenarioID string `json:"scenarioId,omitempty" jsonschema:"Scenario id filter, to restrict runs to one scenario"`
	Limit      *int   `json:"limit,omitempty" jsonschema:"Maximum runs to return (default 20, max 100)"`
}

func (s *Server) listLoadtestRuns(ctx context.Context, _ *mcp.CallToolRequest, in loadtestListInput) (*mcp.CallToolResult, any, error) {
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "scenarioId", in.ScenarioID)
	q.Set("limit", itoa(apiclient.LimitValue(in.Limit, defaultIncidentLimit, maxIncidentLimit)))

	var doc any
	if err := s.api.Get(ctx, "/api/v1/loadtests/runs", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with scenarioId or a shorter range")
}

func (s *Server) listLoadtestScenarios(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
	var scenarios []map[string]any
	if err := s.api.Get(ctx, "/api/v1/loadtests/scenarios", nil, &scenarios); err != nil {
		return nil, nil, err
	}
	for _, sc := range scenarios {
		delete(sc, "spec") // long; tt_create_loadtest_scenario documents the shape
	}
	var domains []map[string]any
	if err := s.api.Get(ctx, "/api/v1/loadtests/domains", nil, &domains); err != nil {
		return nil, nil, err
	}
	doms := make([]map[string]any, 0, len(domains))
	for _, d := range domains {
		doms = append(doms, map[string]any{"host": d["host"], "verified": d["verified"], "dnsRecord": d["dnsRecord"], "httpUrl": d["httpUrl"]})
	}
	return shaped(map[string]any{
		"scenarios": scenarios, "domains": doms,
		"note": "targets must be a verified domain (or its subdomain when verified by DNS); adding and verifying a domain is done by the user in Load tests → Domains",
	}, nil, "")
}

type runGetInput struct {
	ID string `json:"id" jsonschema:"Run id from tt_run_loadtest or tt_list_loadtest_runs"`
}

func (s *Server) getLoadtestRun(ctx context.Context, _ *mcp.CallToolRequest, in runGetInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, toolErrf("id is required")
	}
	var doc any
	if err := s.api.Get(ctx, "/api/v1/loadtests/runs/"+url.PathEscape(id), nil, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, nil, "")
}

// ---- writes (TT_MCP_ALLOW_WRITE) ----

func (s *Server) registerLoadtestWriteTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "tt_create_loadtest_scenario",
		Description: "Create a load test scenario (source agent, state ready). WRITE: dry run unless confirm: true. " +
			"Use it when an endpoint has too little traffic to pick alert thresholds: propose it, and only create after the user agreed to the target, the load and the duration.",
	}, s.createScenario)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "tt_run_loadtest",
		Description: "Start a run of a scenario: it sends REAL traffic to the target. WRITE: dry run unless confirm: true; confirm only after the user approved running it now. " +
			"Poll tt_get_loadtest_run (every 15-30 s) until state is passed, failed, aborted or error.",
	}, s.runLoadtest)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_stop_loadtest",
		Description: "Abort a queued or running load test run. Takes effect within seconds; no confirm needed, it only reduces load.",
	}, s.stopLoadtest)
}

type scenarioInput struct {
	confirmField
	Name string         `json:"name" jsonschema:"e.g. \"checkout baseline (agent)\""`
	Spec map[string]any `json:"spec" jsonschema:"Scenario spec (.ai/load-testing.md § Senaryo formatı). Minimal: {target: \"https://api.example.com\", mode: \"vus\", levels: [{name: \"warm\", ramp: 30, hold: 60, target: 5}, {name: \"steady\", ramp: 30, hold: 300, target: 20}], steps: [\"GET /v1/products\", \"POST /v1/orders\"], weights: [80, 20], requests: {\"POST /v1/orders\": {body: \"{\\\"sku\\\": \\\"{{int 1-500}}\\\"}\", checks: [\"status == 201\"]}}, thinkTime: 1, thresholds: [\"http_req_failed rate < 1%\", \"http_req_duration p(95) < 500 ms\"], gate: {abortErrorRate: 0.2, abortP95Ms: 5000}}. Ramps and holds are seconds"`
}

func (s *Server) createScenario(ctx context.Context, _ *mcp.CallToolRequest, in scenarioInput) (*mcp.CallToolResult, any, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, nil, toolErrf("name is required")
	}
	target, _ := in.Spec["target"].(string)
	if u, err := url.Parse(target); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, toolErrf("spec.target must be the base URL, e.g. https://api.example.com (a verified domain from tt_list_loadtest_scenarios)")
	}
	if _, ok := in.Spec["script"]; !ok {
		if steps, _ := in.Spec["steps"].([]any); len(steps) == 0 {
			return nil, nil, toolErrf("spec.steps is required: [\"METHOD /route\", ...]")
		}
		_, levels := in.Spec["levels"]
		_, stages := in.Spec["stages"]
		if !levels && !stages {
			return nil, nil, toolErrf("spec.levels is required: [{name, ramp, hold, target}] (seconds, VUs or RPS)")
		}
	}
	if _, ok := in.Spec["gate"]; !ok { // stop on its own when the target is clearly falling over
		in.Spec["gate"] = map[string]any{"abortErrorRate": 0.5, "abortP95Ms": 10000}
	}
	body := map[string]any{"name": name, "source": agentSource, "state": "ready", "spec": in.Spec}
	if !in.Confirm {
		return dryRun(http.MethodPost, "/api/v1/loadtests/scenarios", body)
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPost, "/api/v1/loadtests/scenarios", nil, body, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"created": doc}, nil, "")
}

type runInput struct {
	confirmField
	ScenarioID string `json:"scenarioId" jsonschema:"Scenario id from tt_list_loadtest_scenarios or tt_create_loadtest_scenario"`
}

func (s *Server) runLoadtest(ctx context.Context, _ *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ScenarioID)
	if id == "" {
		return nil, nil, toolErrf("scenarioId is required")
	}
	path := "/api/v1/loadtests/scenarios/" + url.PathEscape(id)
	var sc map[string]any
	if err := s.api.Get(ctx, path, nil, &sc); err != nil {
		return nil, nil, err
	}
	if state, _ := sc["state"].(string); state == "draft" {
		return nil, nil, toolErrf("scenario %s is a draft: the user has to review it in Load tests first", id)
	}
	if !in.Confirm {
		return shaped(map[string]any{
			"dryRun": true, "wouldSend": map[string]any{"method": http.MethodPost, "path": path + "/runs"},
			"scenario": map[string]any{"name": sc["name"], "target": sc["target"], "peakVus": sc["peakVus"], "durationS": sc["duration"], "steps": sc["steps"]},
			"next":     "tell the user the target, peak load and duration; call again with confirm: true once they approve",
		}, nil, "")
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPost, path+"/runs", nil, nil, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"started": doc, "next": "poll tt_get_loadtest_run with the run id; tt_stop_loadtest aborts"}, nil, "")
}

func (s *Server) stopLoadtest(ctx context.Context, _ *mcp.CallToolRequest, in runGetInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, toolErrf("id is required")
	}
	var doc any
	if err := s.api.Send(ctx, http.MethodPost, "/api/v1/loadtests/runs/"+url.PathEscape(id)+"/stop", nil, nil, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(map[string]any{"stopped": doc}, nil, "")
}
