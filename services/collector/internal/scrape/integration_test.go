//go:build integration

package scrape

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/itest"
)

var ilog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

func parseOne(t *testing.T, y string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// scrapeTwice scrapes, runs load, waits, scrapes again and returns the second result.
func scrapeTwice(t *testing.T, s Scraper, load func()) emit.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Scrape(ctx); err != nil {
		t.Fatalf("first scrape: %v", err)
	}
	load()
	time.Sleep(1500 * time.Millisecond)
	r, err := s.Scrape(ctx)
	if err != nil {
		t.Fatalf("second scrape: %v", err)
	}
	return r
}

func gauge(t *testing.T, r emit.Result, name string) (float64, map[string]string) {
	t.Helper()
	for _, s := range r.Gauges {
		if s.Name == name {
			attrs := map[string]string{}
			for _, kv := range s.Attrs.ToSlice() {
				attrs[string(kv.Key)] = kv.Value.Emit()
			}
			return s.Value, attrs
		}
	}
	t.Fatalf("metric %s missing; have %v", name, names(r))
	return 0, nil
}

func names(r emit.Result) []string {
	var out []string
	for _, s := range r.Gauges {
		out = append(out, s.Name)
	}
	for _, s := range r.Counts {
		out = append(out, "Δ"+s.Name)
	}
	return out
}

func between(t *testing.T, name string, v, lo, hi float64) {
	t.Helper()
	if v < lo || v > hi {
		t.Errorf("%s = %v, want in [%v, %v]", name, v, lo, hi)
	}
}

func countWhere(r emit.Result, name, attr, contains string) (float64, bool) {
	for _, s := range r.Counts {
		if s.Name != name {
			continue
		}
		if v, ok := s.Attrs.Value(attributeKey(attr)); ok && strings.Contains(v.AsString(), contains) {
			return s.Value, true
		}
	}
	return 0, false
}

func TestPostgresIntegration(t *testing.T) {
	itest.SetupPostgres(t)
	t.Setenv("TTCOL_PG_PW", itest.MonitorPassword)
	c := parseOne(t, fmt.Sprintf(`databases: [{id: ttcol-pg, name: orders-db, engine: postgres, host: %s, port: %s,
  credentialsRef: "env:TTCOL_PG_PW", tls: disable, node: pg-1, slowQueryMs: 400}]`, itest.PostgresHost, itest.PostgresPort))
	s, err := NewDatabase(c.Databases[0], HostOptions{}, ilog)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := scrapeTwice(t, s, func() { itest.PostgresLoad(t, 50) })

	conns, attrs := gauge(t, r, emit.DBConnections)
	between(t, "connections", conns, 1, 100)
	if attrs["trooper.db.id"] != "ttcol-pg" || attrs["trooper.db.name"] != "orders-db" || attrs["db.system"] != "postgresql" ||
		attrs["trooper.db.node"] != "pg-1" || attrs["trooper.db.role"] != "primary" {
		t.Errorf("attrs %v", attrs)
	}
	maxc, _ := gauge(t, r, emit.DBMaxConnections)
	between(t, "max_connections", maxc, 10, 10000)
	ops, _ := gauge(t, r, emit.DBOps)
	between(t, "ops", ops, 1, 1e6)
	hr, _ := gauge(t, r, emit.DBHitRatio)
	between(t, "hit_ratio", hr, 0.5, 1)
	slow, _ := gauge(t, r, emit.DBSlow)
	between(t, "slow/min", slow, 1, 1e4) // the pg_sleep(0.6)
	p95, _ := gauge(t, r, emit.DBLatencyP95)
	between(t, "latency.p95", p95, 0, 10_000)

	calls, ok := countWhere(r, emit.DBQueryCalls, emit.AttrQueryText, "INSERT INTO ttcol_orders")
	if !ok || calls < 50 {
		t.Errorf("insert calls = %v, %v (have %v)", calls, ok, names(r))
	}
	for _, s := range r.Counts {
		if v, _ := s.Attrs.Value(emit.AttrQueryText); strings.Contains(v.AsString(), "INSERT INTO ttcol_orders") {
			if !strings.Contains(v.AsString(), "$1") {
				t.Errorf("query text not normalized: %q", v.AsString())
			}
			if h, _ := s.Attrs.Value(emit.AttrQueryHash); h.AsString() == "" {
				t.Error("missing query hash")
			}
		}
	}
	if _, ok := countWhere(r, emit.DBQueryTotalMs, emit.AttrQueryText, "pg_sleep"); !ok {
		t.Error("pg_sleep total_ms missing")
	}
}

