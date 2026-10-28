//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	colprofilespb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/app"
	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling/otlptest"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// TestOTLPProfiles: OTLP profiles (contracts §24 "OTLP profiles") through ingest-gw — OTLP/HTTP protobuf (gzip),
// OTLP/HTTP JSON and gRPC ProfilesService/Export — → tt.profiles (format=otlp) → ingest-writer → profile_stacks / profiles.
// Services carry a random suffix and are deleted at the end.
func TestOTLPProfiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sfx := fmt.Sprintf("otlpprof-e2e-%d", rand.Uint32())

	wcfg := app.WriterFromEnv()
	wcfg.HealthAddr, wcfg.MetricsAddr = "", ""
	wcfg.Writer.FlushInterval = 500 * time.Millisecond
	group, dropGroup := isolatedGroup(t, ctx, wcfg.Writer.Brokers, topics.Profiles)
	defer dropGroup()
	wcfg.Writer.Group, wcfg.Writer.Topics = group, []string{topics.Profiles}
	wctx, wstop := context.WithCancel(ctx)
	wdone := make(chan error, 1)
	go func() { wdone <- app.RunWriter(wctx, wcfg, log) }()
	defer func() { wstop(); <-wdone }()

	gcfg := app.GatewayFromEnv()
	gcfg.GW.HTTPAddr, gcfg.GW.GRPCAddr, gcfg.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0", ""
	type addrs struct{ http, grpc string }
	ready := make(chan addrs, 1)
	gctx, gstop := context.WithCancel(ctx)
	gdone := make(chan error, 1)
	go func() { gdone <- app.RunGateway(gctx, gcfg, log, func(h, g string) { ready <- addrs{h, g} }) }()
	defer func() { gstop(); <-gdone }()
	var a addrs
	select {
	case a = <-ready:
	case err := <-gdone:
		t.Fatalf("gateway failed: %v", err)
	}

	start := uint64(time.Now().Add(-30 * time.Second).UnixNano())

	// 1) OTLP/HTTP protobuf, Content-Encoding: gzip — a Go service: cpu with endpoint attributes + alloc_space with an inline frame.
	b := otlptest.New()
	h := b.Stack(b.Loc("main.handler"), b.Loc("main.main"))
	inl := b.Stack(b.Loc("bytes.growSlice", "bytes.(*Buffer).Write"), b.Loc("main.main"))
	route, method := b.Attr("trooper.route", "/users/:id"), b.Attr("trooper.method", "GET")
	b.Resource([]string{"service.name", sfx + "-go", "deployment.environment.name", "e2e", "service.version", "1.0.0", "host.name", "h-" + sfx,
		"process.runtime.name", "go"},
		&profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), TimeUnixNano: start, DurationNano: 10e9,
			Samples: []*profilespb.Sample{
				{StackIndex: h, Values: []int64{30_000_000}, AttributeIndices: []int32{route, method}},
				{StackIndex: h, Values: []int64{10_000_000}, AttributeIndices: []int32{route, method}},
			}},
		&profilespb.Profile{SampleType: b.VT("alloc_space", "bytes"), TimeUnixNano: start, DurationNano: 10e9,
			Samples: []*profilespb.Sample{{StackIndex: inl, Values: []int64{8192}}}})
	pb, err := proto.Marshal(b.Req)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(pb)
	_ = zw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+a.http+"/v1development/profiles", &gz)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+ingestKey)
	if code := do(t, req); code != 200 {
		t.Fatalf("otlp/http protobuf: %d", code)
	}

	// 2) OTLP/HTTP JSON — a JVM service (runtime from the Java SDK's process.runtime.name).
	js := fmt.Sprintf(`{"resourceProfiles":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"%s-jvm"}},
	  {"key":"process.runtime.name","value":{"stringValue":"OpenJDK Runtime Environment"}}]},
	 "scopeProfiles":[{"profiles":[{"sampleType":{"typeStrindex":1,"unitStrindex":2},"timeUnixNano":"%d","durationNano":"60000000000",
	  "profileId":"0102030405060708090a0b0c0d0e0f10","samples":[{"stackIndex":1,"values":["7000000"]}]}]}]}],
	 "dictionary":{"stringTable":["","wall","nanoseconds","com.acme.Main.main","com.acme.Api.get"],"functionTable":[{},{"nameStrindex":3},{"nameStrindex":4}],
	  "locationTable":[{},{"lines":[{"functionIndex":1}]},{"lines":[{"functionIndex":2}]}],"stackTable":[{},{"locationIndices":[2,1]}],
	  "attributeTable":[{}],"mappingTable":[{}],"linkTable":[{}]}}`, sfx, start)
	if code := post(t, "http://"+a.http+"/v1development/profiles", "application/json", ingestKey, []byte(js)); code != 200 {
		t.Fatalf("otlp/http json: %d", code)
	}
	if code := post(t, "http://"+a.http+"/v1development/profiles", "application/x-protobuf", ingestKey, []byte("garbage")); code != 400 {
		t.Fatalf("garbage: %d", code)
	}

	// 3) gRPC — eBPF-profiler shape: no service.name, samples/count × period, timestamps only, an unsymbolized native frame.
	e := otlptest.New()
	st := e.Stack(e.NativeLoc("/usr/lib/x86_64-linux-gnu/libc.so.6", 0xdead), e.Loc("ngx_worker_process_cycle"), e.Loc("main"))
	e.Resource([]string{"process.executable.name", sfx + "-nginx", "host.name", "h-" + sfx},
		&profilespb.Profile{SampleType: e.VT("samples", "count"), PeriodType: e.VT("cpu", "nanoseconds"), Period: 50_000_000,
			TimeUnixNano: start, DurationNano: 5e9,
			Samples: []*profilespb.Sample{{StackIndex: st, TimestampsUnixNano: []uint64{start + 1, start + 2, start + 3, start + 4}}}})
	conn, err := grpc.NewClient(a.grpc, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	gctxKey := metadata.AppendToOutgoingContext(ctx, "x-trooper-key", ingestKey)
	if _, err := colprofilespb.NewProfilesServiceClient(conn).Export(gctxKey, e.Req, grpc.UseCompressor(grpcgzip.Name)); err != nil {
		t.Fatalf("grpc export: %v", err)
	}

	opts, err := clickhouse.ParseDSN(wcfg.Common.ClickHouseURL)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		for _, tbl := range []string{"profile_stacks", "profiles"} {
			if err := ch.Exec(cctx, fmt.Sprintf("ALTER TABLE %s DELETE WHERE project_id = 1 AND service LIKE '%%%s%%'", tbl, sfx)); err != nil {
				t.Logf("cleanup (ignored): %v", err)
			}
		}
	}()

	checks := []struct{ name, q string }{
		{"http protobuf cpu", fmt.Sprintf("SELECT count() FROM profiles WHERE service = '%s-go' AND env = 'e2e' AND version = '1.0.0' AND runtime = 'go' AND type = 'cpu' AND total = 40000000 AND stacks = 1 AND end_ts - ts = 10", sfx)},
		{"http protobuf route stack", fmt.Sprintf("SELECT count() FROM profile_stacks WHERE service = '%s-go' AND type = 'cpu' AND route = '/users/:id' AND method = 'GET' AND stack = ['main.main', 'main.handler'] AND value = 40000000 AND stack_hash = cityHash64(stack)", sfx)},
		{"http protobuf inline frames", fmt.Sprintf("SELECT count() FROM profile_stacks WHERE service = '%s-go' AND type = 'alloc_space' AND unit = 'bytes' AND stack = ['main.main', 'bytes.(*Buffer).Write', 'bytes.growSlice'] AND value = 8192", sfx)},
		{"http json", fmt.Sprintf("SELECT count() FROM profile_stacks WHERE service = '%s-jvm' AND runtime = 'jvm' AND type = 'wall' AND stack = ['com.acme.Main.main', 'com.acme.Api.get'] AND value = 7000000", sfx)},
		{"grpc ebpf shape", fmt.Sprintf("SELECT count() FROM profile_stacks WHERE service = 'unknown_service:%s-nginx' AND host = 'h-%s' AND runtime = '' AND type = 'cpu' AND stack = ['main', 'ngx_worker_process_cycle', '[libc.so.6]'] AND value = 200000000", sfx, sfx)},
	}
	deadline := time.Now().Add(60 * time.Second)
	for _, c := range checks {
		for {
			var n uint64
			err := ch.QueryRow(ctx, c.q).Scan(&n)
			if err == nil && n > 0 {
				t.Logf("✓ %-28s %d", c.name, n)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no rows (err=%v): %s", c.name, err, c.q)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// isolatedGroup prepares a consumer group of its own for the in-process writer, committed at the end of topic: another
// ingest-writer on the shared group (a dev stack) cannot take the test's partitions, and the history is not re-ingested.
// The group is deleted by the returned func.
func isolatedGroup(t *testing.T, ctx context.Context, brokers []string, topic string) (string, func()) {
	t.Helper()
	group := fmt.Sprintf("e2e-otlp-profiles-%d", rand.Uint32())
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	mreq := kmsg.NewPtrMetadataRequest()
	mt := kmsg.NewMetadataRequestTopic()
	mt.Topic = kmsg.StringPtr(topic)
	mreq.Topics = append(mreq.Topics, mt)
	mresp, err := mreq.RequestWith(ctx, cl)
	if err != nil || len(mresp.Topics) != 1 || len(mresp.Topics[0].Partitions) == 0 {
		t.Fatalf("metadata %s: %v", topic, err)
	}
	lreq := kmsg.NewPtrListOffsetsRequest()
	lt := kmsg.NewListOffsetsRequestTopic()
	lt.Topic = topic
	for _, p := range mresp.Topics[0].Partitions {
		lp := kmsg.NewListOffsetsRequestTopicPartition()
		lp.Partition, lp.Timestamp = p.Partition, -1 // latest
		lt.Partitions = append(lt.Partitions, lp)
	}
	lreq.Topics = append(lreq.Topics, lt)
	lresp, err := lreq.RequestWith(ctx, cl)
	if err != nil {
		t.Fatalf("list offsets: %v", err)
	}
	creq := kmsg.NewPtrOffsetCommitRequest()
	creq.Group, creq.Generation = group, -1
	ct := kmsg.NewOffsetCommitRequestTopic()
	ct.Topic = topic
	for _, rt := range lresp.Topics {
		for _, p := range rt.Partitions {
			if err := kerr.ErrorForCode(p.ErrorCode); err != nil {
				t.Fatalf("list offsets %s/%d: %v", topic, p.Partition, err)
			}
			cp := kmsg.NewOffsetCommitRequestTopicPartition()
			cp.Partition, cp.Offset = p.Partition, p.Offset
			ct.Partitions = append(ct.Partitions, cp)
		}
	}
	creq.Topics = append(creq.Topics, ct)
	cresp, err := creq.RequestWith(ctx, cl)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	for _, rt := range cresp.Topics {
		for _, p := range rt.Partitions {
			if err := kerr.ErrorForCode(p.ErrorCode); err != nil {
				t.Fatalf("commit %s/%d: %v", topic, p.Partition, err)
			}
		}
	}
	return group, func() {
		defer cl.Close()
		dreq := kmsg.NewPtrDeleteGroupsRequest()
		dreq.Groups = []string{group}
		if _, err := dreq.RequestWith(context.Background(), cl); err != nil {
			t.Logf("delete group (ignored): %v", err)
		}
	}
}
