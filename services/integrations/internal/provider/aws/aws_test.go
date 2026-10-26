package aws

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/fakes"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// cloudwatchFixture is a recorded-shape set of CloudWatch values keyed by metric + identifying dimension.
func cloudwatchFixture(name string, d map[string]string) (float64, bool) {
	id := d["DBInstanceIdentifier"] + d["CacheClusterId"] + d["QueueName"] + d["Broker ID"] + d["Consumer Group"]
	v, ok := map[string]float64{
		"CPUUtilization/orders-db":                       37,
		"FreeableMemory/orders-db":                       4 * (1 << 30), // of 16 GiB (db.r6g.large)
		"FreeStorageSpace/orders-db":                     25 * (1 << 30),
		"DatabaseConnections/orders-db":                  42,
		"ReadIOPS/orders-db":                             100,
		"WriteIOPS/orders-db":                            50,
		"ReplicaLag/orders-db-ro":                        2.5,
		"CPUUtilization/users-aurora-2":                  10,
		"AuroraReplicaLag/users-aurora-2":                18,
		"EngineCPUUtilization/sessions-001":              12,
		"DatabaseMemoryUsagePercentage/sessions-001":     55,
		"CurrConnections/sessions-001":                   300,
		"Evictions/sessions-001":                         7,
		"CacheHitRate/sessions-001":                      98,
		"GetTypeCmds/sessions-001":                       6000,
		"SetTypeCmds/sessions-001":                       1200,
		"CpuUser/1":                                      20,
		"CpuSystem/1":                                    5,
		"MemoryUsed/1":                                   3,
		"MemoryFree/1":                                   1,
		"KafkaDataLogsDiskUsed/1":                        40,
		"BytesInPerSec/1":                                2_500_000,
		"SumOffsetLag/billing":                           1234,
		"ApproximateNumberOfMessagesVisible/emails":      80,
		"ApproximateNumberOfMessagesNotVisible/emails":   4,
		"ApproximateAgeOfOldestMessage/emails":           65,
		"NumberOfMessagesSent/emails":                    120,
		"NumberOfMessagesDeleted/emails":                 90,
		"ApproximateNumberOfMessagesVisible/orders.fifo": 0,
	}[name+"/"+id]
	return v, ok
}

