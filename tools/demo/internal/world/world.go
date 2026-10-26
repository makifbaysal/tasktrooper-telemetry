// Package world describes the demo world. It mirrors the dashboard's mock data
// (apps/web/src/data/mock.ts, infra.ts, kpi.ts, frontend.ts) so that real mode looks like mock mode:
// the same services, endpoints, dependencies, topics, consumer groups, databases and client apps.
package world

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Prefix of every docker container (and state) the demo owns.
const Prefix = "ttdemo-"

// Platform defaults (contracts §2).
const (
	DefaultAPI       = "http://localhost:8080"
	DefaultAPIKey    = "tt_dev_api_key"
	DefaultIngestDSN = "http://tt_dev_ingest_key@localhost:4318/1"
	DefaultKafka     = "localhost:19092"
	DefaultPostgres  = "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"
	WebhookSinkPort  = 57999
	Environment      = "production"
)

// ---------------------------------------------------------------- containers

// Container is one docker container of the demo.
type Container struct {
	Name  string // without prefix
	Image string
	Ports []string // host:container
	Env   []string
	Args  []string
}

// PG is a Postgres database of the world; one container each, because pg_stat_statements and
// pg_stat_database are cluster-wide and every database must show its own queries.
type PG struct {
	ID, Container string
	Port          int
	MaxConns      int
}

var PGs = []PG{
	{ID: "orders-db", Container: "pg-orders", Port: 57432, MaxConns: 60},
	{ID: "payments-db", Container: "pg-payments", Port: 57433, MaxConns: 100},
	{ID: "reco-db", Container: "pg-reco", Port: 57434, MaxConns: 40},
	{ID: "catalog-db", Container: "pg-catalog", Port: 57435, MaxConns: 100},
	{ID: "auth-db", Container: "pg-auth", Port: 57436, MaxConns: 100},
}

// Redis is a cache of the world (one container each: INFO is server-wide).
type Redis struct {
	ID, Container string
	Port          int
	MaxMemory     string
}

var Redises = []Redis{
	{ID: "cache-main", Container: "redis-main", Port: 57379, MaxMemory: "24mb"},
	{ID: "cache-payments", Container: "redis-payments", Port: 57380, MaxMemory: "64mb"},
	{ID: "cache-bff", Container: "redis-bff", Port: 57381, MaxMemory: "64mb"},
	{ID: "reco-cache", Container: "redis-reco", Port: 57382, MaxMemory: "64mb"},
}

const (
	PGPassword    = "ttdemo"
	MySQLPort     = 57306
	MySQLPassword = "ttdemo"
	RabbitAMQP    = 57672
	RabbitMgmt    = 57673
	RabbitUser    = "ttdemo"
	RabbitPass    = "ttdemo"
	RabbitVhost   = "/shop"
)

// Containers returns every container of the demo.
func Containers() []Container {
	var out []Container
	for _, p := range PGs {
		out = append(out, Container{
			Name: p.Container, Image: "postgres:16-alpine", Ports: []string{fmt.Sprintf("%d:5432", p.Port)},
			Env: []string{"POSTGRES_PASSWORD=" + PGPassword},
			Args: []string{"-c", "shared_preload_libraries=pg_stat_statements", "-c", "pg_stat_statements.track=all",
				"-c", fmt.Sprintf("max_connections=%d", p.MaxConns), "-c", "shared_buffers=64MB", "-c", "max_parallel_workers_per_gather=0"},
		})
	}
	for _, r := range Redises {
		out = append(out, Container{
			Name: r.Container, Image: "redis:7-alpine", Ports: []string{fmt.Sprintf("%d:6379", r.Port)},
			Args: []string{"redis-server", "--maxmemory", r.MaxMemory, "--maxmemory-policy", "allkeys-lru", "--save", "", "--appendonly", "no", "--slowlog-log-slower-than", "2000"},
		})
	}
	out = append(out,
		Container{Name: "mysql", Image: "mysql:8.4", Ports: []string{fmt.Sprintf("%d:3306", MySQLPort)},
			Env:  []string{"MYSQL_ROOT_PASSWORD=" + MySQLPassword, "MYSQL_DATABASE=notifications"},
			Args: []string{"--innodb-buffer-pool-size=64M", "--performance-schema=ON", "--long-query-time=0.2", "--slow-query-log=ON"}},
		Container{Name: "rabbitmq", Image: "rabbitmq:3.13-management-alpine",
			Ports: []string{fmt.Sprintf("%d:5672", RabbitAMQP), fmt.Sprintf("%d:15672", RabbitMgmt)},
			Env:   []string{"RABBITMQ_DEFAULT_USER=" + RabbitUser, "RABBITMQ_DEFAULT_PASS=" + RabbitPass, "RABBITMQ_DEFAULT_VHOST=" + RabbitVhost}},
	)
	return out
}

