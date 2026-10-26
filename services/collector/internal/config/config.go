// Package config defines the collector's target list. It is loaded from a static YAML file
// (--config) and/or pulled from the api (GET /api/v1/internal/collector-config). Both use the
// same field names (camelCase, matching the api's JSON conventions and the Postgres
// `databases` / `broker_clusters` columns).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults.
const (
	DefaultInterval = 15 * time.Second
	DefaultTimeout  = 10 * time.Second
	DefaultUserSQL  = "trooper_monitor"
	DefaultUserMQ   = "trooper"
	DefaultTopN     = 50
	DefaultSlowMs   = 500
)

// Duration accepts "15s" strings (YAML/JSON) or plain numbers of seconds.
type Duration time.Duration

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func parseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return Duration(f * float64(time.Second)), nil
	}
	v, err := time.ParseDuration(s)
	return Duration(v), err
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = v
	return nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		return nil
	}
	v, err := parseDuration(s)
	*d = v
	return err
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// FlexString accepts a JSON string or number (the api sends bigint ids as numbers).
type FlexString string

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = FlexString(n.String())
	return nil
}

// Config is the full target list.
type Config struct {
	// Interval between scrapes of each target (default 15s).
	Interval Duration `yaml:"interval" json:"interval"`
	// Timeout of one scrape of one target (default 10s).
	Timeout        Duration   `yaml:"timeout" json:"timeout"`
	Databases      []Database `yaml:"databases" json:"databases"`
	BrokerClusters []Broker   `yaml:"brokerClusters" json:"brokerClusters"`
}

// Database is one row of Postgres `databases` plus collector-only knobs.
type Database struct {
	ID   FlexString `yaml:"id" json:"id"`
	Slug string     `yaml:"slug" json:"slug"`
	// Name is what services use in their dependency ("postgres · orders-db").
	Name string `yaml:"name" json:"name"`
	// Engine: postgres | mysql | redis | mongodb.
	Engine string `yaml:"engine" json:"engine"`
	Host   string `yaml:"host" json:"host"`
	Port   int    `yaml:"port" json:"port"`
	// Collector: agent | cloud | direct.
	Collector string `yaml:"collector" json:"collector"`
	// CredentialsRef: "env:NAME" or "file:/path". The secret is either a full DSN/URL or just a password.
	CredentialsRef  string `yaml:"credentialsRef" json:"credentialsRef"`
	CloudResourceID string `yaml:"cloudResourceId" json:"cloudResourceId"`
	Env             string `yaml:"env" json:"env"`

	// Collector-only (optional).
	DSN      string `yaml:"dsn" json:"dsn"`           // inline DSN; wins over credentialsRef when set
	User     string `yaml:"user" json:"user"`         // default trooper_monitor (redis: none)
	Database string `yaml:"database" json:"database"` // postgres/mysql database to connect to
	TLS      string `yaml:"tls" json:"tls"`           // postgres sslmode / mysql tls / redis "true"
	Node     string `yaml:"node" json:"node"`         // node name (default host)
	// HostMetrics: when the agent runs on the database host (sidecar / VM), report CPU/memory/disk
	// of this machine for this database. nil = follow --host-metrics for agent-mode targets.
	HostMetrics *bool    `yaml:"hostMetrics" json:"hostMetrics"`
	Interval    Duration `yaml:"interval" json:"interval"`
	Timeout     Duration `yaml:"timeout" json:"timeout"`
	TopQueries  int      `yaml:"topQueries" json:"topQueries"`   // statements per scrape (default 50)
	SlowQueryMs float64  `yaml:"slowQueryMs" json:"slowQueryMs"` // "slow" threshold (default 500)
	// TopQueriesSource (MongoDB): auto (default: $queryStats, else the profiler) | querystats | profiler | none.
	TopQueriesSource string `yaml:"topQueriesSource" json:"topQueriesSource"`
}

// Broker is one row of Postgres `broker_clusters` plus collector-only knobs.
type Broker struct {
	ID   FlexString `yaml:"id" json:"id"`
	Slug string     `yaml:"slug" json:"slug"`
	Name string     `yaml:"name" json:"name"`
	// Kind: kafka | rabbitmq.
	Kind string `yaml:"kind" json:"kind"`
	// Endpoint: Kafka bootstrap servers (comma separated) or RabbitMQ management API URL.
	Endpoint       string     `yaml:"endpoint" json:"endpoint"`
	Vhost          string     `yaml:"vhost" json:"vhost"`
	Auth           BrokerAuth `yaml:"auth" json:"auth"`
	CredentialsRef string     `yaml:"credentialsRef" json:"credentialsRef"`
	Collector      string     `yaml:"collector" json:"collector"`

	// Collector-only (optional).
	User        string   `yaml:"user" json:"user"`               // default "trooper"
	TopicFilter string   `yaml:"topicFilter" json:"topicFilter"` // regexp; only matching topics/queues
	GroupFilter string   `yaml:"groupFilter" json:"groupFilter"` // regexp; only matching groups
	Interval    Duration `yaml:"interval" json:"interval"`
	Timeout     Duration `yaml:"timeout" json:"timeout"`
	// Nodes: optional Prometheus endpoints (node exporter, JMX exporter, Redpanda /public_metrics,
	// rabbitmq_prometheus) for trooper.broker.node.* of self-managed brokers.
	Nodes []BrokerNode `yaml:"nodes" json:"nodes"`
	// NodeMetrics (RabbitMQ): "auto" (default) probes the rabbitmq_prometheus plugin on
	// <management host>:15692/metrics when no nodes are configured; "off" disables it.
	NodeMetrics string `yaml:"nodeMetrics" json:"nodeMetrics"`
	// TopicMetadataEvery: how often partitions / replication / retention are re-read (default 5m).
	TopicMetadataEvery Duration `yaml:"topicMetadataEvery" json:"topicMetadataEvery"`
}

