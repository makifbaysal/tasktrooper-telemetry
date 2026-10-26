//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/itest"
	"github.com/tasktrooper/telemetry/services/collector/internal/otlptest"
)

// TestAgentOnceEndToEnd runs the agent exactly like `trooper-agent --once --config … --dsn …`
// against real servers and an in-process OTLP/HTTP receiver, then checks the wire output:
// metric names, kinds, attributes, resource and auth header.
func TestAgentOnceEndToEnd(t *testing.T) {
	itest.SetupPostgres(t)
	itest.SetupMySQL(t)
	itest.SetupRabbit(t)
	rc := itest.RedisClient(t)
	defer rc.Close()
	itest.RedisLoad(t, rc, 20) // keyspace hits/misses exist even on a fresh server
	mc := itest.SetupMongo(t)
	defer mc.Disconnect(context.Background())
	itest.MongoLoad(t, mc, "e2e_orders", 5)
	k := itest.SetupKafka(t, 12)
	defer k.Close()
	queue := fmt.Sprintf("ttcol-e2e-%d", time.Now().UnixNano()%1e6)
	defer itest.RabbitQueue(t, queue, 10, "ttcol-e2e-worker-1")()

	t.Setenv("TTCOL_E2E_PW", itest.MonitorPassword)
	t.Setenv("TTCOL_E2E_MQ", "trooper:"+itest.MonitorPassword)
	redisHost, redisPort, _ := strings.Cut(itest.RedisAddr, ":")
	cfg := fmt.Sprintf(`
interval: 15s
timeout: 10s
databases:
  - {id: ttcol-orders-db, name: orders-db, engine: postgres, host: localhost, port: %s, tls: disable, credentialsRef: "env:TTCOL_E2E_PW", node: pg-1}
  - {id: ttcol-mysql, engine: mysql, host: localhost, port: %s, credentialsRef: "env:TTCOL_E2E_PW"}
  - {id: ttcol-cache, name: session-cache, engine: redis, host: %s, port: %s}
  - {id: ttcol-mongo, name: catalog-db, engine: mongodb, dsn: %q}
brokerClusters:
  - {id: ttcol-kafka, kind: kafka, endpoint: %q, topicFilter: "^%s$", groupFilter: "^%s$", nodes: [{name: rp-1, metricsUrl: %q}]}
  - {id: ttcol-rabbit, kind: rabbitmq, endpoint: %q, credentialsRef: "env:TTCOL_E2E_MQ", topicFilter: "^ttcol-e2e-", nodes: [{metricsUrl: %q}]}
`, itest.PostgresPort, itest.MySQLPort, redisHost, redisPort, itest.MongoURI, itest.KafkaBrokers, k.Topic, k.Group, itest.KafkaMetrics,
		itest.RabbitMgmt, itest.RabbitProm)
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := otlptest.New()
	defer srv.Close()

	f, err := parseFlags([]string{"--config", path, "--dsn", "http://tt_dev_ingest_key@" + srv.Listener.Addr().String() + "/1",
		"--once", "--once-gap", "6s", "--host-metrics"})
	if err != nil {
		t.Fatal(err)
	}
	// keep stdout clean-ish: the JSON dump goes to stdout, which go test captures
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, f, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("run: %v", err)
	}

	if keys := srv.Keys(); len(keys) == 0 || keys[0] != "tt_dev_ingest_key" {
		t.Fatalf("x-trooper-key %v", keys)
	}
	points := srv.Points()
	if len(points) == 0 {
		t.Fatal("no points received")
	}
	known := append(slices.Clone(emit.GaugeNames), emit.CounterNames...)
	for _, p := range points {
		if !slices.Contains(known, p.Metric) {
			t.Errorf("non-contract metric %s", p.Metric)
		}
		if p.ServiceName != "trooper-agent" {
			t.Errorf("service.name %q", p.ServiceName)
		}
	}

	// every target up
	for _, id := range []string{"ttcol-orders-db", "ttcol-mysql", "ttcol-cache", "ttcol-mongo", "ttcol-kafka", "ttcol-rabbit"} {
		up := srv.Find(emit.CollectorUp, map[string]string{emit.AttrTarget: id})
		if len(up) == 0 || up[len(up)-1].Value != 1 {
			t.Errorf("%s not up: %+v", id, up)
		}
	}

	dbAttrKeys := []string{emit.AttrDBID, emit.AttrDBName, emit.AttrDBSystem, emit.AttrDBNode, emit.AttrDBRole}
	for _, c := range []struct {
		id, system string
		metrics    []string
	}{
		{"ttcol-orders-db", "postgresql", []string{emit.DBConnections, emit.DBMaxConnections, emit.DBOps, emit.DBHitRatio, emit.DBSlow, emit.DBLatencyP95}},
		{"ttcol-mysql", "mysql", []string{emit.DBConnections, emit.DBMaxConnections, emit.DBOps, emit.DBHitRatio, emit.DBSlow}},
		{"ttcol-cache", "redis", []string{emit.DBConnections, emit.DBMaxConnections, emit.DBOps, emit.DBHitRatio, emit.DBMemory, emit.DBLatencyP95, emit.DBEvictions}},
		{"ttcol-mongo", "mongodb", []string{emit.DBConnections, emit.DBMaxConnections, emit.DBOps, emit.DBHitRatio, emit.DBMemory, emit.DBEvictions}},
	} {
		for _, m := range c.metrics {
			ps := srv.Find(m, map[string]string{emit.AttrDBID: c.id, emit.AttrDBSystem: c.system})
			if len(ps) == 0 {
				t.Errorf("%s: %s missing", c.id, m)
				continue
			}
			for _, k := range dbAttrKeys {
				if ps[0].Attrs[k] == "" {
					t.Errorf("%s %s: attribute %s empty (%v)", c.id, m, k, ps[0].Attrs)
				}
			}
			if ps[0].Kind != "gauge" {
				t.Errorf("%s %s kind %s", c.id, m, ps[0].Kind)
			}
		}
	}
	// host metrics for agent-mode databases
	for _, m := range []string{emit.DBCPU, emit.DBMemory, emit.DBDisk} {
		if len(srv.Find(m, map[string]string{emit.AttrDBID: "ttcol-orders-db"})) == 0 {
			t.Errorf("host metric %s missing", m)
		}
	}
	// query metrics: delta monotonic sums with hash + normalized text
	calls := srv.Find(emit.DBQueryCalls, map[string]string{emit.AttrDBID: "ttcol-orders-db"})
	if len(calls) == 0 {
		t.Fatal("no query calls for postgres")
	}
	for _, p := range calls {
		if p.Kind != "sum" || !p.Delta || !p.Monotonic || p.Attrs[emit.AttrQueryHash] == "" || p.Attrs[emit.AttrQueryText] == "" {
			t.Errorf("bad query point %+v", p)
		}
	}
	if len(srv.Find(emit.DBQueryP95Ms, map[string]string{emit.AttrDBID: "ttcol-orders-db"})) == 0 {
		t.Error("query p95 missing")
	}

	lagAttrs := []string{emit.AttrCluster, emit.AttrMsgSystem, emit.AttrDestination, emit.AttrGroup, emit.AttrPartition}
	for _, c := range []struct{ cluster, system, topic, group string }{
		{"ttcol-kafka", "kafka", k.Topic, k.Group},
		{"ttcol-rabbit", "rabbitmq", queue, "ttcol-e2e-worker"},
	} {
		ps := srv.Find(emit.BrokerLag, map[string]string{emit.AttrCluster: c.cluster, emit.AttrMsgSystem: c.system, emit.AttrDestination: c.topic, emit.AttrGroup: c.group})
		if len(ps) == 0 {
			t.Errorf("%s: lag missing", c.cluster)
			continue
		}
		for _, k := range lagAttrs {
			if _, ok := ps[0].Attrs[k]; !ok {
				t.Errorf("%s lag: attribute %s missing", c.cluster, k)
			}
		}
		if len(srv.Find(emit.BrokerGroupMembers, map[string]string{emit.AttrCluster: c.cluster, emit.AttrGroup: c.group})) == 0 {
			t.Errorf("%s: group members missing", c.cluster)
		}
		if g := srv.Find(emit.BrokerGroupMembers, map[string]string{emit.AttrCluster: c.cluster, emit.AttrGroup: c.group}); len(g) > 0 && g[0].Attrs[emit.AttrGroupState] == "" {
			t.Errorf("%s: group state missing", c.cluster)
		}
	}
	if len(srv.Find(emit.BrokerProduceRate, map[string]string{emit.AttrCluster: "ttcol-kafka"})) == 0 {
		t.Error("kafka produce_rate missing")
	}
	if len(srv.Find(emit.BrokerGroupRebalances, map[string]string{emit.AttrCluster: "ttcol-kafka"})) == 0 {
		t.Error("kafka rebalances missing")
	}
	if len(srv.Find(emit.BrokerUnacked, map[string]string{emit.AttrCluster: "ttcol-rabbit"})) == 0 {
		t.Error("rabbit unacked missing")
	}
	// round 2: topic / queue metadata, queue age, broker node metrics
	topicAttrs := []string{emit.AttrCluster, emit.AttrMsgSystem, emit.AttrDestination}
	for _, c := range []struct {
		cluster, system, dest string
		metrics               []string
	}{
		{"ttcol-kafka", "kafka", k.Topic, []string{emit.BrokerTopicPartitions, emit.BrokerTopicReplication, emit.BrokerTopicRetentionMs,
			emit.BrokerTopicSizeBytes, emit.BrokerTopicInRate, emit.BrokerTopicAvgMsgBytes, emit.BrokerTopicBytesInRate}},
		{"ttcol-rabbit", "rabbitmq", queue, []string{emit.BrokerTopicPartitions, emit.BrokerTopicReplication, emit.BrokerTopicRetentionMs,
			emit.BrokerTopicSizeBytes, emit.BrokerTopicInRate, emit.BrokerTopicAvgMsgBytes, emit.BrokerTopicBytesInRate}},
	} {
		for _, m := range c.metrics {
			ps := srv.Find(m, map[string]string{emit.AttrCluster: c.cluster, emit.AttrMsgSystem: c.system, emit.AttrDestination: c.dest})
			if len(ps) == 0 {
				t.Errorf("%s: %s missing", c.cluster, m)
				continue
			}
			for _, a := range topicAttrs {
				if ps[0].Attrs[a] == "" {
					t.Errorf("%s %s: attribute %s empty", c.cluster, m, a)
				}
			}
			if ps[0].Kind != "gauge" {
				t.Errorf("%s %s kind %s", c.cluster, m, ps[0].Kind)
			}
		}
	}
	if len(srv.Find(emit.BrokerOldestAgeS, map[string]string{emit.AttrCluster: "ttcol-rabbit", emit.AttrDestination: queue})) == 0 {
		t.Error("rabbit oldest_age_s missing")
	}
	for _, c := range []struct{ cluster, node string }{{"ttcol-kafka", "rp-1"}, {"ttcol-rabbit", ""}} {
		for _, m := range []string{emit.BrokerNodeCPU, emit.BrokerNodeMemory, emit.BrokerNodeDisk, emit.BrokerNodeNetIn} {
			if c.cluster == "ttcol-rabbit" && m == emit.BrokerNodeCPU {
				continue // needs Erlang microstate accounting
			}
			match := map[string]string{emit.AttrCluster: c.cluster}
			if c.node != "" {
				match[emit.AttrBrokerNode] = c.node
			}
			if len(srv.Find(m, match)) == 0 {
				t.Errorf("%s: node %s missing", c.cluster, m)
			}
		}
	}
	t.Logf("received %d points", len(points))
}
