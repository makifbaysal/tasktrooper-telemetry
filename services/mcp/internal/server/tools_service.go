package server

import (
	"context"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// registerServiceTools adds the service/endpoint read tools (ADR-025): discovery,
// one service's KPIs, its endpoints, and one endpoint's root-cause baseline.
func (s *Server) registerServiceTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_services",
		Description: "List services in the project with throughput, error rate and latency summary; start here to discover service names.",
	}, s.listServices)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_get_service_kpis",
		Description: "Health, RED metrics, dependencies and version info of ONE service; use after tt_list_services, not for discovery.",
	}, s.getServiceKpis)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_endpoints",
		Description: "Endpoints (routes, consumers) of one service with error rate and p95; returns the endpoint ids other tools need.",
	}, s.listEndpoints)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_endpoint_rca",
		Description: "Explain WHY one endpoint got slower or errorier: per-dependency delta vs a baseline window (root-cause attribution). Requires an endpoint id from tt_list_endpoints.",
	}, s.endpointRCA)
}

type serviceListInput struct {
	RangeInput
	Type string `json:"type,omitempty" jsonschema:"Service types, comma separated (http | worker | web | mobile)"`
}

// validServiceTypes is the `type` enum (mcp.md rule 2); comma separated.
var validServiceTypes = map[string]bool{"http": true, "worker": true, "web": true, "mobile": true}

func (s *Server) listServices(ctx context.Context, _ *mcp.CallToolRequest, in serviceListInput) (*mcp.CallToolResult, any, error) {
	if in.Type != "" {
		for _, t := range strings.Split(in.Type, ",") {
			if !validServiceTypes[strings.TrimSpace(t)] {
				return nil, nil, toolErrf("type must be one of http, worker, web, mobile (got %q)", strings.TrimSpace(t))
			}
		}
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "type", in.Type)
	var doc any
	if err := s.api.Get(ctx, "/api/v1/services", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with range or env, or filter by type")
}

func (s *Server) getServiceKpis(ctx context.Context, _ *mcp.CallToolRequest, in serviceDetailInput) (*mcp.CallToolResult, any, error) {
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	var doc any
	if err := s.api.Get(ctx, "/api/v1/services/"+url.PathEscape(in.Service), q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, nil, "")
}

type endpointListInput struct {
	RangeInput
	Service string `json:"service" jsonschema:"Service name, as returned by tt_list_services"`
}

func (s *Server) listEndpoints(ctx context.Context, _ *mcp.CallToolRequest, in endpointListInput) (*mcp.CallToolResult, any, error) {
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	path := "/api/v1/services/" + url.PathEscape(in.Service) + "/endpoints"
	var doc any
	if err := s.api.Get(ctx, path, q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with range or env")
}

type rcaInput struct {
	RangeInput
	Service  string `json:"service" jsonschema:"Service name, as returned by tt_list_services"`
	Endpoint string `json:"endpoint" jsonschema:"Endpoint id (slug) from tt_list_endpoints, not the route"`
	Baseline string `json:"baseline,omitempty" jsonschema:"Comparison window: prev (default), 1d or 7d"`
}

func (s *Server) endpointRCA(ctx context.Context, _ *mcp.CallToolRequest, in rcaInput) (*mcp.CallToolResult, any, error) {
	if in.Baseline != "" && in.Baseline != "prev" && in.Baseline != "1d" && in.Baseline != "7d" {
		return nil, nil, toolErrf("baseline must be one of prev, 1d, 7d")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "baseline", in.Baseline)
	path := "/api/v1/services/" + url.PathEscape(in.Service) + "/endpoints/" + url.PathEscape(in.Endpoint) + "/rca"
	var doc any
	if err := s.api.Get(ctx, path, q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, nil, "")
}