// BrokerNode is one Prometheus scrape endpoint of a broker node.
type BrokerNode struct {
	// Name is trooper.broker.node (default: the metrics URL host).
	Name string `yaml:"name" json:"name"`
	// BrokerID links a Kafka broker id to this node (its log-dir disk usage is reported under Name).
	BrokerID *int32 `yaml:"brokerId" json:"brokerId"`
	// MetricsURL is the Prometheus text endpoint, e.g. http://kafka-1:9100/metrics.
	MetricsURL string `yaml:"metricsUrl" json:"metricsUrl"`
	// DiskMount: node exporter filesystem behind trooper.broker.node.disk (default: the Kafka
	// data mount if known, else "/").
	DiskMount string `yaml:"diskMount" json:"diskMount"`
}

// BrokerAuth mirrors broker_clusters.auth: { mechanism: none|plain|scram-sha-256|scram-sha-512|iam|oauthbearer, tls }.
type BrokerAuth struct {
	Mechanism string `yaml:"mechanism" json:"mechanism"`
	// AWS MSK IAM (mechanism iam): credentials from credentialsRef ("accessKeyId:secret[:sessionToken]")
	// or the default AWS chain (env, shared config/profile, web identity / IRSA, ECS, EC2 IMDS);
	// optional roleArn is assumed with STS.
	AWSRegion  string `yaml:"awsRegion" json:"awsRegion"`
	AWSProfile string `yaml:"awsProfile" json:"awsProfile"`
	RoleARN    string `yaml:"roleArn" json:"roleArn"`
	// OAUTHBEARER: tokenUrl set → client-credentials grant with credentialsRef "clientId:clientSecret";
	// else credentialsRef is the (static) bearer token.
	TokenURL           string            `yaml:"tokenUrl" json:"tokenUrl"`
	Scopes             []string          `yaml:"scopes" json:"scopes"`
	Extensions         map[string]string `yaml:"extensions" json:"extensions"`
	TLS                bool              `yaml:"tls" json:"tls"`
	CAFile             string            `yaml:"caFile" json:"caFile"`
	CertFile           string            `yaml:"certFile" json:"certFile"`
	KeyFile            string            `yaml:"keyFile" json:"keyFile"`
	InsecureSkipVerify bool              `yaml:"insecureSkipVerify" json:"insecureSkipVerify"`
}

// Key is the stable id used in metric attributes (trooper.db.id / trooper.broker.cluster):
// the slug, else the id, else the name.
func (d Database) Key() string { return firstNonEmpty(d.Slug, string(d.ID), d.Name) }

// Key is the stable id used in metric attributes.
func (b Broker) Key() string { return firstNonEmpty(b.Slug, string(b.ID), b.Name) }

// DisplayName returns name, falling back to the key.
func (d Database) DisplayName() string { return firstNonEmpty(d.Name, d.Key()) }

// NodeName returns the node attribute for this database.
func (d Database) NodeName() string {
	if d.Node != "" {
		return d.Node
	}
	if d.Host != "" {
		return d.Host
	}
	return d.Key()
}

// DefaultPort returns the engine's default port.
func DefaultPort(engine string) int {
	switch engine {
	case "postgres":
		return 5432
	case "mysql":
		return 3306
	case "redis":
		return 6379
	case "mongodb":
		return 27017
	}
	return 0
}

// Parse decodes YAML (or JSON, which is valid YAML), applies defaults and validates.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c.normalize()
}

// ParseJSON decodes the api's JSON response (unknown fields ignored for forward compatibility).
func ParseJSON(b []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("collector-config: %w", err)
	}
	return c.normalize()
}

