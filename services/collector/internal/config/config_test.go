package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseIngestDSN(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    IngestDSN
		wantErr string
	}{
		{"local dev", "http://tt_dev_ingest_key@localhost:4318/1",
			IngestDSN{MetricsURL: "http://localhost:4318/v1/metrics", Key: "tt_dev_ingest_key", ProjectID: "1", Insecure: true}, ""},
		{"https no port", "https://abc@ingest.tasktrooper.io/42",
			IngestDSN{MetricsURL: "https://ingest.tasktrooper.io/v1/metrics", Key: "abc", ProjectID: "42"}, ""},
		{"no project", "https://abc@ingest.example.com",
			IngestDSN{MetricsURL: "https://ingest.example.com/v1/metrics", Key: "abc"}, ""},
		{"missing key", "http://localhost:4318/1", IngestDSN{}, "ingest key is missing"},
		{"bad scheme", "grpc://k@localhost:4317/1", IngestDSN{}, "scheme"},
		{"empty", "", IngestDSN{}, "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIngestDSN(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
		})
	}
}

const sample = `
interval: 30s
databases:
  - id: orders-db
    name: orders-db
    engine: postgres
    host: orders-db.internal
    credentialsRef: env:ORDERS_DB_PASSWORD
  - id: cache
    engine: redis
    host: localhost
    interval: 5s
brokerClusters:
  - id: kafka-main
    kind: kafka
    endpoint: localhost:19092
    auth: { mechanism: SCRAM-SHA-512, tls: true }
  - id: rabbit
    kind: rabbitmq
    endpoint: http://localhost:15672
`

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval.D() != 30*time.Second || c.Timeout.D() != DefaultTimeout {
		t.Fatalf("interval/timeout %v %v", c.Interval.D(), c.Timeout.D())
	}
	pg, rd := c.Databases[0], c.Databases[1]
	if pg.Port != 5432 || pg.Collector != "agent" || pg.Interval.D() != 30*time.Second || pg.TopQueries != DefaultTopN || pg.SlowQueryMs != DefaultSlowMs {
		t.Fatalf("pg defaults: %+v", pg)
	}
	if rd.Port != 6379 || rd.Interval.D() != 5*time.Second {
		t.Fatalf("redis defaults: %+v", rd)
	}
	if c.BrokerClusters[0].Auth.Mechanism != "scram-sha-512" {
		t.Fatalf("mechanism %q", c.BrokerClusters[0].Auth.Mechanism)
	}
	if c.BrokerClusters[1].Vhost != "/" {
		t.Fatalf("vhost %q", c.BrokerClusters[1].Vhost)
	}
	if pg.Key() != "orders-db" || pg.NodeName() != "orders-db.internal" {
		t.Fatalf("key/node %q %q", pg.Key(), pg.NodeName())
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"bad engine", "databases: [{id: a, engine: oracle, host: h}]", "engine"},
		{"no id", "databases: [{engine: redis, host: h}]", "id, slug or name"},
		{"no host", "databases: [{id: a, engine: redis}]", "host, dsn or credentialsRef"},
		{"duplicate", "databases: [{id: a, engine: redis, host: h}, {id: a, engine: redis, host: h}]", "duplicate"},
		{"bad kind", "brokerClusters: [{id: a, kind: nats, endpoint: x}]", "kind"},
		{"no endpoint", "brokerClusters: [{id: a, kind: kafka}]", "endpoint"},
		{"bad regexp", "brokerClusters: [{id: a, kind: kafka, endpoint: x, topicFilter: '('}]", "bad filter"},
		{"unknown field", "databases: [{id: a, engine: redis, host: h, pasword: x}]", "pasword"},
		{"bad duration", "interval: soon", "soon"},
		{"bad collector", "databases: [{id: a, engine: redis, host: h, collector: magic}]", "collector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want %q", err, tt.want)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	c, err := Parse(nil)
	if err != nil || c.Interval.D() != DefaultInterval {
		t.Fatalf("%v %+v", err, c)
	}
}

const apiJSON = `{
  "interval": 15,
  "databases": [
    {"id": 12, "slug": "orders-db", "name": "orders-db", "engine": "postgres", "host": "10.0.0.5", "port": 5433,
     "collector": "direct", "credentialsRef": "env:ORDERS_DB_DSN", "cloudResourceId": null, "env": "production", "createdAt": "2026-01-01T00:00:00Z"}
  ],
  "brokerClusters": [
    {"id": 3, "slug": "kafka-main", "name": "kafka-main", "kind": "kafka", "endpoint": "b-1:9096,b-2:9096", "vhost": null,
     "auth": {"mechanism": "scram-sha-512", "tls": true}, "credentialsRef": "file:/secrets/kafka", "collector": "agent"}
  ]
}`

func TestParseJSON(t *testing.T) {
	c, err := ParseJSON([]byte(apiJSON))
	if err != nil {
		t.Fatal(err)
	}
	d := c.Databases[0]
	if d.ID != "12" || d.Key() != "orders-db" || d.Port != 5433 || d.Collector != "direct" || c.Interval.D() != 15*time.Second {
		t.Fatalf("%+v", d)
	}
	b := c.BrokerClusters[0]
	if b.ID != "3" || !b.Auth.TLS || b.Auth.Mechanism != "scram-sha-512" {
		t.Fatalf("%+v", b)
	}
}

