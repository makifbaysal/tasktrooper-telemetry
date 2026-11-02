// Package toolset exposes the TaskTrooper MCP tool surface to other services in this repo (ADR-033): the api's
// dashboard assistant builds the same server tt-mcp runs and talks to it over an in-memory transport, so tool
// names, schemas, result shaping and dry-run semantics stay identical.
package toolset

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
	"github.com/tasktrooper/telemetry/services/mcp/internal/server"
)

// Options configure one tool session.
type Options struct {
	// BaseURL is the api base URL the tools call (paths start with /api/v1). With an in-process Transport any
	// syntactically valid URL works, e.g. "http://api.internal".
	BaseURL string
	// Transport serves the tool HTTP calls (nil = http.DefaultTransport).
	Transport http.RoundTripper
	// Auth sets the credentials of every tool request (e.g. the caller's session cookie + project header).
	Auth func(h http.Header)
	// AllowWrite registers the write tools (each acts only with confirm: true).
	AllowWrite bool
	// Timeout of one tool HTTP call (default 15s).
	Timeout time.Duration
	Log     *slog.Logger
}

// Connect builds the MCP server and returns a client session connected to it in memory. Close the session when
// done.
func Connect(ctx context.Context, o Options) (*mcp.ClientSession, error) {
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	api := apiclient.New(o.BaseURL, "", o.Timeout).WithTransport(o.Transport).WithAuth(o.Auth)
	srv := server.New(api, log, server.Options{AllowWrite: o.AllowWrite})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "tasktrooper-assistant", Version: "v0.1.0"}, nil)
	return client.Connect(ctx, ct, nil)
}

// NeedsApproval reports whether a tool call would change something: every write tool acts only with
// confirm: true, so that flag is what a human must approve.
func NeedsApproval(input json.RawMessage) bool {
	var in struct {
		Confirm bool `json:"confirm"`
	}
	_ = json.Unmarshal(input, &in)
	return in.Confirm
}

// ToolCount is the number of tools Connect registers.
func ToolCount(allowWrite bool) int { return server.ToolCount(allowWrite) }