func TestMySQLIntegration(t *testing.T) {
	itest.SetupMySQL(t)
	t.Setenv("TTCOL_MY_PW", itest.MonitorPassword)
	c := parseOne(t, fmt.Sprintf(`databases: [{id: ttcol-mysql, engine: mysql, host: localhost, port: %s, credentialsRef: "env:TTCOL_MY_PW"}]`, itest.MySQLPort))
	s, err := NewDatabase(c.Databases[0], HostOptions{}, ilog)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var closePS func()
	r := scrapeTwice(t, s, func() { itest.MySQLLoad(t, 50); closePS = itest.MySQLPreparedLoad(t, 25) })
	defer closePS()
	if calls, ok := countWhere(r, emit.DBQueryCalls, emit.AttrQueryText, "WHERE `total` = ? AND `id` >"); !ok || calls != 25 {
		t.Errorf("prepared statement calls = %v %v (%v)", calls, ok, names(r))
	}

	conns, attrs := gauge(t, r, emit.DBConnections)
	between(t, "connections", conns, 1, 1000)
	if attrs["db.system"] != "mysql" || attrs["trooper.db.role"] != "primary" {
		t.Errorf("attrs %v", attrs)
	}
	maxc, _ := gauge(t, r, emit.DBMaxConnections)
	between(t, "max_connections", maxc, 10, 100000)
	ops, _ := gauge(t, r, emit.DBOps)
	between(t, "ops", ops, 10, 1e6)
	hr, _ := gauge(t, r, emit.DBHitRatio)
	between(t, "hit_ratio", hr, 0.5, 1)
	if _, ok := countWhere(r, emit.DBQueryCalls, emit.AttrQueryText, "INSERT INTO `ttcol` . `orders`"); !ok {
		if _, ok := countWhere(r, emit.DBQueryCalls, emit.AttrQueryText, "INSERT INTO"); !ok {
			t.Errorf("insert digest missing: %v", names(r))
		}
	}
	p95, _ := gauge(t, r, emit.DBLatencyP95)
	between(t, "latency.p95", p95, 0, 10_000)
}

func TestRedisIntegration(t *testing.T) {
	cl := itest.RedisClient(t)
	defer cl.Close()
	host, port, _ := strings.Cut(itest.RedisAddr, ":")
	c := parseOne(t, fmt.Sprintf(`databases: [{id: ttcol-redis, engine: redis, host: %s, port: %s}]`, host, port))
	s, err := NewDatabase(c.Databases[0], HostOptions{Enabled: true}, ilog) // agent mode → host metrics too
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := scrapeTwice(t, s, func() { itest.RedisLoad(t, cl, 200) })

	conns, attrs := gauge(t, r, emit.DBConnections)
	between(t, "connections", conns, 1, 10000)
	if attrs["db.system"] != "redis" || attrs["trooper.db.role"] != "primary" {
		t.Errorf("attrs %v", attrs)
	}
	maxc, _ := gauge(t, r, emit.DBMaxConnections)
	between(t, "maxclients", maxc, 100, 1e6)
	ops, _ := gauge(t, r, emit.DBOps)
	between(t, "ops", ops, 50, 1e7)
	hr, _ := gauge(t, r, emit.DBHitRatio)
	between(t, "hit_ratio", hr, 0.5, 0.8) // 200 hits / 100 misses
	mem, _ := gauge(t, r, emit.DBMemory)
	between(t, "memory", mem, 0, 1)
	p95, _ := gauge(t, r, emit.DBLatencyP95)
	between(t, "ping p95 ms", p95, 0, 500)
	ev, _ := gauge(t, r, emit.DBEvictions)
	between(t, "evictions", ev, 0, 0)
	for _, m := range []string{emit.DBCPU, emit.DBMemory, emit.DBDisk} {
		v, _ := gauge(t, r, m)
		between(t, m, v, 0, 1)
	}
	if calls, ok := countWhere(r, emit.DBQueryCalls, emit.AttrQueryText, "SET"); !ok || calls < 200 {
		t.Errorf("SET calls = %v %v", calls, ok)
	}
}

