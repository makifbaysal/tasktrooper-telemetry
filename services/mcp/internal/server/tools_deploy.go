package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// registerDeployTools adds deploy-marker inspection, used to correlate a
// regression with a release.
func (s *Server) registerDeployTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_deploys",
		Description: "Deploy markers (newest versions first seen, Vercel/CI markers); use to correlate an incident or latency change with a deployment.",
	}, s.listDeploys)
}

type deployListInput struct {
	RangeInput
	Service string `json:"service,omitempty" jsonschema:"Service name, as returned by tt_list_services"`
	Source  string `json:"source,omitempty" jsonschema:"Deploy source filter (e.g. spans, vercel, ci)"`
	Limit   *int   `json:"limit,omitempty" jsonschema:"Maximum markers to return (default 20, max 100)"`
}

func (s *Server) listDeploys(ctx context.Context, _ *mcp.CallToolRequest, in deployListInput) (*mcp.CallToolResult, any, error) {
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "source", in.Source)
	q.Set("limit", itoa(apiclient.LimitValue(in.Limit, defaultIncidentLimit, maxIncidentLimit)))

	var doc any
	if err := s.api.Get(ctx, "/api/v1/deploys", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with service, source or a shorter range")
}
