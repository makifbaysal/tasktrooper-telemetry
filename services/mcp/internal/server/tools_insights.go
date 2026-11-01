package server

import (
	"context"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
	"github.com/tasktrooper/telemetry/services/mcp/internal/redact"
)

// registerInsightTools adds the read tools left out of v1 (ADR-032): error issues, KPIs, code-intelligence
// suggestions and inventory, monitor templates, notification channels, RUM app summary and AI usage. They are
// what an agent needs to design alerts that fit the project.
func (s *Server) registerInsightTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_errors",
		Description: "Error issues grouped by fingerprint (type, message, count, first / last seen, affected service and route); use for 'what is failing' and to pick error_new or error-rate monitors. Returns fingerprints for tt_get_error.",
	}, s.listErrors)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_get_error",
		Description: "ONE error issue with the stack of its latest occurrence (symbolicated when possible). Requires a fingerprint from tt_list_errors.",
	}, s.getError)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_kpis",
		Description: "KPIs (SLOs) with current SLI, error budget, burn rates and burn-rate rule states; check before adding monitors so an SLO burn alert is not duplicated by a threshold monitor.",
	}, s.listKpis)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_alert_suggestions",
		Description: "Monitor suggestions from code analysis (criticality, rationale, ready monitor draft). Prefer accepting a suggestion (tt_accept_suggestion) over writing the same monitor by hand. Empty when no repository is connected.",
	}, s.listSuggestions)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_inventory",
		Description: "Code inventory from connected repositories: endpoints, consumers, crons, views and dependencies, incl. dead (no traffic for 30 days) and shadow (traffic, not in code) items. Use to find crons for job_missed monitors and critical routes.",
	}, s.listInventory)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_monitor_templates",
		Description: "Ready-made infra, serverless and LLM monitors (kind + query + condition + severity). Copy one into tt_preview_monitor / tt_create_monitor instead of inventing the query shape for those kinds.",
	}, s.listMonitorTemplates)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_notify_channels",
		Description: "Where alerts can go: Slack App status + the channels its bot sees, and the project's configured channels (PagerDuty, Opsgenie, webhook, Teams, e-mail). Every item has the exact target string for a monitor's or KPI alert's channels list. Pass refresh: true for a Slack channel created in the last 5 minutes.",
	}, s.listNotifyChannels)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_get_app_summary",
		Description: "Web / mobile app (RUM) summary: users, sessions, error rate, Web Vitals or app start / frozen frames. Use for client apps from tt_list_services (type web or mobile).",
	}, s.getAppSummary)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_get_ai_overview",
		Description: "LLM usage: calls, errors, tokens, estimated cost and p95 by model, service and endpoint. Use before adding llm cost / error monitors.",
	}, s.getAIOverview)
}

// ---- errors ----

const (
	defaultErrorLimit = 20
	maxErrorLimit     = 100
)

var validErrorStatus = map[string]bool{"open": true, "resolved": true, "ignored": true}

type errorListInput struct {
	RangeInput
	Service string `json:"service,omitempty" jsonschema:"Service name, as returned by tt_list_services"`
	Route   string `json:"route,omitempty" jsonschema:"Route template, e.g. /v1/orders/:id"`
	Status  string `json:"status,omitempty" jsonschema:"open, resolved or ignored (default: all)"`
	Limit   *int   `json:"limit,omitempty" jsonschema:"Maximum issues (default 20, max 100)"`
}

func (s *Server) listErrors(ctx context.Context, _ *mcp.CallToolRequest, in errorListInput) (*mcp.CallToolResult, any, error) {
	if in.Status != "" && !validErrorStatus[in.Status] {
		return nil, nil, toolErrf("status must be one of open, resolved, ignored")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "route", in.Route)
	apiclient.PutIf(q, "status", in.Status)
	q.Set("limit", itoa(apiclient.LimitValue(in.Limit, defaultErrorLimit, maxErrorLimit)))
	var doc any
	if err := s.api.Get(ctx, "/api/v1/errors", q, &doc); err != nil {
		return nil, nil, err
	}
	redactDoc(doc)
	return shaped(doc, []string{"items"}, "narrow with service, route or status")
}

type errorGetInput struct {
	RangeInput
	Fingerprint string `json:"fingerprint" jsonschema:"Error fingerprint, from tt_list_errors"`
}