func TestRabbitMQIntegration(t *testing.T) {
	itest.SetupRabbit(t)
	queue := fmt.Sprintf("ttcol-emails-%d", time.Now().UnixNano()%1e6)
	cleanup := itest.RabbitQueue(t, queue, 40, "ttcol-mailer-7f9c2a", "ttcol-mailer-a81b3c")
	defer cleanup()
	t.Setenv("TTCOL_RMQ_PW", itest.MonitorPassword)
	c := parseOne(t, fmt.Sprintf(`brokerClusters: [{id: ttcol-rabbit, kind: rabbitmq, endpoint: %q, credentialsRef: "env:TTCOL_RMQ_PW", topicFilter: "^ttcol-",
  nodes: [{metricsUrl: %q}]}]`, itest.RabbitMgmt, itest.RabbitProm))
	s, err := NewBroker(c.BrokerClusters[0], ilog)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// management stats refresh every 5 s
	var r emit.Result
	itest.Wait(t, "rabbitmq queue stats", 30*time.Second, func() error {
		var err error
		r, err = s.Scrape(context.Background())
		if err != nil {
			return err
		}
		if v, ok := find(r, emit.BrokerUnacked, map[string]string{emit.AttrDestination: queue}); !ok || v == 0 {
			return fmt.Errorf("unacked not visible yet (%v)", names(r))
		}
		return nil
	})
	match := map[string]string{emit.AttrDestination: queue, emit.AttrGroup: "ttcol-mailer", emit.AttrPartition: "0",
		emit.AttrMsgSystem: "rabbitmq", emit.AttrCluster: "ttcol-rabbit"}
	lag, ok := find(r, emit.BrokerLag, match)
	if !ok {
		t.Fatalf("lag missing: %v", r.Gauges)
	}
	between(t, "ready", lag, 30, 30) // 40 published, 2 consumers × prefetch 5 unacked
	unacked, _ := find(r, emit.BrokerUnacked, match)
	between(t, "unacked", unacked, 10, 10)
	members, ok := find(r, emit.BrokerGroupMembers, map[string]string{emit.AttrGroup: "ttcol-mailer"})
	if !ok || members != 2 {
		t.Errorf("members = %v %v", members, ok)
	}
	if _, ok := find(r, emit.BrokerNodeMemory, nil); !ok {
		t.Error("node memory missing")
	}
	age, ok := find(r, emit.BrokerOldestAgeS, match)
	if !ok {
		t.Errorf("oldest_age_s missing")
	}
	between(t, "oldest_age_s", age, 55, 120)
	ta := map[string]string{emit.AttrDestination: queue, emit.AttrMsgSystem: "rabbitmq", emit.AttrCluster: "ttcol-rabbit"}
	for m, want := range map[string][2]float64{
		emit.BrokerTopicPartitions: {1, 1}, emit.BrokerTopicReplication: {1, 1}, emit.BrokerTopicRetentionMs: {600000, 600000},
		emit.BrokerTopicSizeBytes: {400, 400}, emit.BrokerTopicAvgMsgBytes: {10, 10}, emit.BrokerTopicInRate: {0, 1e6},
	} {
		v, ok := find(r, m, ta)
		if !ok {
			t.Errorf("%s missing", m)
			continue
		}
		between(t, m, v, want[0], want[1])
	}
	// /api/nodes + rabbitmq_prometheus (node name from rabbitmq_identity_info)
	for _, m := range []string{emit.BrokerNodeDisk, emit.BrokerNodeFD, emit.BrokerNodeProc} {
		v, ok := find(r, m, nil)
		if !ok {
			t.Errorf("%s missing", m)
		}
		between(t, m, v, 0, 1)
	}
	r2, err := s.Scrape(context.Background()) // second Prometheus scrape → counter rates
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := find(r2, emit.BrokerNodeNetIn, nil); !ok || v < 0 {
		t.Errorf("rabbit net_in %v %v", v, ok)
	}
}

