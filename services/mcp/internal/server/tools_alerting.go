package server

import (
	"context"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
	"github.com/tasktrooper/telemetry/services/mcp/internal/redact"
)

// registerAlertingTools adds the alert/incident read tools.
func (s *Server) registerAlertingTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_monitors",
		Description: "Alert rules (monitors) with kind, query and state; use to find what is alerting or what is configured for a service.",
	}, s.listMonitors)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_incidents",
		Description: "Open or recent incidents with severity and current value; start here for 'what is broken right now'.",
	}, s.listIncidents)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_get_incident",
		Description: "Everything about ONE incident: alert detail, root cause and timeline. rootCause is unstructured and its shape may vary; treat it as opaque JSON.",
	}, s.getIncident)
}

type monitorListInput struct {
	RangeInput
	Service   string `json:"service,omitempty" jsonschema:"Service name, as returned by tt_list_services"`
	Endpoint  string `json:"endpoint,omitempty" jsonschema:"Endpoint as \"METHOD /route\" (e.g. \"POST /v1/charges\", method + route from tt_list_endpoints) or a consumer process name; NOT the slug id. A slug matches nothing and returns an empty list"`
	Kind      string `json:"kind,omitempty" jsonschema:"Monitor kind filter (threshold, latency, log_match, error_new, anomaly, composite, kpi_burn, ...)"`
	ClusterID string `json:"clusterId,omitempty" jsonschema:"Infra cluster id filter"`
	Group     string `json:"group,omitempty" jsonschema:"Consumer group filter"`
}

func (s *Server) listMonitors(ctx context.Context, _ *mcp.CallToolRequest, in monitorListInput) (*mcp.CallToolResult, any, error) {
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "endpoint", in.Endpoint)
	apiclient.PutIf(q, "kind", in.Kind)
	apiclient.PutIf(q, "clusterId", in.ClusterID)
	apiclient.PutIf(q, "group", in.Group)

	var doc any
	if err := s.api.Get(ctx, "/api/v1/monitors", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with service, endpoint or kind")
}

const (
	defaultIncidentLimit = 20
	maxIncidentLimit     = 100
)

type incidentListInput struct {
	RangeInput
	State     string `json:"state,omitempty" jsonschema:"Incident state: firing, resolved or acked"`
	Service   string `json:"service,omitempty" jsonschema:"Service name, as returned by tt_list_services"`
	Endpoint  string `json:"endpoint,omitempty" jsonschema:"Endpoint as \"METHOD /route\" (e.g. \"POST /v1/charges\", method + route from tt_list_endpoints) or a consumer process name; NOT the slug id. A slug matches nothing and returns an empty list"`
	MonitorID string `json:"monitorId,omitempty" jsonschema:"Monitor id, from tt_list_monitors"`
	KpiID     string `json:"kpiId,omitempty" jsonschema:"KPI id filter"`
	Since     *int   `json:"since,omitempty" jsonschema:"Only incidents started at or after this ts (ms)"`
	Limit     *int   `json:"limit,omitempty" jsonschema:"Maximum incidents to return (default 20, max 100)"`
}

func (s *Server) listIncidents(ctx context.Context, _ *mcp.CallToolRequest, in incidentListInput) (*mcp.CallToolResult, any, error) {
	if in.State != "" && !validStates[in.State] {
		return nil, nil, toolErrf("state must be one of firing, resolved, acked")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "state", in.State)
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "endpoint", in.Endpoint)
	apiclient.PutIf(q, "monitorId", in.MonitorID)
	apiclient.PutIf(q, "kpiId", in.KpiID)
	apiclient.PutInt(q, "since", in.Since)
	q.Set("limit", itoa(apiclient.LimitValue(in.Limit, defaultIncidentLimit, maxIncidentLimit)))

	var doc any
	if err := s.api.Get(ctx, "/api/v1/incidents", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with state, service or a shorter since")
}

type incidentGetInput struct {
	ID string `json:"id" jsonschema:"Incident id, from tt_list_incidents"`
}

func (s *Server) getIncident(ctx context.Context, _ *mcp.CallToolRequest, in incidentGetInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, toolErrf("id is required")
	}
	// Two upstream calls, merged into one result (ADR-025, contracts §26).
	var incident any
	if err := s.api.Get(ctx, "/api/v1/incidents/"+url.PathEscape(id), nil, &incident); err != nil {
		return nil, nil, err
	}
	var events any
	if err := s.api.Get(ctx, "/api/v1/incidents/"+url.PathEscape(id)+"/events", nil, &events); err != nil {
		return nil, nil, err
	}
	// rootCause is opaque; it is passed through unchanged, but events may carry
	// free-text detail worth masking.
	redactEvents(events)
	merged := map[string]any{"incident": incident, "events": events}
	return shaped(merged, []string{"events"}, "the incident timeline is long; focus on one state or service")
}

// redactEvents masks PII in an incident timeline's data payloads.
func redactEvents(doc any) {
	items, ok := doc.([]any)
	if !ok {
		return
	}
	for _, it := range items {
		ev, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if data, ok := ev["data"].(map[string]any); ok {
			for k, v := range data {
				if s, ok := v.(string); ok {
					data[k] = redact.String(s)
				}
			}
		}
	}
}

var validStates = map[string]bool{"firing": true, "resolved": true, "acked": true}