func (s *Server) getError(ctx context.Context, _ *mcp.CallToolRequest, in errorGetInput) (*mcp.CallToolResult, any, error) {
	fp := strings.TrimSpace(in.Fingerprint)
	if fp == "" {
		return nil, nil, toolErrf("fingerprint is required (from tt_list_errors)")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	var doc any
	if err := s.api.Get(ctx, "/api/v1/errors/"+url.PathEscape(fp), q, &doc); err != nil {
		return nil, nil, err
	}
	redactDoc(doc)
	return shaped(doc, nil, "")
}

// ---- KPIs, suggestions, inventory, templates ----

type kpiListInput struct {
	Service string `json:"service,omitempty" jsonschema:"Service name filter, as returned by tt_list_services"`
}

func (s *Server) listKpis(ctx context.Context, _ *mcp.CallToolRequest, in kpiListInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	apiclient.PutIf(q, "serviceId", in.Service)
	var doc any
	if err := s.api.Get(ctx, "/api/v1/kpis", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with service")
}

var validSuggestionStates = map[string]bool{"proposed": true, "accepted": true, "rejected": true, "all": true}

type suggestionListInput struct {
	State string `json:"state,omitempty" jsonschema:"proposed (default), accepted, rejected or all"`
}

func (s *Server) listSuggestions(ctx context.Context, _ *mcp.CallToolRequest, in suggestionListInput) (*mcp.CallToolResult, any, error) {
	if in.State != "" && !validSuggestionStates[in.State] {
		return nil, nil, toolErrf("state must be one of proposed, accepted, rejected, all")
	}
	q := url.Values{}
	apiclient.PutIf(q, "state", in.State)
	var doc any
	if err := s.api.Get(ctx, "/api/v1/suggestions", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "filter with state")
}

type inventoryInput struct {
	Kind    string `json:"kind,omitempty" jsonschema:"Item kind filter (endpoint, consumer, cron, view, dependency)"`
	Service string `json:"service,omitempty" jsonschema:"Service name filter"`
	RepoID  string `json:"repoId,omitempty" jsonschema:"Repository id filter"`
}

func (s *Server) listInventory(ctx context.Context, _ *mcp.CallToolRequest, in inventoryInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	apiclient.PutIf(q, "kind", in.Kind)
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "repoId", in.RepoID)
	var doc any
	if err := s.api.Get(ctx, "/api/v1/inventory", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with kind or service")
}

type noInput struct{}

func (s *Server) listMonitorTemplates(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
	var doc any
	if err := s.api.Get(ctx, "/api/v1/monitor-templates", nil, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "")
}

// ---- notification channels ----

type channelListInput struct {
	Query   string `json:"q,omitempty" jsonschema:"Slack channel name substring"`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"Bypass the 5 minute Slack channel cache (a channel created just now)"`
}

// listNotifyChannels answers "where can an alert go": the Slack App installation and its channels, plus the
// project's named notification channels (PagerDuty, Opsgenie, webhook, Teams, e-mail, Slack webhooks). Every item
// carries the exact string for a channels list, so the agent never builds one by hand.
func (s *Server) listNotifyChannels(ctx context.Context, _ *mcp.CallToolRequest, in channelListInput) (*mcp.CallToolResult, any, error) {
	out := map[string]any{
		"targetFormats": []string{
			"slack:<channelId>#<name> (Slack App channel: slack.channels[].target)",
			"<name> (a configured channel: configured[].target)",
			"email:a@b.c[,c@d.e] (works without a configured channel)",
		},
	}

	var status map[string]any
	if err := s.api.Get(ctx, "/api/v1/integrations/slack", nil, &status); err != nil {
		out["slack"] = map[string]any{"error": err.Error()}
	} else if installed, _ := status["installed"].(bool); !installed {
		out["slack"] = map[string]any{"installed": false, "note": "Slack App not connected: Settings → Integrations → Slack"}
	} else {
		slack := map[string]any{"installed": true, "team": status["teamName"]}
		q := url.Values{}
		apiclient.PutIf(q, "q", strings.TrimPrefix(strings.TrimSpace(in.Query), "#"))
		if in.Refresh {
			q.Set("refresh", "true")
		}
		var list []map[string]any
		if err := s.api.Get(ctx, "/api/v1/integrations/slack/channels", q, &list); err != nil {
			slack["error"] = err.Error()
		} else {
			for _, c := range list {
				id, _ := c["id"].(string)
				name, _ := c["name"].(string)
				c["target"] = "slack:" + id + "#" + strings.TrimPrefix(name, "#")
			}
			slack["channels"] = list
			slack["note"] = "private channels need /invite @TaskTrooper first; public ones work without joining"
		}
		out["slack"] = slack
	}

	var configured []map[string]any
	if err := s.api.Get(ctx, "/api/v1/notification-channels", nil, &configured); err != nil {
		out["configured"] = map[string]any{"error": err.Error()}
	} else {
		items := make([]map[string]any, 0, len(configured))
		for _, c := range configured { // config holds webhook URLs / key references: never pass it on
			items = append(items, map[string]any{"name": c["name"], "type": c["type"], "target": c["name"]})
		}
		out["configured"] = items
	}
	return shaped(out, nil, "narrow Slack channels with q")
}

// ---- RUM, AI ----

type appSummaryInput struct {
	RangeInput
	App string `json:"app" jsonschema:"Client app name (a web or mobile service from tt_list_services)"`
}

func (s *Server) getAppSummary(ctx context.Context, _ *mcp.CallToolRequest, in appSummaryInput) (*mcp.CallToolResult, any, error) {
	app := strings.TrimSpace(in.App)
	if app == "" {
		return nil, nil, toolErrf("app is required (a web or mobile service from tt_list_services)")
	}
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	var doc any
	if err := s.api.Get(ctx, "/api/v1/apps/"+url.PathEscape(app)+"/summary", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, nil, "")
}

type aiOverviewInput struct {
	RangeInput
	Service  string `json:"service,omitempty" jsonschema:"Service name filter"`
	Provider string `json:"provider,omitempty" jsonschema:"gen_ai.provider.name filter, e.g. openai, anthropic"`
	Model    string `json:"model,omitempty" jsonschema:"Model filter"`
}

func (s *Server) getAIOverview(ctx context.Context, _ *mcp.CallToolRequest, in aiOverviewInput) (*mcp.CallToolResult, any, error) {
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "service", in.Service)
	apiclient.PutIf(q, "provider", in.Provider)
	apiclient.PutIf(q, "model", in.Model)
	var doc any
	if err := s.api.Get(ctx, "/api/v1/ai/overview", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"series"}, "narrow with service, provider or model")
}

// redactDoc masks PII in every string of a decoded document (error messages and stacks can carry emails, IPs,
// tokens).
func redactDoc(doc any) {
	switch v := doc.(type) {
	case map[string]any:
		for k, x := range v {
			if s, ok := x.(string); ok {
				v[k] = redact.String(s)
			} else {
				redactDoc(x)
			}
		}
	case []any:
		for i, x := range v {
			if s, ok := x.(string); ok {
				v[i] = redact.String(s)
			} else {
				redactDoc(x)
			}
		}
	}
}
