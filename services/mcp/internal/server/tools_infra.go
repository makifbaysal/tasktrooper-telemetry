package server

import (
	"context"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// registerInfraTools adds the infrastructure read tools (ADR-032): hosts, databases and consumer groups, the
// targets of infra and consumer_lag monitors.
func (s *Server) registerInfraTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_hosts",
		Description: "Hosts and K8s nodes reported by trooper-agent with CPU, memory, disk and load and their status; use for infra monitors (entity host).",
	}, s.listHosts)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_databases",
		Description: "Registered databases (Postgres, MySQL, Redis, ClickHouse, ...) with live health: connections, slow queries, replication; use to judge database alerts.",
	}, s.listDatabases)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "tt_list_consumer_groups",
		Description: "Message-queue consumer groups across clusters (Kafka, RabbitMQ, SQS, Pub/Sub) with lag and owner service; returns clusterId + group, the target of consumer_lag monitors.",
	}, s.listConsumerGroups)
}

type hostListInput struct {
	Range   *string `json:"range,omitempty" jsonschema:"Time window: 15m, 30m, 1h, 6h, 24h or 7d (default 1h)"`
	Cluster string  `json:"cluster,omitempty" jsonschema:"K8s cluster filter"`
	Query   string  `json:"q,omitempty" jsonschema:"Substring of host name, host id, node or cluster"`
}

func (s *Server) listHosts(ctx context.Context, _ *mcp.CallToolRequest, in hostListInput) (*mcp.CallToolResult, any, error) {
	q, err := RangeInput{Range: in.Range}.Query()
	if err != nil {
		return nil, nil, err
	}
	apiclient.PutIf(q, "cluster", in.Cluster)
	apiclient.PutIf(q, "q", in.Query)
	var doc any
	if err := s.api.Get(ctx, "/api/v1/infra/hosts", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with q or cluster")
}

func (s *Server) listDatabases(ctx context.Context, _ *mcp.CallToolRequest, in RangeInput) (*mcp.CallToolResult, any, error) {
	q, err := in.Query()
	if err != nil {
		return nil, nil, err
	}
	var doc any
	if err := s.api.Get(ctx, "/api/v1/infra/databases", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "")
}

type groupListInput struct {
	ClusterID string `json:"clusterId,omitempty" jsonschema:"Cluster id filter"`
	Service   string `json:"service,omitempty" jsonschema:"Owner service filter"`
}

func (s *Server) listConsumerGroups(ctx context.Context, _ *mcp.CallToolRequest, in groupListInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	apiclient.PutIf(q, "clusterId", in.ClusterID)
	apiclient.PutIf(q, "serviceId", in.Service)
	var doc any
	if err := s.api.Get(ctx, "/api/v1/infra/groups", q, &doc); err != nil {
		return nil, nil, err
	}
	return shaped(doc, []string{"items"}, "narrow with clusterId or service")
}