// PGDSN is the superuser DSN of a database (dbname = the database id, e.g. "orders-db", so the
// SDK's db.namespace — and the dependency name "postgresql · orders-db" — match the mock).
func PGDSN(id string, dbname string) string {
	for _, p := range PGs {
		if p.ID == id {
			if dbname == "" {
				dbname = id
			}
			return fmt.Sprintf("postgres://postgres:%s@localhost:%d/%s?sslmode=disable", PGPassword, p.Port, dbname)
		}
	}
	panic("unknown pg " + id)
}

func RedisAddr(id string) string {
	for _, r := range Redises {
		if r.ID == id {
			return fmt.Sprintf("localhost:%d", r.Port)
		}
	}
	panic("unknown redis " + id)
}

func MySQLDSN(db string) string {
	return fmt.Sprintf("root:%s@tcp(localhost:%d)/%s?parseTime=true&interpolateParams=true", MySQLPassword, MySQLPort, db)
}

func RabbitURL() string {
	return fmt.Sprintf("amqp://%s:%s@localhost:%d/%s", RabbitUser, RabbitPass, RabbitAMQP, strings.ReplaceAll(RabbitVhost, "/", "%2F"))
}

// ---------------------------------------------------------------- kafka / rabbit

type Topic struct {
	Name       string
	Partitions int32
	Retention  time.Duration
}

var Topics = []Topic{
	{"orders.created", 12, 7 * 24 * time.Hour},
	{"orders.status-changed", 6, 7 * 24 * time.Hour},
	{"orders.created.dlq", 1, 30 * 24 * time.Hour},
	{"payments.events", 6, 14 * 24 * time.Hour},
	{"reco.events", 6, 2 * 24 * time.Hour},
	{"catalog.events", 6, 7 * 24 * time.Hour},
	{"auth.events", 3, 30 * 24 * time.Hour},
}

// TopicNames lists the demo topics (for filters and `demo down`).
func TopicNames() []string {
	out := make([]string, len(Topics))
	for i, t := range Topics {
		out[i] = t.Name
	}
	return out
}

// Kafka consumer groups (mock names).
const (
	GroupFulfilment  = "order-events.fulfilment"
	GroupSearchIndex = "order-events.search-index"
	GroupProjections = "order-events.projections"
	GroupDLQReplay   = "order-events.dlq-replay"
	GroupS3Sink      = "analytics.s3-sink"
	GroupLedger      = "ledger-sync"
	GroupRecoTrainer = "reco-trainer"
	GroupAudit       = "audit-log.sink"
)

// GroupFilter / TopicFilter keep the collector on the demo's groups and topics (the shared Redpanda
// also carries the platform's own tt.* topics).
const (
	GroupFilter = `^(order-events\.|analytics\.s3-sink$|ledger-sync$|reco-trainer$|audit-log\.sink$)`
	TopicFilter = `^(orders\.|payments\.events$|reco\.events$|catalog\.events$|auth\.events$)`
)

// RabbitMQ topology (vhost /shop).
const (
	ExchangeEmails      = "emails"       // topic
	ExchangeEmailEvents = "email.events" // fanout
	QueueTransactional  = "emails.transactional"
	QueueMarketing      = "emails.marketing"
	QueueEmailEvents    = "email.events"
)

