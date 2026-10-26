package scrape

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestParsePromText(t *testing.T) {
	in := `# HELP x
# TYPE node_cpu_seconds_total counter
node_cpu_seconds_total{cpu="0",mode="idle"} 100
node_cpu_seconds_total{cpu="0",mode="user"} 50 1700000000000
weird{a="x\"y",b="c,d"} 3
kafka_server_BrokerTopicMetrics_BytesInPerSec_Count 1e6
broken{a="x" 4
nan_metric NaN
`
	p, err := ParsePromText(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := p.Sum("node_cpu_seconds_total"); !ok || v != 150 {
		t.Errorf("sum %v %v", v, ok)
	}
	if v, _ := p.Sum("node_cpu_seconds_total", "mode=idle"); v != 100 {
		t.Errorf("idle %v", v)
	}
	if v, _ := p.Sum("node_cpu_seconds_total", "mode=!idle"); v != 50 {
		t.Errorf("not idle %v", v)
	}
	if w := p["weird"]; len(w) != 1 || w[0].Labels["a"] != `x"y` || w[0].Labels["b"] != "c,d" {
		t.Errorf("labels %+v", w)
	}
	if v, ok := kafkaBytesIn(p); !ok || v != 1e6 {
		t.Errorf("bytes in %v %v", v, ok)
	}
	if _, ok := p["broken"]; ok {
		t.Error("malformed line parsed")
	}
	if _, ok := p.First("nan_metric"); ok {
		t.Error("NaN must be skipped")
	}
}

func TestNodeMapper(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(10 * time.Second)
	tests := []struct {
		name   string
		a, b   string
		mount  string
		expect map[string]float64
	}{
		{
			name: "node exporter",
			a: `node_cpu_seconds_total{cpu="0",mode="idle"} 100
node_cpu_seconds_total{cpu="0",mode="user"} 100
node_memory_MemTotal_bytes 1000
node_memory_MemAvailable_bytes 250
node_filesystem_size_bytes{mountpoint="/data"} 100
node_filesystem_avail_bytes{mountpoint="/data"} 40
node_filesystem_size_bytes{mountpoint="/"} 100
node_filesystem_avail_bytes{mountpoint="/"} 90
node_network_receive_bytes_total{device="eth0"} 0
node_network_receive_bytes_total{device="lo"} 0`,
			b: `node_cpu_seconds_total{cpu="0",mode="idle"} 106
node_cpu_seconds_total{cpu="0",mode="user"} 104
node_memory_MemTotal_bytes 1000
node_memory_MemAvailable_bytes 250
node_filesystem_size_bytes{mountpoint="/data"} 100
node_filesystem_avail_bytes{mountpoint="/data"} 40
node_network_receive_bytes_total{device="eth0"} 50000000
node_network_receive_bytes_total{device="lo"} 99999999999`,
			mount:  "/data",
			expect: map[string]float64{emit.BrokerNodeCPU: 0.4, emit.BrokerNodeMemory: 0.75, emit.BrokerNodeDisk: 0.6, emit.BrokerNodeNetIn: 5},
		},
		{
			name: "jmx exporter",
			a: `java_lang_OperatingSystem_CpuLoad 0.3
java_lang_OperatingSystem_TotalMemorySize 2000
java_lang_OperatingSystem_FreeMemorySize 500
kafka_server_brokertopicmetrics_bytesin_total{topic="a"} 1000
kafka_server_brokertopicmetrics_bytesin_total 1000000`,
			b: `java_lang_OperatingSystem_CpuLoad 0.3
java_lang_OperatingSystem_TotalMemorySize 2000
java_lang_OperatingSystem_FreeMemorySize 500
kafka_server_brokertopicmetrics_bytesin_total{topic="a"} 99999
kafka_server_brokertopicmetrics_bytesin_total 21000000`,
			expect: map[string]float64{emit.BrokerNodeCPU: 0.3, emit.BrokerNodeMemory: 0.75, emit.BrokerNodeNetIn: 2},
		},
		{
			name: "redpanda",
			a: `redpanda_cpu_busy_seconds_total{shard="0"} 10
redpanda_cpu_busy_seconds_total{shard="1"} 10
redpanda_memory_allocated_memory{shard="0"} 300
redpanda_memory_free_memory{shard="0"} 700
redpanda_storage_disk_total_bytes 1000
redpanda_storage_disk_free_bytes 800
redpanda_kafka_request_bytes_total{redpanda_request="produce",redpanda_topic="a"} 0
redpanda_kafka_request_bytes_total{redpanda_request="consume",redpanda_topic="a"} 0`,
			b: `redpanda_cpu_busy_seconds_total{shard="0"} 15
redpanda_cpu_busy_seconds_total{shard="1"} 13
redpanda_memory_allocated_memory{shard="0"} 300
redpanda_memory_free_memory{shard="0"} 700
redpanda_storage_disk_total_bytes 1000
redpanda_storage_disk_free_bytes 800
redpanda_kafka_request_bytes_total{redpanda_request="produce",redpanda_topic="a"} 10000000
redpanda_kafka_request_bytes_total{redpanda_request="consume",redpanda_topic="a"} 999999999`,
			expect: map[string]float64{emit.BrokerNodeCPU: 0.4, emit.BrokerNodeMemory: 0.3, emit.BrokerNodeDisk: 0.2, emit.BrokerNodeNetIn: 1},
		},
		{
			name: "rabbitmq_prometheus",
			a: `rabbitmq_process_resident_memory_bytes 200
rabbitmq_resident_memory_limit_bytes 1000
rabbitmq_disk_space_available_bytes 1000
rabbitmq_disk_space_available_limit_bytes 100
erlang_vm_statistics_bytes_received_total 0
erlang_vm_msacc_sleep_seconds_total{type="scheduler",id="1"} 10
erlang_vm_msacc_emulator_seconds_total{type="scheduler",id="1"} 10`,
			b: `rabbitmq_process_resident_memory_bytes 200
rabbitmq_resident_memory_limit_bytes 1000
rabbitmq_disk_space_available_bytes 1000
rabbitmq_disk_space_available_limit_bytes 100
erlang_vm_statistics_bytes_received_total 3000000
erlang_vm_msacc_sleep_seconds_total{type="scheduler",id="1"} 17
erlang_vm_msacc_emulator_seconds_total{type="scheduler",id="1"} 13`,
			expect: map[string]float64{emit.BrokerNodeCPU: 0.3, emit.BrokerNodeMemory: 0.2, emit.BrokerNodeDisk: 0.1, emit.BrokerNodeNetIn: 0.3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewNodeMapper()
			pa, _ := ParsePromText(strings.NewReader(tt.a))
			pb, _ := ParsePromText(strings.NewReader(tt.b))
			first := m.Map("n", pa, tt.mount, t0)
			if _, ok := first[emit.BrokerNodeNetIn]; ok {
				t.Error("first scrape cannot have a net_in rate")
			}
			got := m.Map("n", pb, tt.mount, t1)
			for k, want := range tt.expect {
				if v, ok := got[k]; !ok || !near(v, want) {
					t.Errorf("%s = %v,%v want %v", k, v, ok, want)
				}
			}
		})
	}
}

func TestTopicSizeStats(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(10 * time.Second)
	tests := []struct {
		name                 string
		prev                 *sizeReading
		size, msgs, retained float64
		inRate               float64
		haveRate             bool
		avg, bytes           float64
		haveAvg, haveBytes   bool
	}{
		{"first scrape lifetime avg", nil, 10000, 100, 100, 0, false, 100, 0, true, false},
		{"interval delta", &sizeReading{size: 10000, msgs: 100, at: t0}, 15000, 120, 120, 2, true, 250, 500, true, true},
		{"no traffic", &sizeReading{size: 10000, msgs: 100, at: t0}, 10000, 100, 100, 0, true, 100, 0, true, true},
		{"retention deleted segments", &sizeReading{size: 10000, msgs: 100, at: t0}, 6000, 110, 60, 1, true, 100, 100, true, true},
		{"empty topic", nil, 0, 0, 0, 0, false, 0, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			avg, b, ha, hb := TopicSizeStats(tt.prev, tt.size, tt.msgs, tt.retained, t1, tt.inRate, tt.haveRate)
			if ha != tt.haveAvg || hb != tt.haveBytes || (ha && !near(avg, tt.avg)) || (hb && !near(b, tt.bytes)) {
				t.Fatalf("got avg=%v(%v) bytes=%v(%v)", avg, ha, b, hb)
			}
		})
	}
}