func TestDiscoverAndCollectAgainstFakeAWS(t *testing.T) {
	ts := time.Date(2026, 10, 25, 12, 4, 0, 0, time.UTC)
	f := fakes.NewAWS(t, ts, cloudwatchFixture)
	defer f.Close()
	p, err := New(context.Background(), []byte(`{"regions":["eu-central-1"],"sqsConsumers":{"emails":"mailer"}}`), nil, Options{Endpoint: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range inv.Databases {
		names = append(names, fmt.Sprintf("%s:%s:%s:%d:%s", d.Name, d.Engine, d.Host, d.Port, d.Source))
	}
	for _, b := range inv.Brokers {
		names = append(names, fmt.Sprintf("%s:%s:%s:%s", b.Name, b.Kind, b.Endpoint, b.Source))
	}
	want := []string{
		"orders-db:postgres:orders-db.abc.eu-central-1.rds.amazonaws.com:5432:AWS RDS · eu-central-1",
		"orders-db-ro:postgres:orders-db-ro.abc.eu-central-1.rds.amazonaws.com:5432:AWS RDS · eu-central-1",
		"users-aurora-2:mysql:users-aurora-2.abc.rds.amazonaws.com:3306:Amazon Aurora · eu-central-1",
		"sessions:redis:sessions.abc.cache.amazonaws.com:6379:Amazon ElastiCache · eu-central-1",
		"events:kafka:b-1.events.kafka.eu-central-1.amazonaws.com:9098:Amazon MSK · eu-central-1",
		"sqs-eu-central-1:sqs:https://sqs.eu-central-1.amazonaws.com:Amazon SQS · eu-central-1",
	}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("inventory\n got %v\nwant %v", names, want)
	}
	if d := inv.Databases[0]; d.DiskGB == nil || *d.DiskGB != 100 || d.InstanceClass != "db.r6g.large" || d.Version != "16.3" || d.Zone != "eu-central-1a" {
		t.Errorf("rds row %+v", d)
	}
	if inv.Databases[2].DiskGB != nil {
		t.Error("aurora must not report allocated storage")
	}
	if b := inv.Brokers[0]; b.Version != "3.6.0" {
		t.Errorf("msk %+v", b)
	}

	res, err := p.Collect(context.Background(), inv, ts.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range res.Samples {
		if !s.TS.Equal(ts) || s.Region != "eu-central-1" {
			t.Errorf("%s ts %v region %q", s.Name, s.TS, s.Region)
		}
		id := s.Attrs["trooper.db.node"] + s.Attrs["trooper.broker.node"] + s.Attrs["messaging.consumer.group.name"]
		if s.Attrs["trooper.db.role"] != "" {
			id += "/" + s.Attrs["trooper.db.role"]
		}
		if s.Name == model.TopicInRate || s.Name == model.TopicRetentionMs {
			id = s.Attrs["messaging.destination.name"]
		}
		got[s.Name+"|"+id] = s.Value
	}
	exp := map[string]float64{
		"trooper.db.cpu|orders-db/primary":                  0.37,
		"trooper.db.memory|orders-db/primary":               0.75,
		"trooper.db.disk|orders-db/primary":                 0.75,
		"trooper.db.connections|orders-db/primary":          42,
		"trooper.db.ops|orders-db/primary":                  150,
		"trooper.db.replication_lag|orders-db-ro/replica":   2500,
		"trooper.db.cpu|users-aurora-2/replica":             0.1,
		"trooper.db.replication_lag|users-aurora-2/replica": 18,
		"trooper.db.cpu|sessions-001/primary":               0.12,
		"trooper.db.memory|sessions-001/primary":            0.55,
		"trooper.db.connections|sessions-001/primary":       300,
		"trooper.db.evictions|sessions-001/primary":         7,
		"trooper.db.hit_ratio|sessions-001/primary":         0.98,
		"trooper.db.ops|sessions-001/primary":               120,
		"trooper.broker.node.cpu|1":                         0.25,
		"trooper.broker.node.memory|1":                      0.75,
		"trooper.broker.node.disk|1":                        0.4,
		"trooper.broker.node.net_in|1":                      2.5,
		"trooper.broker.lag|billing":                        1234,
		"trooper.broker.lag|mailer":                         80,
		"trooper.broker.unacked|mailer":                     4,
		"trooper.broker.oldest_age_s|mailer":                65,
		"trooper.broker.produce_rate|mailer":                2,
		"trooper.broker.consume_rate|mailer":                1.5,
		"trooper.broker.topic.in_rate|emails":               2,
		"trooper.broker.topic.retention_ms|emails":          86400000,
		"trooper.broker.lag|orders.fifo":                    0,
	}
	var keys []string
	for k, v := range exp {
		if g, ok := got[k]; !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v (ok=%v), want %v", k, g, ok, v)
		}
		keys = append(keys, k)
	}
	if len(got) != len(exp) {
		var extra []string
		for k := range got {
			if _, ok := exp[k]; !ok {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		t.Errorf("unexpected samples: %v", extra)
	}
	for _, s := range res.Samples {
		switch s.Name {
		case model.BrokerLag:
			if s.Attrs["messaging.destination.partition.id"] != "0" {
				t.Errorf("partition %v", s.Attrs)
			}
			if s.Attrs["messaging.consumer.group.name"] == "mailer" && (s.Attrs["trooper.broker.cluster"] != "aws-sqs-eu-central-1" || s.Attrs["messaging.system"] != "aws_sqs") {
				t.Errorf("sqs attrs %v", s.Attrs)
			}
			if s.Attrs["messaging.consumer.group.name"] == "billing" && (s.Attrs["trooper.broker.cluster"] != "aws-events" || s.Attrs["messaging.destination.name"] != "orders") {
				t.Errorf("msk attrs %v", s.Attrs)
			}
		case model.DBCPU:
			if s.Attrs["trooper.db.node"] == "sessions-001" && (s.Attrs["trooper.db.id"] != "aws-sessions" || s.Attrs["db.system"] != "redis") {
				t.Errorf("cache attrs %v", s.Attrs)
			}
		}
	}
}

func TestGetMetricDataBatches(t *testing.T) {
	ts := time.Date(2026, 10, 25, 12, 4, 0, 0, time.UTC)
	f := fakes.NewAWS(t, ts, func(string, map[string]string) (float64, bool) { return 1, true })
	defer f.Close()
	p, err := New(context.Background(), []byte(`{"regions":["us-east-1"]}`), nil, Options{Endpoint: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	rs := &regionState{region: "us-east-1"}
	for i := range 130 { // 130 queues × 5 metrics = 650 queries → 2 batches
		rs.queues = append(rs.queues, sqsQueue{Name: fmt.Sprintf("q%d", i)})
	}
	v, err := fetch(context.Background(), p.regions[0].cw, plan(rs), ts.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if f.GMDCalls != 2 || f.Queries != 650 || len(v) != 130 {
		t.Fatalf("calls %d queries %d keys %d", f.GMDCalls, f.Queries, len(v))
	}
	if pt := v[queueKey("q129")]["NumberOfMessagesDeleted"]; pt.Value != 1 || !pt.TS.Equal(ts) {
		t.Fatalf("q129 %+v", pt)
	}
}

func TestInstanceMemoryGB(t *testing.T) {
	tests := []struct {
		class string
		want  float64
	}{
		{"db.t3.micro", 1}, {"db.t4g.medium", 4}, {"db.t3.large", 8}, {"db.t3.2xlarge", 32},
		{"db.m5.large", 8}, {"db.m6g.4xlarge", 64}, {"db.r6g.large", 16}, {"db.r7g.16xlarge", 512},
		{"db.x2g.large", 32}, {"db.serverless", 0}, {"db.m5.metal", 0}, {"bogus", 0}, {"db.custom.1", 0},
	}
	for _, tt := range tests {
		if got := InstanceMemoryGB(tt.class, nil); got != tt.want {
			t.Errorf("%s = %v want %v", tt.class, got, tt.want)
		}
	}
	if InstanceMemoryGB("db.z1d.large", map[string]float64{"db.z1d.large": 16}) != 16 {
		t.Error("override")
	}
}

func TestNewRejectsBadSecret(t *testing.T) {
	if _, err := New(context.Background(), []byte(`{"regions":["eu-west-1"]}`), []byte(`not json`), Options{}); err == nil {
		t.Fatal("want error")
	}
	if _, err := New(context.Background(), []byte(`{"regions":["eu-west-1"]}`), []byte(`{"accessKeyId":"AKIA","secretAccessKey":"x"}`), Options{}); err != nil {
		t.Fatal(err)
	}
}
