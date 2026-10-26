package gcp

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sqlJSON = `{"items":[
 {"name":"orders-db","databaseVersion":"POSTGRES_16","region":"europe-west1","gceZone":"europe-west1-b","state":"RUNNABLE","instanceType":"CLOUD_SQL_INSTANCE",
  "connectionName":"shop-prod:europe-west1:orders-db","ipAddresses":[{"ipAddress":"10.0.0.3","type":"PRIVATE"},{"ipAddress":"34.1.2.3","type":"PRIMARY"}],
  "settings":{"tier":"db-custom-2-7680","dataDiskSizeGb":"100","availabilityType":"REGIONAL"}},
 {"name":"orders-db-replica","databaseVersion":"POSTGRES_16","region":"europe-west1","instanceType":"READ_REPLICA_INSTANCE","masterInstanceName":"shop-prod:orders-db",
  "ipAddresses":[{"ipAddress":"10.0.0.4","type":"PRIVATE"}],"settings":{"tier":"db-custom-2-7680","dataDiskSizeGb":"100"}},
 {"name":"legacy","databaseVersion":"MYSQL_8_0","region":"us-central1","instanceType":"CLOUD_SQL_INSTANCE","settings":{"tier":"db-n1-standard-1"}},
 {"name":"mssql","databaseVersion":"SQLSERVER_2019_STANDARD","region":"us-central1"}]}`

const redisJSON = `{"instances":[{"name":"projects/shop-prod/locations/europe-west1/instances/cache","host":"10.1.0.5","port":6379,"tier":"STANDARD_HA",
 "memorySizeGb":5,"redisVersion":"REDIS_7_0","locationId":"europe-west1-c","state":"READY","replicaCount":1}]}`

const subsPage1 = `{"subscriptions":[{"name":"projects/shop-prod/subscriptions/orders-worker","topic":"projects/shop-prod/topics/orders","ackDeadlineSeconds":30,"messageRetentionDuration":"604800s"}],"nextPageToken":"p2"}`
const subsPage2 = `{"subscriptions":[{"name":"projects/shop-prod/subscriptions/billing","topic":"projects/shop-prod/topics/orders","ackDeadlineSeconds":10}]}`

// series builds a timeSeries.list answer; points are newest first.
func series(label string, pts map[string][]string) string {
	var b strings.Builder
	b.WriteString(`{"timeSeries":[`)
	i := 0
	for k, vals := range pts {
		if i > 0 {
			b.WriteString(",")
		}
		i++
		fmt.Fprintf(&b, `{"resource":{"labels":{%q:%q}},"points":[`, label, k)
		for j, v := range vals {
			if j > 0 {
				b.WriteString(",")
			}
			end := time.Date(2026, 10, 25, 12, 4-j, 0, 0, time.UTC).Format(time.RFC3339)
			fmt.Fprintf(&b, `{"interval":{"endTime":%q},"value":%s}`, end, v)
		}
		b.WriteString("]}")
	}
	b.WriteString("]}")
	return b.String()
}

func d(v float64) string { return fmt.Sprintf(`{"doubleValue":%v}`, v) }
func n(v int) string     { return fmt.Sprintf(`{"int64Value":"%d"}`, v) }

