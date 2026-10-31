// Package server builds the MCP server and registers the read-only tool
// surface (ADR-025, contracts §26). Every tool wraps one or two GET endpoints
// of services/api; nothing here talks to a database, and no tool mutates state.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// ToolCount is asserted by the end-to-end test and printed at startup.
const ToolCount = 12

// Server holds the shared client and logger and owns tool registration.
type Server struct {
	api *apiclient.Client
	log *slog.Logger
}

// New returns an MCP server with all v1 tools registered.
func New(api *apiclient.Client, log *slog.Logger) *mcp.Server {
	s := &Server{api: api, log: log}
	srv := mcp.NewServer(&mcp.Implementation{Name: "tasktrooper", Version: "v0.1.0"}, &mcp.ServerOptions{
		Logger: log, // stderr; the SDK's default logger is discarded
	})
	s.registerServiceTools(srv)
	s.registerTraceTools(srv)
	s.registerLogTools(srv)
	s.registerAlertingTools(srv)
	s.registerLoadtestTools(srv)
	s.registerDeployTools(srv)
	return srv
}

// ---- shared input fields ----
//
// range and env appear on most tools and are defined once so the enum and the
// default window stay identical across the surface (contracts §8). They are
// pointers so an omitted field is distinguishable from an empty one and the
// generated schema marks them optional.
//
// NOTE: jsonschema-go treats the jsonschema struct tag as a plain description
// (it rejects tags starting with "WORD="), so enums are listed in the
// description text and enforced by Query() below — never as enum= tags, which
// make mcp.AddTool panic at startup.

type RangeInput struct {
	Range *string `json:"range,omitempty" jsonschema:"Time window: 15m, 30m, 1h, 6h, 24h or 7d (default 1h)"`
	Env   *string `json:"env,omitempty" jsonschema:"Deployment environment filter (empty = all)"`
}

// validRanges is the contracts §8 range enum.
var validRanges = map[string]bool{"15m": true, "30m": true, "1h": true, "6h": true, "24h": true, "7d": true}

// Query validates the shared input and builds the query string for a
// range-scoped GET. Validation happens before any API call.
func (r RangeInput) Query() (url.Values, error) {
	if v := deref(r.Range); v != "" && !validRanges[v] {
		return nil, toolErrf("range must be one of 15m, 30m, 1h, 6h, 24h, 7d (got %q)", v)
	}
	q := url.Values{}
	apiclient.PutIf(q, "range", apiclient.Range(deref(r.Range)))
	apiclient.PutIf(q, "env", deref(r.Env))
	return q, nil
}

// serviceDetailInput is the shared shape for tools that take a service name in
// the path.
type serviceDetailInput struct {
	RangeInput
	Service string `json:"service" jsonschema:"Service name, as returned by tt_list_services"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- result shaping ----

// shaped fits a decoded API document under the result cap and returns it as
// tool content. fields names the list to trim (nil = single object, no trim
// beyond the byte cap); hint tells the agent how to narrow the query.
func shaped(doc any, fields []string, hint string) (*mcp.CallToolResult, any, error) {
	out, err := apiclient.Truncate(doc, fields, hint)
	if err != nil {
		return nil, nil, toolErrf("could not shape result")
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, nil, toolErrf("could not encode result")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
}

// toolErrf builds a short, actionable error the LLM can see and self-correct
// from. Returned as a regular error so the SDK marks the result IsError.
func toolErrf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
