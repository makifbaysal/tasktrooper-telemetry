package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// registerLoadtestTools adds load-test run inspection (read-only: no start/stop).
func (s *Server) registerLoadtestTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_loadtest_runs",
		Description: "Load test runs with state, thresholds and summary; use to check a scenario's latest result, not to start tests.",
	}, s.listLoadtestRuns)
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