// LoadFile reads and parses a YAML file.
func LoadFile(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

var engines = []string{"postgres", "mysql", "redis", "mongodb"}
var kinds = []string{"kafka", "rabbitmq"}
var collectors = []string{"agent", "cloud", "direct"}

func (c Config) normalize() (*Config, error) {
	if c.Interval <= 0 {
		c.Interval = Duration(DefaultInterval)
	}
	if c.Timeout <= 0 {
		c.Timeout = Duration(DefaultTimeout)
	}
	var errs []error
	seen := map[string]bool{}
	for i := range c.Databases {
		d := &c.Databases[i]
		d.Engine = strings.ToLower(strings.TrimSpace(d.Engine))
		if d.Engine == "postgresql" {
			d.Engine = "postgres"
		}
		d.Collector = strings.ToLower(firstNonEmpty(d.Collector, "agent"))
		if d.Interval <= 0 {
			d.Interval = c.Interval
		}
		if d.Timeout <= 0 {
			d.Timeout = c.Timeout
		}
		if d.Port == 0 {
			d.Port = DefaultPort(d.Engine)
		}
		if d.TopQueries <= 0 {
			d.TopQueries = DefaultTopN
		}
		if d.SlowQueryMs <= 0 {
			d.SlowQueryMs = DefaultSlowMs
		}
		d.TopQueriesSource = strings.ToLower(firstNonEmpty(d.TopQueriesSource, "auto"))
		switch {
		case d.Key() == "":
			errs = append(errs, fmt.Errorf("databases[%d]: id, slug or name is required", i))
		case !slices.Contains(engines, d.Engine):
			errs = append(errs, fmt.Errorf("databases[%d] %s: engine %q must be one of %v", i, d.Key(), d.Engine, engines))
		case !slices.Contains(collectors, d.Collector):
			errs = append(errs, fmt.Errorf("databases[%d] %s: collector %q must be one of %v", i, d.Key(), d.Collector, collectors))
		case d.Host == "" && d.DSN == "" && d.CredentialsRef == "" && d.Collector != "cloud":
			errs = append(errs, fmt.Errorf("databases[%d] %s: host, dsn or credentialsRef is required", i, d.Key()))
		case seen["db/"+d.Key()]:
			errs = append(errs, fmt.Errorf("databases[%d]: duplicate id %q", i, d.Key()))
		}
		seen["db/"+d.Key()] = true
	}
	for i := range c.BrokerClusters {
		b := &c.BrokerClusters[i]
		b.Kind = strings.ToLower(strings.TrimSpace(b.Kind))
		b.Collector = strings.ToLower(firstNonEmpty(b.Collector, "agent"))
		b.Auth.Mechanism = strings.ToLower(firstNonEmpty(b.Auth.Mechanism, "none"))
		if b.Interval <= 0 {
			b.Interval = c.Interval
		}
		if b.Timeout <= 0 {
			b.Timeout = c.Timeout
		}
		if b.Kind == "rabbitmq" && b.Vhost == "" {
			b.Vhost = "/"
		}
		if b.Auth.Mechanism == "aws-msk-iam" || b.Auth.Mechanism == "aws_msk_iam" {
			b.Auth.Mechanism = "iam"
		}
		if b.TopicMetadataEvery <= 0 {
			b.TopicMetadataEvery = Duration(5 * time.Minute)
		}
		b.NodeMetrics = strings.ToLower(firstNonEmpty(b.NodeMetrics, "auto"))
		for j, n := range b.Nodes {
			if n.MetricsURL == "" && n.BrokerID == nil {
				errs = append(errs, fmt.Errorf("brokerClusters[%d].nodes[%d]: metricsUrl or brokerId is required", i, j))
			}
		}
		switch {
		case b.Key() == "":
			errs = append(errs, fmt.Errorf("brokerClusters[%d]: id, slug or name is required", i))
		case !slices.Contains(kinds, b.Kind):
			errs = append(errs, fmt.Errorf("brokerClusters[%d] %s: kind %q must be one of %v", i, b.Key(), b.Kind, kinds))
		case b.Endpoint == "":
			errs = append(errs, fmt.Errorf("brokerClusters[%d] %s: endpoint is required", i, b.Key()))
		case seen["mq/"+b.Key()]:
			errs = append(errs, fmt.Errorf("brokerClusters[%d]: duplicate id %q", i, b.Key()))
		}
		for _, re := range []string{b.TopicFilter, b.GroupFilter} {
			if re != "" {
				if _, err := regexp.Compile(re); err != nil {
					errs = append(errs, fmt.Errorf("brokerClusters[%d] %s: bad filter %q: %w", i, b.Key(), re, err))
				}
			}
		}
		seen["mq/"+b.Key()] = true
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &c, nil
}

// Merge returns base plus the targets of extra whose keys are not already in base (static
// config wins over remote config).
func Merge(base, extra *Config) *Config {
	switch {
	case base == nil && extra == nil:
		return &Config{Interval: Duration(DefaultInterval), Timeout: Duration(DefaultTimeout)}
	case base == nil:
		return extra
	case extra == nil:
		return base
	}
	out := *base
	out.Databases = slices.Clone(base.Databases)
	out.BrokerClusters = slices.Clone(base.BrokerClusters)
	have := map[string]bool{}
	for _, d := range base.Databases {
		have["db/"+d.Key()] = true
	}
	for _, b := range base.BrokerClusters {
		have["mq/"+b.Key()] = true
	}
	for _, d := range extra.Databases {
		if !have["db/"+d.Key()] {
			out.Databases = append(out.Databases, d)
		}
	}
	for _, b := range extra.BrokerClusters {
		if !have["mq/"+b.Key()] {
			out.BrokerClusters = append(out.BrokerClusters, b)
		}
	}
	return &out
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
