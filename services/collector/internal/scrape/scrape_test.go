package scrape

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

func TestGroupFromConsumerTag(t *testing.T) {
	tests := []struct{ tag, queue, want string }{
		{"amq.ctag-H3kq9xZ_abc", "jobs", "jobs"},
		{"", "jobs", "jobs"},
		{"billing-worker-7f9c2a", "q", "billing-worker"},
		{"billing-worker-5d8f7b9c4-x2k9p", "q", "billing-worker"},
		{"order-indexer-0", "q", "order-indexer"},
		{"mailer_3", "q", "mailer"},
		{"svc-3fa85f64-5717-4562-b3fc-2c963f66afa6", "q", "svc"},
		{"worker2", "q", "worker2"},
		{"email.sender.12", "q", "email.sender"},
		{"12345", "q", "q"},
		{"notifications", "q", "notifications"},
	}
	for _, tt := range tests {
		if got := GroupFromConsumerTag(tt.tag, tt.queue); got != tt.want {
			t.Errorf("GroupFromConsumerTag(%q)=%q want %q", tt.tag, got, tt.want)
		}
	}
}

func TestParseInfo(t *testing.T) {
	raw := "# Server\r\nredis_version:7.2.4\r\nrole:master\r\n\r\n# Clients\r\nconnected_clients:12\r\n" +
		"slave0:ip=10.0.0.2,port=6379,state=online,offset=1234,lag=1\r\ncmdstat_get:calls=10,usec=50,usec_per_call=5.00\r\n"
	info := ParseInfo(raw)
	if info["redis_version"] != "7.2.4" || info["role"] != "master" || info["connected_clients"] != "12" {
		t.Fatalf("%v", info)
	}
	kv := ParseKV(info["slave0"])
	if kv["ip"] != "10.0.0.2" || kv["lag"] != "1" || kv["offset"] != "1234" {
		t.Fatalf("%v", kv)
	}
	if ParseKV(info["cmdstat_get"])["usec"] != "50" {
		t.Fatal("cmdstat")
	}
}

func TestNormalizeQueryText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"SELECT *\n  FROM t\tWHERE id = $1 ", "SELECT * FROM t WHERE id = $1"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeQueryText(tt.in); got != tt.want {
			t.Errorf("%q → %q want %q", tt.in, got, tt.want)
		}
	}
	long := strings.Repeat("é", 2000)
	got := NormalizeQueryText(long)
	if len(got) > MaxQueryText || !strings.HasSuffix(got, "…") || !utf8Valid(got) {
		t.Fatalf("truncation broken: len=%d", len(got))
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestHexDigestToUint64(t *testing.T) {
	tests := []struct{ in, want string }{
		{"00000000000000ff" + strings.Repeat("a", 48), "255"},
		{"ffffffffffffffff", "18446744073709551615"},
	}
	for _, tt := range tests {
		if got := HexDigestToUint64(tt.in); got != tt.want {
			t.Errorf("%q → %q want %q", tt.in, got, tt.want)
		}
	}
	if HexDigestToUint64("not-hex") == "" {
		t.Error("fallback hash empty")
	}
}

func TestTopN(t *testing.T) {
	tests := []struct {
		in   []float64
		n    int
		want []int
	}{
		{[]float64{1, 5, 3, 0, 4}, 2, []int{1, 4}},
		{[]float64{1, 2}, 5, []int{1, 0}},
		{[]float64{0, 0}, 3, []int{}},
	}
	for _, tt := range tests {
		got := TopN(tt.in, tt.n)
		if len(got) != len(tt.want) {
			t.Fatalf("%v: got %v want %v", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("%v: got %v want %v", tt.in, got, tt.want)
			}
		}
	}
}

func TestLagFor(t *testing.T) {
	tests := []struct{ end, committed, start, want int64 }{
		{100, 90, 0, 10},
		{100, 100, 0, 0},
		{100, -1, 40, 60},
		{100, 120, 0, 0}, // committed ahead after truncation: never negative
	}
	for _, tt := range tests {
		if got := LagFor(tt.end, tt.committed, tt.start); got != tt.want {
			t.Errorf("LagFor(%d,%d,%d)=%d want %d", tt.end, tt.committed, tt.start, got, tt.want)
		}
	}
}

func TestGenerationKey(t *testing.T) {
	if GenerationKey([]string{"b", "a"}) != GenerationKey([]string{"a", "b"}) {
		t.Fatal("order must not matter")
	}
	if GenerationKey(nil) != "" {
		t.Fatal("empty")
	}
}

func TestSystemName(t *testing.T) {
	if SystemName("postgres") != "postgresql" || SystemName("redis") != "redis" {
		t.Fatal()
	}
}

func TestKafkaOpts(t *testing.T) {
	t.Setenv("TTCOL_KAFKA_CREDS", "u:p")
	tests := []struct {
		name string
		auth config.BrokerAuth
		err  string
	}{
		{"plaintext", config.BrokerAuth{Mechanism: "none"}, ""},
		{"scram512 tls", config.BrokerAuth{Mechanism: "scram-sha-512", TLS: true}, ""},
		{"scram256", config.BrokerAuth{Mechanism: "scram-sha-256"}, ""},
		{"plain", config.BrokerAuth{Mechanism: "plain"}, ""},
		{"iam static", config.BrokerAuth{Mechanism: "iam", AWSRegion: "eu-west-1"}, ""},
		{"oauth static token", config.BrokerAuth{Mechanism: "oauthbearer"}, ""},
		{"oauth client credentials", config.BrokerAuth{Mechanism: "oauthbearer", TokenURL: "https://idp/token"}, ""},
		{"unknown", config.BrokerAuth{Mechanism: "kerberos"}, "unknown"},
		{"missing ca", config.BrokerAuth{TLS: true, CAFile: "/nonexistent/ca.pem"}, "caFile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := KafkaOpts(config.Broker{Endpoint: "a:9092,b:9092", Auth: tt.auth, CredentialsRef: "env:TTCOL_KAFKA_CREDS"})
			if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err=%v want %q", err, tt.err)
			}
		})
	}
}