func TestTitleState(t *testing.T) {
	for in, want := range map[string]string{"stable": "Stable", "assigning": "Assigning", "not_ready": "NotReady", "Stable": "Stable", "": ""} {
		if got := TitleState(in); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

func TestQueueRetentionAndAge(t *testing.T) {
	if v := QueueRetentionMs(map[string]any{"x-message-ttl": 5000.0}, map[string]any{"message-ttl": 1.0}); v != 5000 {
		t.Error(v)
	}
	if v := QueueRetentionMs(nil, map[string]any{"message-ttl": 7.0}); v != 7 {
		t.Error(v)
	}
	if v := QueueRetentionMs(nil, nil); v != -1 {
		t.Error(v)
	}
	now := time.Unix(10_000, 0)
	if v, ok := OldestAgeSeconds(9_970.0, now); !ok || v != 30 {
		t.Error(v, ok)
	}
	if v, ok := OldestAgeSeconds(9_970_000.0*1000, now); !ok || v != 0 { // ms, in the future → 0
		t.Error(v, ok)
	}
	if _, ok := OldestAgeSeconds("", now); ok {
		t.Error("empty")
	}
	if _, ok := OldestAgeSeconds(nil, now); ok {
		t.Error("nil")
	}
}

func TestHistogramQuantile(t *testing.T) {
	lower := []float64{0, 8, 64, 512, 3072}
	tests := []struct {
		counts []float64
		want   float64
		ok     bool
	}{
		{[]float64{0, 0, 100, 0, 0}, 64 + 0.95*(512-64), true},
		{[]float64{0, 90, 0, 10, 0}, 512 + 0.5*(3072-512), true},
		{[]float64{0, 0, 0, 0, 5}, 3072, true},
		{[]float64{0, 0, 0, 0, 0}, 0, false},
	}
	for i, tt := range tests {
		got, ok := HistogramQuantile(lower, tt.counts, 0.95)
		if ok != tt.ok || (ok && !near(got, tt.want)) {
			t.Errorf("%d: %v,%v want %v", i, got, ok, tt.want)
		}
	}
}

func TestReplStatus(t *testing.T) {
	p := time.Unix(1000, 0)
	doc := func(my int32, self string) bson.M {
		return bson.M{"myState": my, "members": bson.A{
			bson.D{{Key: "name", Value: "a:27017"}, {Key: "state", Value: int32(1)}, {Key: "optimeDate", Value: bson.NewDateTimeFromTime(p)}, {Key: "self", Value: self == "a"}},
			bson.D{{Key: "name", Value: "b:27017"}, {Key: "state", Value: int32(2)}, {Key: "optimeDate", Value: bson.NewDateTimeFromTime(p.Add(-1500 * time.Millisecond))}, {Key: "self", Value: self == "b"}},
		}}
	}
	role, lags := ReplStatus(doc(1, "a"))
	if role != "primary" || len(lags) != 1 || lags[0].node != "b:27017" || lags[0].ms != 1500 {
		t.Errorf("primary: %s %+v", role, lags)
	}
	role, lags = ReplStatus(doc(2, "b"))
	if role != "replica" || len(lags) != 1 || lags[0].node != "" || lags[0].ms != 1500 {
		t.Errorf("secondary: %s %+v", role, lags)
	}
	if role, _ := ReplStatus(bson.M{"myState": int32(7)}); role != "arbiter" {
		t.Error(role)
	}
}

func TestShapeText(t *testing.T) {
	raw, _ := bson.Marshal(bson.D{
		{Key: "cmdNs", Value: bson.D{{Key: "db", Value: "shop"}, {Key: "coll", Value: "orders"}}},
		{Key: "command", Value: "find"},
		{Key: "filter", Value: bson.D{{Key: "status", Value: bson.D{{Key: "$eq", Value: "?string"}}}}},
	})
	if got := ShapeText(raw); got != `find shop.orders {"filter":{"status":{"$eq":"?string"}}}` {
		t.Errorf("got %s", got)
	}
}

func TestProfileShape(t *testing.T) {
	a := bson.M{"op": "query", "ns": "shop.orders", "command": bson.D{
		{Key: "find", Value: "orders"}, {Key: "filter", Value: bson.D{{Key: "status", Value: "new"}, {Key: "total", Value: bson.D{{Key: "$gt", Value: int32(5)}}}}},
		{Key: "lsid", Value: bson.D{{Key: "id", Value: "x"}}}, {Key: "$db", Value: "shop"}, {Key: "limit", Value: int32(3)},
	}}
	b := bson.M{"op": "query", "ns": "shop.orders", "command": bson.M{
		"find": "orders", "filter": bson.M{"total": bson.M{"$gt": 99.5}, "status": "paid"}, "$db": "shop",
	}}
	sa, sb := ProfileShape(a), ProfileShape(b)
	if sa != sb {
		t.Fatalf("shapes differ:\n%s\n%s", sa, sb)
	}
	if strings.Contains(sa, "new") || strings.Contains(sa, "lsid") || !strings.Contains(sa, `"$gt":"?"`) {
		t.Errorf("shape %s", sa)
	}
	in := bson.M{"op": "query", "ns": "s.o", "command": bson.D{{Key: "find", Value: "o"}, {Key: "filter", Value: bson.D{{Key: "id", Value: bson.D{{Key: "$in", Value: bson.A{int32(1), int32(2), int32(3)}}}}}}}}
	if s := ProfileShape(in); !strings.Contains(s, `"$in":["?",…]`) {
		t.Errorf("array shape %s", s)
	}
}

func TestTokenSource(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id, sec, _ := r.BasicAuth()
		_ = r.ParseForm()
		if id != "cid" || sec != "s3cret" || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "kafka read" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":60}`, calls.Load())
	}))
	defer srv.Close()
	now := time.Unix(0, 0)
	ts := &tokenSource{url: srv.URL, clientID: "cid", secret: "s3cret", scopes: []string{"kafka", "read"}, hc: srv.Client(), now: func() time.Time { return now }}
	ctx := context.Background()
	a, err := ts.Token(ctx)
	if err != nil || a != "tok-1" {
		t.Fatal(a, err)
	}
	if b, _ := ts.Token(ctx); b != "tok-1" {
		t.Error("token not cached")
	}
	now = now.Add(45 * time.Second) // within 30 s of expiry → refresh
	if c, _ := ts.Token(ctx); c != "tok-2" {
		t.Error("token not refreshed", c)
	}
	bad := &tokenSource{url: srv.URL, clientID: "x", secret: "y", hc: srv.Client()}
	if _, err := bad.Token(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("want 401, got %v", err)
	}
}
