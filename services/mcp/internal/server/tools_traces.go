package server

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
	"github.com/tasktrooper/telemetry/services/mcp/internal/redact"
)

func itoa(n int) string { return strconv.Itoa(n) }

// hexID matches a 32-hex-char trace id (the only id format the trace tools accept).
var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// registerTraceTools adds trace discovery and single-trace inspection.
func (s *Server) registerTraceTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_search_traces",
		Description: "Find recent traces by service/endpoint/duration/error; returns trace ids for tt_get_trace.",
	}, s.searchTraces)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_get_trace",
		Description: "Full span tree of ONE trace (waterfall, attributes, dependencies). Use a trace id from tt_search_traces or an error issue.",
	}, s.getTrace)
}

const (
	defaultTraceLimit = 20
	maxTraceLimit     = 100
)

type traceSearchInput struct {
	RangeInput
	Service    string `json:"service,omitempty" jsonschema:"Service name, as returned by tt_list_services"`
	Endpoint   string `json:"endpoint,omitempty" jsonschema:"Endpoint id (slug) from tt_list_endpoints, not the route"`
	ErrorsOnly bool   `json:"errorsOnly,omitempty" jsonschema:"Only traces containing an error span"`
	MinMs      *int   `json:"minMs,omitempty" jsonschema:"Only traces at least this slow, in milliseconds"`
	Sort       string `json:"sort,omitempty" jsonschema:"slowest to sort by duration instead of recency"`
	Limit      *int   `json:"limit,omitempty" jsonschema:"Maximum traces to return (default 20, max 100)"`
}

func (s *Server) searchTraces(ctx context.Context, _ *mcp.CallToolRequest, in traceSearchInput) (*mcp.CallToolResult, any, error) {
	if in.Sort != "" && in.Sort != "slowest" {
		return nil, nil, toolErrf("sort must be empty or 'slowest'")
	}
	if in.MinMs != nil && *in.MinMs < 0 {
		return nil, nil, toolErrf("minMs must be >= 0")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "endpoint", in.Endpoint)
	apiclient.PutBool(q, "errors", in.ErrorsOnly)
	apiclient.PutInt(q, "minMs", in.MinMs)
	if in.Sort != "" {
		q.Set("sort", in.Sort)
	}
	q.Set("limit", itoa(apiclient.LimitValue(in.Limit, defaultTraceLimit, maxTraceLimit)))

	var doc any
	if err := s.api.Get(ctx, "/api/v1/traces", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with service, endpoint, errorsOnly or a higher minMs")
}

type traceGetInput struct {
	TraceID string `json:"traceId" jsonschema:"32 hex chars, from tt_search_traces"`
}

func (s *Server) getTrace(ctx context.Context, _ *mcp.CallToolRequest, in traceGetInput) (*mcp.CallToolResult, any, error) {
	id := strings.ToLower(strings.TrimSpace(in.TraceID))
	if !hexID.MatchString(id) {
		return nil, nil, toolErrf("traceId must be 32 hex chars (got %d chars)", len(id))
	}
	var doc any
	if err := s.api.Get(ctx, "/api/v1/traces/"+url.PathEscape(id), nil, &doc); err != nil {
		return nil, nil, err
	}
	// Redact span attributes and events before they reach the model.
	if obj, ok := doc.(map[string]any); ok {
		if spans, ok := obj["spans"].([]any); ok {
			for _, sp := range spans {
				redactSpan(sp)
			}
		}
	}
	// Spans are capped separately from the byte cap so a deep trace stays readable.
	out, err := apiclient.TraceSpans(doc, apiclient.MaxTraceSpans,
		"this trace has more spans than shown; inspect a narrower service or endpoint instead")
	if err != nil {
		return nil, nil, toolErrf("could not shape result")
	}
	return shaped(out, nil, "")
}

// redactSpan masks PII in a span's attrs, resource attrs and events, in place.
func redactSpan(sp any) {
	obj, ok := sp.(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"attrs", "resource"} {
		if m, ok := obj[key].(map[string]any); ok {
			for k, v := range m {
				if s, ok := v.(string); ok {
					m[k] = redact.String(s)
				}
			}
		}
	}
	if events, ok := obj["events"].([]any); ok {
		for _, ev := range events {
			if e, ok := ev.(map[string]any); ok {
				if attrs, ok := e["attrs"].(map[string]any); ok {
					for k, v := range attrs {
						if s, ok := v.(string); ok {
							attrs[k] = redact.String(s)
						}
					}
				}
			}
		}
	}
}