func TestKafkaOptsIAMBadSecret(t *testing.T) {
	t.Setenv("TTCOL_KAFKA_IAM", "only-one-part")
	_, err := KafkaOpts(config.Broker{Endpoint: "b-1:9098", Auth: config.BrokerAuth{Mechanism: "iam"}, CredentialsRef: "env:TTCOL_KAFKA_IAM"})
	if err == nil || !strings.Contains(err.Error(), "accessKeyId") {
		t.Fatalf("err=%v", err)
	}
}

func TestNewDatabaseMongo(t *testing.T) {
	s, err := NewDatabase(config.Database{Engine: "mongodb", Host: "h", Port: 27017, Timeout: config.Duration(time.Second)}, HostOptions{}, slog.Default())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	_ = s.Close()
}

// fakeRabbit serves the three management endpoints the scraper reads.
func fakeRabbit(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "trooper" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var v any
		switch r.URL.EscapedPath() {
		case "/api/queues/%2Fshop":
			v = []map[string]any{
				{"name": "emails", "messages_ready": 120, "messages_unacknowledged": 8, "consumers": 3,
					"messages": 128, "message_bytes": 128 * 512, "head_message_timestamp": float64(time.Now().Unix() - 90),
					"type": "quorum", "members": []string{"rabbit@node1", "rabbit@node2", "rabbit@node3"},
					"arguments": map[string]any{"x-message-ttl": 60000}, "effective_policy_definition": map[string]any{},
					"message_stats": map[string]any{"publish_details": map[string]any{"rate": 40.0}, "deliver_get_details": map[string]any{"rate": 30.0}}},
				{"name": "orphan", "messages_ready": 7, "messages_unacknowledged": 0, "consumers": 0},
			}
		case "/api/consumers/%2Fshop":
			v = []map[string]any{
				{"consumer_tag": "mailer-7f9c2a", "queue": map[string]any{"name": "emails"}},
				{"consumer_tag": "mailer-a81b3c", "queue": map[string]any{"name": "emails"}},
				{"consumer_tag": "amq.ctag-xyz", "queue": map[string]any{"name": "emails"}},
			}
		case "/api/nodes":
			v = []map[string]any{{"name": "rabbit@node1", "running": true, "mem_used": 400.0, "mem_limit": 1000.0,
				"fd_used": 50, "fd_total": 1000, "proc_used": 100, "proc_total": 1000, "disk_free": 1000.0, "disk_free_limit": 50.0}}
		default:
			t.Errorf("unexpected path %s", r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
}

func find(r emit.Result, name string, attrs map[string]string) (float64, bool) {
	for _, s := range r.Gauges {
		if s.Name != name {
			continue
		}
		ok := true
		for k, v := range attrs {
			if got, has := s.Attrs.Value(attributeKey(k)); !has || got.AsString() != v {
				ok = false
			}
		}
		if ok {
			return s.Value, true
		}
	}
	return 0, false
}

func TestRabbitMQScrapeFake(t *testing.T) {
	srv := fakeRabbit(t)
	defer srv.Close()
	t.Setenv("TTCOL_RMQ", "pw")
	cfg, err := config.Parse([]byte("brokerClusters: [{id: rabbit-jobs, kind: rabbitmq, endpoint: '" + srv.URL + "', vhost: /shop, credentialsRef: 'env:TTCOL_RMQ'}]"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewRabbitMQ(cfg.BrokerClusters[0], slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		metric string
		attrs  map[string]string
		want   float64
	}{
		{emit.BrokerLag, map[string]string{"messaging.destination.name": "emails", "messaging.consumer.group.name": "mailer", "messaging.destination.partition.id": "0", "messaging.system": "rabbitmq", "trooper.broker.cluster": "rabbit-jobs"}, 120},
		{emit.BrokerLag, map[string]string{"messaging.destination.name": "emails", "messaging.consumer.group.name": "emails"}, 120},
		{emit.BrokerConsumeRate, map[string]string{"messaging.consumer.group.name": "mailer"}, 20},
		{emit.BrokerConsumeRate, map[string]string{"messaging.consumer.group.name": "emails"}, 10},
		{emit.BrokerProduceRate, map[string]string{"messaging.consumer.group.name": "mailer"}, 40},
		{emit.BrokerUnacked, map[string]string{"messaging.consumer.group.name": "mailer"}, 8 * 2.0 / 3},
		{emit.BrokerLag, map[string]string{"messaging.destination.name": "orphan", "messaging.consumer.group.name": ""}, 7},
		{emit.BrokerGroupMembers, map[string]string{"messaging.consumer.group.name": "mailer", "trooper.broker.group.state": "Stable"}, 2},
		{emit.BrokerNodeMemory, map[string]string{"trooper.broker.node": "rabbit@node1"}, 0.4},
		{emit.BrokerNodeDisk, map[string]string{"trooper.broker.node": "rabbit@node1"}, 0.05},
		{emit.BrokerNodeFD, map[string]string{"trooper.broker.node": "rabbit@node1"}, 0.05},
		{emit.BrokerNodeProc, map[string]string{"trooper.broker.node": "rabbit@node1"}, 0.1},
		{emit.BrokerTopicPartitions, map[string]string{"messaging.destination.name": "emails", "messaging.system": "rabbitmq"}, 1},
		{emit.BrokerTopicReplication, map[string]string{"messaging.destination.name": "emails"}, 3},
		{emit.BrokerTopicRetentionMs, map[string]string{"messaging.destination.name": "emails"}, 60000},
		{emit.BrokerTopicRetentionMs, map[string]string{"messaging.destination.name": "orphan"}, -1},
		{emit.BrokerTopicSizeBytes, map[string]string{"messaging.destination.name": "emails"}, 128 * 512},
		{emit.BrokerTopicAvgMsgBytes, map[string]string{"messaging.destination.name": "emails"}, 512},
		{emit.BrokerTopicInRate, map[string]string{"messaging.destination.name": "emails"}, 40},
		{emit.BrokerTopicBytesInRate, map[string]string{"messaging.destination.name": "emails"}, 40 * 512},
	}
	for _, c := range checks {
		got, ok := find(r, c.metric, c.attrs)
		if !ok || mathAbs(got-c.want) > 1e-9 {
			t.Errorf("%s %v = %v,%v want %v", c.metric, c.attrs, got, ok, c.want)
		}
	}
	if age, ok := find(r, emit.BrokerOldestAgeS, map[string]string{"messaging.destination.name": "emails", "messaging.consumer.group.name": "mailer"}); !ok || age < 89 || age > 95 {
		t.Errorf("oldest_age_s = %v,%v want ≈90", age, ok)
	}
	if _, ok := find(r, emit.BrokerOldestAgeS, map[string]string{"messaging.destination.name": "orphan"}); ok {
		t.Error("queue without head timestamp must not report oldest_age_s")
	}
	if _, ok := find(r, emit.BrokerConsumeRate, map[string]string{"messaging.destination.name": "orphan"}); ok {
		t.Error("queue without readers must not report consume rate")
	}

	// bad credentials → error, not panic
	t.Setenv("TTCOL_RMQ", "wrong")
	s2, _ := NewRabbitMQ(cfg.BrokerClusters[0], slog.Default())
	if _, err := s2.Scrape(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401, got %v", err)
	}
}

func mathAbs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func attributeKey(k string) attribute.Key { return attribute.Key(k) }