func TestKafkaIntegration(t *testing.T) {
	k := itest.SetupKafka(t, 30)
	defer k.Close()
	c := parseOne(t, fmt.Sprintf(`brokerClusters: [{id: ttcol-kafka, kind: kafka, endpoint: %q, topicFilter: "^%s$", groupFilter: "^%s$",
  nodes: [{name: rp-1, metricsUrl: %q}]}]`, itest.KafkaBrokers, k.Topic, k.Group, itest.KafkaMetrics))
	s, err := NewBroker(c.BrokerClusters[0], ilog)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := scrapeTwice(t, s, func() { k.Produce(t, 30, 1000) })

	var total, committedSum float64
	parts := 0
	for _, g := range r.Gauges {
		if g.Name != emit.BrokerLag {
			continue
		}
		parts++
		total += g.Value
		for _, kv := range g.Attrs.ToSlice() {
			switch string(kv.Key) {
			case emit.AttrCluster:
				if kv.Value.AsString() != "ttcol-kafka" {
					t.Errorf("cluster %v", kv.Value)
				}
			case emit.AttrDestination:
				if kv.Value.AsString() != k.Topic {
					t.Errorf("topic %v", kv.Value)
				}
			case emit.AttrGroup:
				if kv.Value.AsString() != k.Group {
					t.Errorf("group %v", kv.Value)
				}
			case emit.AttrMsgSystem:
				if kv.Value.AsString() != "kafka" {
					t.Errorf("system %v", kv.Value)
				}
			}
		}
	}
	for _, g := range r.Gauges {
		if g.Name == emit.BrokerCommittedOffset {
			committedSum += g.Value
		}
	}
	if parts != 3 {
		t.Errorf("lag partitions = %d want 3 (%v)", parts, names(r))
	}
	if committedSum < 1 || total+committedSum != 60 {
		t.Errorf("lag %v + committed %v != 60 produced", total, committedSum)
	}
	ta := map[string]string{emit.AttrDestination: k.Topic, emit.AttrMsgSystem: "kafka", emit.AttrCluster: "ttcol-kafka"}
	for m, want := range map[string][2]float64{
		emit.BrokerTopicPartitions: {3, 3}, emit.BrokerTopicReplication: {1, 1}, emit.BrokerTopicRetentionMs: {3600000, 3600000},
		emit.BrokerTopicInRate: {1, 1000}, emit.BrokerTopicSizeBytes: {30 * 1000, 1e6},
		emit.BrokerTopicAvgMsgBytes: {1000, 1500}, // on-disk: value + record/batch overhead
		emit.BrokerTopicBytesInRate: {1000, 1e6},
	} {
		v, ok := find(r, m, ta)
		if !ok {
			t.Errorf("%s missing (%v)", m, names(r))
			continue
		}
		between(t, m, v, want[0], want[1])
	}
	// Redpanda /public_metrics → node cpu/memory/disk/net_in
	for _, m := range []string{emit.BrokerNodeCPU, emit.BrokerNodeMemory, emit.BrokerNodeDisk} {
		v, ok := find(r, m, map[string]string{emit.AttrBrokerNode: "rp-1"})
		if !ok {
			t.Errorf("node %s missing", m)
		}
		between(t, m, v, 0, 1)
	}
	if v, ok := find(r, emit.BrokerNodeNetIn, map[string]string{emit.AttrBrokerNode: "rp-1"}); !ok || v <= 0 {
		t.Errorf("node net_in %v %v", v, ok)
	}
	members, ok := find(r, emit.BrokerGroupMembers, map[string]string{emit.AttrGroup: k.Group, emit.AttrGroupState: "Stable"})
	if !ok || members != 1 {
		t.Errorf("members = %v %v (%v)", members, ok, r.Gauges)
	}
	if reb, ok := find(r, emit.BrokerGroupRebalances, map[string]string{emit.AttrGroup: k.Group}); !ok || reb != 0 {
		t.Errorf("rebalances = %v %v", reb, ok)
	}
	if _, ok := find(r, emit.BrokerProduceRate, map[string]string{emit.AttrGroup: k.Group}); !ok {
		t.Error("produce_rate missing")
	}
	if _, ok := find(r, emit.BrokerEndOffset, map[string]string{emit.AttrPartition: "0"}); !ok {
		t.Error("end_offset missing")
	}
}