func TestResolveSecretAndConnString(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "pw")
	if err := os.WriteFile(f, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTCOL_TEST_DSN", "postgres://u:p@db:5432/app?sslmode=disable")
	t.Setenv("TTCOL_TEST_PW", "p@ss word")
	tests := []struct {
		name string
		db   Database
		want string
		err  string
	}{
		{"inline dsn wins", Database{Engine: "postgres", DSN: "postgres://x@h/db", CredentialsRef: "env:NOPE"}, "postgres://x@h/db", ""},
		{"secret is a dsn", Database{Engine: "postgres", Host: "ignored", CredentialsRef: "env:TTCOL_TEST_DSN"}, "postgres://u:p@db:5432/app?sslmode=disable", ""},
		{"postgres password from file", Database{Engine: "postgres", Host: "db", Port: 5432, CredentialsRef: "file:" + f},
			"postgres://trooper_monitor:s3cr3t@db:5432/postgres?application_name=trooper-agent&sslmode=prefer", ""},
		{"mysql password escaped as is", Database{Engine: "mysql", Host: "db", Port: 3306, CredentialsRef: "env:TTCOL_TEST_PW"},
			"trooper_monitor:p@ss word@tcp(db:3306)/", ""},
		{"redis without auth", Database{Engine: "redis", Host: "cache", Port: 6379}, "redis://cache:6379/0", ""},
		{"missing env", Database{Engine: "redis", Host: "h", CredentialsRef: "env:TTCOL_DOES_NOT_EXIST"}, "", "not set"},
		{"bad scheme", Database{Engine: "redis", Host: "h", CredentialsRef: "vault:x"}, "", "unsupported scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.db.ConnString()
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err=%v want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestBrokerCredentials(t *testing.T) {
	t.Setenv("TTCOL_MQ_PAIR", "alice:pw:with:colons")
	t.Setenv("TTCOL_MQ_PW", "justpw")
	tests := []struct {
		b          Broker
		user, pass string
	}{
		{Broker{CredentialsRef: "env:TTCOL_MQ_PAIR"}, "alice", "pw:with:colons"},
		{Broker{CredentialsRef: "env:TTCOL_MQ_PW"}, "trooper", "justpw"},
		{Broker{CredentialsRef: "env:TTCOL_MQ_PW", User: "bob"}, "bob", "justpw"},
		{Broker{}, "trooper", ""},
	}
	for _, tt := range tests {
		u, p, err := tt.b.Credentials()
		if err != nil || u != tt.user || p != tt.pass {
			t.Errorf("%+v: got %q %q %v", tt.b, u, p, err)
		}
	}
}

func TestMerge(t *testing.T) {
	static := &Config{Databases: []Database{{ID: "a", Host: "static"}}}
	remote := &Config{Databases: []Database{{ID: "a", Host: "remote"}, {ID: "b"}}, BrokerClusters: []Broker{{ID: "k"}}}
	m := Merge(static, remote)
	if len(m.Databases) != 2 || m.Databases[0].Host != "static" || len(m.BrokerClusters) != 1 {
		t.Fatalf("%+v", m)
	}
	if len(static.Databases) != 1 {
		t.Fatal("base mutated")
	}
	if Merge(nil, nil).Interval.D() != DefaultInterval {
		t.Fatal("nil merge")
	}
}

func TestFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RemotePath || r.Header.Get("Authorization") != "Bearer tt_dev_api_key" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(apiJSON))
	}))
	defer srv.Close()
	c, err := Fetcher{APIURL: srv.URL + "/", APIKey: "tt_dev_api_key"}.Fetch(context.Background())
	if err != nil || len(c.Databases) != 1 {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := (Fetcher{APIURL: srv.URL, APIKey: "bad"}).Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401, got %v", err)
	}
}

func TestExampleConfigParses(t *testing.T) {
	c, err := LoadFile("../../examples/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Databases) != 4 || len(c.BrokerClusters) != 4 {
		t.Fatalf("%d dbs %d brokers", len(c.Databases), len(c.BrokerClusters))
	}
}

func TestBrokerRound2Fields(t *testing.T) {
	c, err := Parse([]byte(`
brokerClusters:
  - id: msk
    kind: kafka
    endpoint: b-1.x.kafka.eu-west-1.amazonaws.com:9098
    auth: {mechanism: AWS-MSK-IAM, awsRegion: eu-west-1, roleArn: "arn:aws:iam::1:role/r"}
    nodes:
      - {name: kafka-1, brokerId: 1, metricsUrl: "http://kafka-1:9100/metrics", diskMount: /data}
  - id: oauth
    kind: kafka
    endpoint: k:9093
    auth: {mechanism: oauthbearer, tokenUrl: "https://idp/token", scopes: [kafka]}
databases:
  - {id: m, engine: mongodb, host: h, topQueriesSource: Profiler}
`))
	if err != nil {
		t.Fatal(err)
	}
	b := c.BrokerClusters[0]
	if b.Auth.Mechanism != "iam" || b.NodeMetrics != "auto" || b.TopicMetadataEvery.D() != 5*time.Minute ||
		len(b.Nodes) != 1 || *b.Nodes[0].BrokerID != 1 || b.Nodes[0].DiskMount != "/data" {
		t.Errorf("%+v", b)
	}
	if c.BrokerClusters[1].Auth.TokenURL == "" || c.Databases[0].TopQueriesSource != "profiler" || c.Databases[0].Port != 27017 {
		t.Errorf("%+v %+v", c.BrokerClusters[1], c.Databases[0])
	}
	if _, err := Parse([]byte(`brokerClusters: [{id: k, kind: kafka, endpoint: k:9092, nodes: [{name: x}]}]`)); err == nil ||
		!strings.Contains(err.Error(), "metricsUrl or brokerId") {
		t.Errorf("err=%v", err)
	}
}