var monitoring = map[string]string{
	"cloudsql.googleapis.com/database/cpu/utilization":              series("database_id", map[string][]string{"shop-prod:orders-db": {d(0.42), d(0.1)}, "shop-prod:legacy": {d(0.05)}}),
	"cloudsql.googleapis.com/database/memory/utilization":           series("database_id", map[string][]string{"shop-prod:orders-db": {d(0.7)}}),
	"cloudsql.googleapis.com/database/disk/utilization":             series("database_id", map[string][]string{"shop-prod:orders-db": {d(0.31)}}),
	"cloudsql.googleapis.com/database/postgresql/num_backends":      series("database_id", map[string][]string{"shop-prod:orders-db": {n(57)}}),
	"cloudsql.googleapis.com/database/network/connections":          series("database_id", map[string][]string{"shop-prod:legacy": {n(9)}}),
	"cloudsql.googleapis.com/database/replication/replica_lag":      series("database_id", map[string][]string{"shop-prod:orders-db-replica": {d(1.5)}}),
	"cloudsql.googleapis.com/database/postgresql/transaction_count": series("database_id", map[string][]string{"shop-prod:orders-db": {d(120.5)}}),
	"redis.googleapis.com/stats/cpu_utilization":                    series("instance_id", map[string][]string{"projects/shop-prod/locations/europe-west1/instances/cache": {d(0.25)}}),
	"redis.googleapis.com/stats/memory/usage_ratio":                 series("instance_id", map[string][]string{"projects/shop-prod/locations/europe-west1/instances/cache": {d(0.6)}}),
	"redis.googleapis.com/clients/connected":                        series("instance_id", map[string][]string{"projects/shop-prod/locations/europe-west1/instances/cache": {n(14)}}),
	"redis.googleapis.com/stats/evicted_keys":                       series("instance_id", map[string][]string{"projects/shop-prod/locations/europe-west1/instances/cache": {n(3)}}),
	"redis.googleapis.com/stats/cache_hit_ratio":                    series("instance_id", map[string][]string{"projects/shop-prod/locations/europe-west1/instances/cache": {d(0.93)}}),
	"pubsub.googleapis.com/subscription/num_undelivered_messages":   series("subscription_id", map[string][]string{"orders-worker": {n(1200)}, "billing": {n(0)}}),
	"pubsub.googleapis.com/subscription/oldest_unacked_message_age": series("subscription_id", map[string][]string{"orders-worker": {n(95)}}),
	"pubsub.googleapis.com/subscription/ack_message_count":          series("subscription_id", map[string][]string{"orders-worker": {d(40)}}),
	"pubsub.googleapis.com/topic/send_message_operation_count":      series("topic_id", map[string][]string{"orders": {d(45)}}),
}

