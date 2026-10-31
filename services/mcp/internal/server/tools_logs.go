package server

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
	"github.com/tasktrooper/telemetry/services/mcp/internal/redact"
)

// registerLogTools adds log search.
func (s *Server) registerLogTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_search_logs",
		Description: "Search log lines (newest first) by query text, level, service, route or trace id; cursor-paginate with `before`/`nextBefore`.",
	}, s.searchLogs)
}

const (
	defaultLogLimit = 50
	maxLogLimit     = 200
)

type logSearchInput struct {
	RangeInput
	Service string `json:"service,omitempty" jsonschema:"Service name, as returned by tt_list_services"`
	Level   string `json:"level,omitempty" jsonschema:"Exact log level, not a minimum: debug, info, warn or error (one per call; query error and warn separately)"`
	Q       string `json:"q,omitempty" jsonschema:"Full-text query over log messages"`
	TraceID string `json:"traceId,omitempty" jsonschema:"32 hex chars; logs belonging to one trace"`
	Route   string `json:"route,omitempty" jsonschema:"Route template filter (e.g. /orders/:id)"`
	Before  *int   `json:"before,omitempty" jsonschema:"Cursor: return lines older than this ts (ms), from the previous nextBefore"`
	Limit   *int   `json:"limit,omitempty" jsonschema:"Maximum lines to return (default 50, max 200)"`
}

func (s *Server) searchLogs(ctx context.Context, _ *mcp.CallToolRequest, in logSearchInput) (*mcp.CallToolResult, any, error) {
	if in.Level != "" && !validLevels[in.Level] {
		return nil, nil, toolErrf("level must be one of debug, info, warn, error")
	}
	if in.TraceID != "" && !hexID.MatchString(strings.ToLower(in.TraceID)) {
		return nil, nil, toolErrf("traceId must be 32 hex chars")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "level", in.Level)
	// The text query and header-like filters are deliberately not logged anywhere.
	apiclient.PutIf(q, "q", in.Q)
	apiclient.PutIf(q, "traceId", in.TraceID)
	apiclient.PutIf(q, "route", in.Route)
	apiclient.PutInt(q, "before", in.Before)
	q.Set("limit", itoa(apiclient.LimitValue(in.Limit, defaultLogLimit, maxLogLimit)))

	var doc any
	if err := s.api.Get(ctx, "/api/v1/logs", q, &doc); err != nil {
		return nil, nil, err
	}
	// Mask PII in the messages before they reach the model.
	if obj, ok := doc.(map[string]any); ok {
		if lines, ok := obj["lines"].([]any); ok {
			for _, l := range lines {
				if line, ok := l.(map[string]any); ok {
					if msg, ok := line["message"].(string); ok {
						line["message"] = redact.String(msg)
					}
				}
			}
		}
	}
	return shaped(doc, []string{"lines"}, "narrow with service, level, q or traceId, or page back with before=<nextBefore>")
}

var validLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