func TestMongoDBIntegration(t *testing.T) {
	cl := itest.SetupMongo(t)
	defer cl.Disconnect(context.Background())
	coll := fmt.Sprintf("orders_%d", time.Now().UnixNano()%1e6)
	defer cl.Database("ttcol").Collection(coll).Drop(context.Background())

	for _, src := range []string{"querystats", "profiler"} {
		t.Run(src, func(t *testing.T) {
			if src == "profiler" {
				itest.MongoProfile(t, cl, 2)
				defer itest.MongoProfile(t, cl, 0)
			}
			c := parseOne(t, fmt.Sprintf(`databases: [{id: ttcol-mongo, name: catalog-db, engine: mongodb, dsn: %q, node: mongo-1, topQueriesSource: %s, slowQueryMs: 0.0001}]`,
				itest.MongoURI, src))
			s, err := NewDatabase(c.Databases[0], HostOptions{}, ilog)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r := scrapeTwice(t, s, func() { itest.MongoLoad(t, cl, coll, 40) })

			conns, attrs := gauge(t, r, emit.DBConnections)
			between(t, "connections", conns, 1, 10000)
			if attrs["db.system"] != "mongodb" || attrs["trooper.db.role"] != "primary" || attrs["trooper.db.node"] != "mongo-1" {
				t.Errorf("attrs %v", attrs)
			}
			maxc, _ := gauge(t, r, emit.DBMaxConnections)
			between(t, "max_connections", maxc, 100, 1e7)
			ops, _ := gauge(t, r, emit.DBOps)
			between(t, "ops", ops, 10, 1e6)
			hr, _ := gauge(t, r, emit.DBHitRatio)
			between(t, "hit_ratio", hr, 0.5, 1)
			mem, _ := gauge(t, r, emit.DBMemory)
			between(t, "memory", mem, 0.0001, 1)
			p95, _ := gauge(t, r, emit.DBLatencyP95)
			between(t, "latency.p95", p95, 0, 5000)
			ev, _ := gauge(t, r, emit.DBEvictions)
			between(t, "evictions", ev, 0, 1e6)

			var found bool
			for _, cnt := range r.Counts {
				txt, _ := cnt.Attrs.Value(emit.AttrQueryText)
				if cnt.Name == emit.DBQueryCalls && strings.Contains(txt.AsString(), coll) && strings.Contains(txt.AsString(), "find") {
					found = true
					if cnt.Value != 40 {
						t.Errorf("find calls = %v want 40 (%s)", cnt.Value, txt.AsString())
					}
					if strings.Contains(txt.AsString(), "paid") {
						t.Errorf("literal in shape: %s", txt.AsString())
					}
					if h, _ := cnt.Attrs.Value(emit.AttrQueryHash); h.AsString() == "" {
						t.Error("missing hash")
					}
				}
			}
			if !found {
				t.Errorf("find shape missing: %v", names(r))
			}
			if src == "profiler" {
				slow, _ := gauge(t, r, emit.DBSlow)
				between(t, "slow/min", slow, 1, 1e7)
			}
		})
	}
}

// TestKafkaKIP848Integration needs an Apache Kafka 4.x broker (make itest-up starts ttcol-kafka4).
func TestKafkaKIP848Integration(t *testing.T) {
	if conn, err := net.DialTimeout("tcp", itest.Kafka4, 2*time.Second); err != nil {
		t.Skipf("no Kafka 4 broker at %s: %v", itest.Kafka4, err)
	} else {
		conn.Close()
	}
	k := itest.SetupKafkaAt(t, itest.Kafka4, 12, true)
	defer k.Close()
	c := parseOne(t, fmt.Sprintf(`brokerClusters: [{id: ttcol-kafka4, kind: kafka, endpoint: %q, topicFilter: "^%s$", groupFilter: "^%s$"}]`,
		itest.Kafka4, k.Topic, k.Group))
	s, err := NewBroker(c.BrokerClusters[0], ilog)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var r emit.Result
	itest.Wait(t, "kip-848 group stable", 30*time.Second, func() error {
		var err error
		if r, err = s.Scrape(context.Background()); err != nil {
			return err
		}
		if _, ok := find(r, emit.BrokerGroupMembers, map[string]string{emit.AttrGroup: k.Group, emit.AttrGroupState: "Stable"}); !ok {
			return fmt.Errorf("group not stable yet: %v", r.Gauges)
		}
		return nil
	})
	gv, err := s.(*Kafka).describeGroups(context.Background(), []string{k.Group}, time.Now())
	if err != nil || len(gv) != 1 || !strings.HasPrefix(gv[0].Generation, "epoch:") {
		t.Errorf("group not described via ConsumerGroupDescribe: %+v %v", gv, err)
	}
	if m, _ := find(r, emit.BrokerGroupMembers, map[string]string{emit.AttrGroup: k.Group}); m != 1 {
		t.Errorf("members = %v", m)
	}
	var lagSum float64
	parts := 0
	for _, g := range r.Gauges {
		if g.Name == emit.BrokerLag {
			parts++
			lagSum += g.Value
		}
	}
	if parts != 3 || lagSum < 0 || lagSum > 12 {
		t.Errorf("lag partitions %d sum %v", parts, lagSum)
	}
	if v, ok := find(r, emit.BrokerNodeDisk, map[string]string{emit.AttrBrokerNode: "broker-1"}); !ok || v <= 0 || v > 1 {
		t.Errorf("log-dir disk %v %v (%v)", v, ok, names(r))
	}
	if v, ok := find(r, emit.BrokerTopicRetentionMs, map[string]string{emit.AttrDestination: k.Topic}); !ok || v != 3600000 {
		t.Errorf("retention %v %v", v, ok)
	}
}
