// Package model holds the types shared by the trooper-cloud runner, store and providers (contracts §5, §10).
package model

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Integration is one row of Postgres `integrations`.
type Integration struct {
	ID             int64
	ProjectID      int64
	Provider       string // aws | gcp | neon | vercel
	Name           string
	Config         json.RawMessage
	CredentialsRef string
	Status         string
	LastSyncAt     *time.Time
}

// Database is a discovered `databases` row (contracts §10). Slug is derived from Provider + Name.
type Database struct {
	Name            string
	Engine          string // postgres | mysql | redis
	Host            string
	Port            int
	Source          string // 'AWS RDS · eu-central-1'
	Version         string
	Zone            string
	InstanceClass   string
	DiskGB          *float64
	CloudResourceID string
	Env             string
	Meta            map[string]any
}

// Broker is a discovered `broker_clusters` row.
type Broker struct {
	Name     string
	Kind     string // kafka | sqs | pubsub
	Endpoint string
	Source   string
	Version  string
	Meta     map[string]any
}

// Inventory is what discovery found. Provider-specific state the metrics step needs lives in State.
type Inventory struct {
	Databases []Database
	Brokers   []Broker
	// Other counts resources that are not stored as databases/brokers (Vercel projects).
	Other int
	State any
}

// Count is the number of discovered resources (integrations.resources).
func (i *Inventory) Count() int { return len(i.Databases) + len(i.Brokers) + i.Other }

// Sample is one gauge data point (contracts §5 names).
type Sample struct {
	Name   string
	Value  float64
	TS     time.Time
	Region string
	Attrs  map[string]string
}

// DeployMarker is a `deploy_markers` row.
type DeployMarker struct {
	Service       string
	Env           string
	Version       string
	Source        string // vercel
	URL           string
	CommitMessage string
	Author        string
	State         string // building | ready | error | canceled
	TS            time.Time
	ExternalID    string
}

// Result is what one metrics step produced.
type Result struct {
	Samples []Sample
	Deploys []DeployMarker
	// DBMeta patches `databases.meta` by slug (e.g. Neon compute state), merged with ||.
	DBMeta map[string]map[string]any
}

// Provider is one cloud integration instance (credentials already resolved).
type Provider interface {
	// Discover lists resources. A partial inventory may be returned together with an error.
	Discover(ctx context.Context) (*Inventory, error)
	// Collect reads metrics (and deploys) for the last complete minute. Partial results + error allowed.
	Collect(ctx context.Context, inv *Inventory, now time.Time) (*Result, error)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug is `<provider>-<name>` lower-cased with every non [a-z0-9] run replaced by '-' (contracts §10).
func Slug(provider, name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 100 {
		s = s[:100]
	}
	return provider + "-" + s
}

// Metric names (contracts §5).
const (
	DBCPU         = "trooper.db.cpu"
	DBMemory      = "trooper.db.memory"
	DBDisk        = "trooper.db.disk"
	DBConnections = "trooper.db.connections"
	DBMaxConn     = "trooper.db.max_connections"
	DBOps         = "trooper.db.ops"
	DBHitRatio    = "trooper.db.hit_ratio"
	DBEvictions   = "trooper.db.evictions"
	DBReplLag     = "trooper.db.replication_lag"

	BrokerLag         = "trooper.broker.lag"
	BrokerConsumeRate = "trooper.broker.consume_rate"
	BrokerProduceRate = "trooper.broker.produce_rate"
	BrokerUnacked     = "trooper.broker.unacked"
	BrokerOldestAge   = "trooper.broker.oldest_age_s"
	BrokerNodeCPU     = "trooper.broker.node.cpu"
	BrokerNodeMemory  = "trooper.broker.node.memory"
	BrokerNodeDisk    = "trooper.broker.node.disk"
	BrokerNodeNetIn   = "trooper.broker.node.net_in"
	TopicRetentionMs  = "trooper.broker.topic.retention_ms"
	TopicInRate       = "trooper.broker.topic.in_rate"
)

// DBAttrs are the attributes of trooper.db.* gauges.
func DBAttrs(slug, name, system, node, role string) map[string]string {
	return map[string]string{
		"trooper.db.id": slug, "trooper.db.name": name, "db.system": system, "trooper.db.node": node, "trooper.db.role": role,
	}
}

// LagAttrs are the attributes of trooper.broker.lag & co. Queue systems (SQS, Pub/Sub) and aggregated
// Kafka lag (MSK SumOffsetLag) use partition "0".
func LagAttrs(cluster, system, destination, group, partition string) map[string]string {
	return map[string]string{
		"trooper.broker.cluster": cluster, "messaging.system": system, "messaging.destination.name": destination,
		"messaging.consumer.group.name": group, "messaging.destination.partition.id": partition,
	}
}

// NodeAttrs are the attributes of trooper.broker.node.*.
func NodeAttrs(cluster, system, node string) map[string]string {
	return map[string]string{"trooper.broker.cluster": cluster, "messaging.system": system, "trooper.broker.node": node}
}

// TopicAttrs are the attributes of trooper.broker.topic.*.
func TopicAttrs(cluster, system, topic string) map[string]string {
	return map[string]string{"trooper.broker.cluster": cluster, "messaging.system": system, "messaging.destination.name": topic}
}

// DBSystem maps a databases.engine to the OTel db.system value.
func DBSystem(engine string) string {
	if engine == "postgres" {
		return "postgresql"
	}
	return engine
}

// F returns a pointer to f.
func F(f float64) *float64 { return &f }