// ---------------------------------------------------------------- services

// Service is a backend service simulated with the Go SDK.
type Service struct {
	Name      string
	Version   string
	Language  string // shown in the UI (mock); the demo itself is Go
	PGType    string // services.type: api | worker
	HTTP      bool
	Instances int
}

var Services = []Service{
	{Name: "orders-api", Version: "v2.41.0", Language: "Node.js", PGType: "api", HTTP: true, Instances: 1},
	{Name: "payments-api", Version: "v1.18.3", Language: "Go", PGType: "api", HTTP: true, Instances: 1},
	{Name: "catalog-api", Version: "v3.2.0", Language: "Go", PGType: "api", HTTP: true, Instances: 1},
	{Name: "auth-api", Version: "v5.0.2", Language: "Node.js", PGType: "api", HTTP: true, Instances: 1},
	{Name: "storefront-bff", Version: "v1.9.4", Language: "Node.js", PGType: "api", HTTP: true, Instances: 1},
	{Name: "recommendation-api", Version: "v0.14.0", Language: "Python", PGType: "api", HTTP: true, Instances: 1},
	{Name: "order-events-consumer", Version: "v2.41.0", Language: "Node.js", Instances: 3},
	{Name: "email-worker", Version: "v0.9.12", Language: "Python", PGType: "worker", Instances: 2},
}

// ServiceByName returns the service definition.
func ServiceByName(n string) (Service, bool) {
	for _, s := range Services {
		if s.Name == n {
			return s, true
		}
	}
	return Service{}, false
}

// ---------------------------------------------------------------- scenarios

// Scenarios are the mock's incident stories, each reproduced for real.
var Scenarios = []struct{ Name, What string }{
	{"reco-db-slow", "migration 0042 drops idx_reco_items_user_score: sequential scans on reco_items slow recommendation-api, storefront-bff and the homepage LCP"},
	{"orders-db-connections", "orders-api keeps a large idle pool: orders-db connections near max_connections"},
	{"fulfilment-hot-partition", "orders keyed by merchant_id, merchant m-1042 is 31% of orders and its records wait on a lock: lag grows on one partition of order-events.fulfilment"},
	{"ledger-sync-rebalancing", "ledger-sync members exceed max.poll.interval and rejoin: constant rebalances, growing lag on payments.events"},
	{"payments-provider-errors", "iyzico times out / fails: POST /v1/charges error rate ~3% and p95 above 600 ms"},
	{"cache-evictions", "auth-api writes session:* keys without TTL: cache-main hits maxmemory and evicts"},
}

// ScenarioNames lists every scenario name.
func ScenarioNames() []string {
	out := make([]string, len(Scenarios))
	for i, s := range Scenarios {
		out[i] = s.Name
	}
	return out
}

// ---------------------------------------------------------------- runtime config (orchestrator → service processes)

// Runtime is passed from the orchestrator to each service process (env DEMO_RUNTIME, JSON).
type Runtime struct {
	DSN       string            `json:"dsn"`
	Peers     map[string]string `json:"peers"`     // service / external name → base URL
	Kafka     []string          `json:"kafka"`     // bootstrap brokers
	Scenarios map[string]int64  `json:"scenarios"` // name → active since (unix ms); absent = off
	RPS       float64           `json:"rps"`
	Debug     bool              `json:"debug"`
}

// Active reports whether a scenario is on now.
func (r Runtime) Active(name string) bool {
	since, ok := r.Scenarios[name]
	return ok && time.Now().UnixMilli() >= since
}

// Encode returns the env value.
func (r Runtime) Encode() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// LoadRuntime reads DEMO_RUNTIME.
func LoadRuntime() (Runtime, error) {
	var r Runtime
	if err := json.Unmarshal([]byte(os.Getenv("DEMO_RUNTIME")), &r); err != nil {
		return r, fmt.Errorf("DEMO_RUNTIME: %w", err)
	}
	return r, nil
}