func fakeGoogle(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/sql/projects/shop-prod/instances":
			_, _ = io.WriteString(w, sqlJSON)
		case "/redis/projects/shop-prod/locations/-/instances":
			_, _ = io.WriteString(w, redisJSON)
		case "/pubsub/projects/shop-prod/subscriptions":
			if q.Get("pageToken") == "p2" {
				_, _ = io.WriteString(w, subsPage2)
			} else {
				_, _ = io.WriteString(w, subsPage1)
			}
		case "/mon/projects/shop-prod/timeSeries":
			f := q.Get("filter")
			mt := strings.TrimSuffix(strings.TrimPrefix(f, `metric.type = "`), `"`)
			if q.Get("aggregation.alignmentPeriod") != "60s" || q.Get("interval.endTime") != "2026-10-25T12:05:00Z" {
				t.Errorf("bad query %s", r.URL.RawQuery)
			}
			if body, ok := monitoring[mt]; ok {
				_, _ = io.WriteString(w, body)
			} else {
				_, _ = io.WriteString(w, `{}`)
			}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

func TestDiscoverAndCollect(t *testing.T) {
	srv := fakeGoogle(t)
	defer srv.Close()
	p, err := New(context.Background(), []byte(`{"projectIds":["shop-prod"],"consumerMap":{"billing":"billing-svc"}}`), nil,
		Endpoints{SQLAdmin: srv.URL + "/sql", Redis: srv.URL + "/redis", PubSub: srv.URL + "/pubsub", Monitoring: srv.URL + "/mon", NoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Databases) != 4 || len(inv.Brokers) != 1 {
		t.Fatalf("dbs %d brokers %d", len(inv.Databases), len(inv.Brokers))
	}
	dbt := []struct {
		i                                 int
		name, engine, host, source, class string
		port                              int
		disk                              float64
	}{
		{0, "orders-db", "postgres", "34.1.2.3", "Cloud SQL · europe-west1", "db-custom-2-7680", 5432, 100},
		{1, "orders-db-replica", "postgres", "10.0.0.4", "Cloud SQL · europe-west1", "db-custom-2-7680", 5432, 100},
		{2, "legacy", "mysql", "", "Cloud SQL · us-central1", "db-n1-standard-1", 3306, 0},
		{3, "cache", "redis", "10.1.0.5", "Memorystore · europe-west1", "STANDARD_HA 5 GB", 6379, 0},
	}
	for _, tt := range dbt {
		got := inv.Databases[tt.i]
		if got.Name != tt.name || got.Engine != tt.engine || got.Host != tt.host || got.Source != tt.source || got.InstanceClass != tt.class || got.Port != tt.port {
			t.Errorf("%d: %+v", tt.i, got)
		}
		if (tt.disk == 0) != (got.DiskGB == nil) {
			t.Errorf("%d disk %v", tt.i, got.DiskGB)
		}
	}
	b := inv.Brokers[0]
	if b.Name != "pubsub-shop-prod" || b.Kind != "pubsub" || b.Meta["subscriptionCount"] != 2 {
		t.Fatalf("broker %+v", b)
	}

	res, err := p.Collect(context.Background(), inv, time.Date(2026, 10, 25, 12, 5, 20, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range res.Samples {
		key := s.Name + "|" + s.Attrs["trooper.db.id"] + s.Attrs["messaging.consumer.group.name"]
		if s.Attrs["trooper.db.role"] != "" {
			key += "|" + s.Attrs["trooper.db.role"]
		}
		got[key] = s.Value
		if s.TS != time.Date(2026, 10, 25, 12, 4, 0, 0, time.UTC) {
			t.Errorf("%s ts %v", s.Name, s.TS)
		}
	}
	want := map[string]float64{
		"trooper.db.cpu|gcp-orders-db|primary":                     0.42,
		"trooper.db.memory|gcp-orders-db|primary":                  0.7,
		"trooper.db.disk|gcp-orders-db|primary":                    0.31,
		"trooper.db.connections|gcp-orders-db|primary":             57,
		"trooper.db.ops|gcp-orders-db|primary":                     120.5,
		"trooper.db.replication_lag|gcp-orders-db-replica|replica": 1500,
		"trooper.db.cpu|gcp-legacy|primary":                        0.05,
		"trooper.db.connections|gcp-legacy|primary":                9,
		"trooper.db.cpu|gcp-cache|primary":                         0.25,
		"trooper.db.memory|gcp-cache|primary":                      0.6,
		"trooper.db.connections|gcp-cache|primary":                 14,
		"trooper.db.evictions|gcp-cache|primary":                   3,
		"trooper.db.hit_ratio|gcp-cache|primary":                   0.93,
		"trooper.broker.lag|orders-worker":                         1200,
		"trooper.broker.lag|billing-svc":                           0,
		"trooper.broker.oldest_age_s|orders-worker":                95,
		"trooper.broker.consume_rate|orders-worker":                40,
		"trooper.broker.produce_rate|orders-worker":                45,
		"trooper.broker.produce_rate|billing-svc":                  45,
		"trooper.broker.topic.in_rate|":                            45,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v (ok=%v), want %v", k, g, ok, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d samples, want %d: %v", len(got), len(want), got)
	}
	for _, s := range res.Samples {
		if s.Name == "trooper.broker.lag" && (s.Attrs["trooper.broker.cluster"] != "gcp-pubsub-shop-prod" || s.Attrs["messaging.system"] != "gcp_pubsub" || s.Attrs["messaging.destination.name"] != "orders") {
			t.Errorf("lag attrs %v", s.Attrs)
		}
	}
}

func TestRegionOf(t *testing.T) {
	for in, want := range map[string]string{"europe-west1-b": "europe-west1", "us-central1": "us-central1", "global": "global"} {
		if got := regionOf(in); got != want {
			t.Errorf("%s → %s", in, got)
		}
	}
}
