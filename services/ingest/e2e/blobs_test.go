//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/pprof/profile"

	"github.com/tasktrooper/telemetry/services/ingest/internal/app"
)

// TestReplayProfilesLLM: session replay (POST /v1/replay → tt.replay → S3 + replay_segments / replay_sessions), profiles
// (POST /v1/profiles pprof + folded → tt.profiles → profile_stacks / profiles) and GenAI spans (OTLP/JSON → spans with
// dep_kind 'ai', llm_content, llm_1m, edges_1m). Needs the compose infra incl. the s3 service and
// deploy/clickhouse/005_replay_profiles_llm.sql. Rows use the service prefix "blobs-e2e-" and are deleted at the end.
func TestReplayProfilesLLM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := fmt.Sprintf("blobs-e2e-%d", rand.Uint32())
	session := fmt.Sprintf("sess-%d", rand.Uint32())

	wcfg := app.WriterFromEnv()
	wcfg.HealthAddr, wcfg.MetricsAddr = "", ""
	wcfg.Writer.FlushInterval = 500 * time.Millisecond
	if wcfg.S3.Endpoint == "" {
		wcfg.S3.Endpoint = "http://localhost:9002"
	}
	wcfg.S3.Bucket = "tt-replays-e2e"
	wctx, wstop := context.WithCancel(ctx)
	wdone := make(chan error, 1)
	go func() { wdone <- app.RunWriter(wctx, wcfg, log) }()
	defer func() { wstop(); <-wdone }()

	gcfg := app.GatewayFromEnv()
	gcfg.GW.HTTPAddr, gcfg.GW.GRPCAddr, gcfg.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0", ""
	addrs := make(chan string, 1)
	gctx, gstop := context.WithCancel(ctx)
	gdone := make(chan error, 1)
	go func() { gdone <- app.RunGateway(gctx, gcfg, log, func(h, _ string) { addrs <- h }) }()
	defer func() { gstop(); <-gdone }()
	var base string
	select {
	case a := <-addrs:
		base = "http://" + a
	case err := <-gdone:
		t.Fatalf("gateway failed: %v", err)
	}

	// 1) Replay: two segments, gzip'ed (CompressionStream) with the key in the query string (sendBeacon).
	now := time.Now().UnixMilli()
	for seg := range 2 {
		body := fmt.Sprintf(`{"resource":{"service":"%[1]s-web","env":"e2e","version":"1.0.0","platform":"web"},"session_id":"%[2]s","segment":%[3]d,
		 "start_ts":%[4]d,"end_ts":%[5]d,"format":"rrweb","has_full_snapshot":%[6]t,"view_name":"/checkout","anonymous_id":"anon-1",
		 "stats":{"clicks":2,"errors":%[3]d,"rage_clicks":0},"events":[{"type":4,"data":{"href":"https://shop.test/checkout"},"timestamp":%[4]d},{"type":2,"data":{},"timestamp":%[4]d}]}`,
			svc, session, seg, now+int64(seg)*5000, now+int64(seg)*5000+4000, seg == 0)
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/replay?key="+ingestKey, bytes.NewReader(gzipBytes(body)))
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("Origin", "https://shop.test")
		if code := do(t, req); code != 200 {
			t.Fatalf("replay segment %d: %d", seg, code)
		}
	}
	if code := post(t, base+"/v1/replay", "application/json", ingestKey, []byte(`{"session_id":"x"}`)); code != 400 {
		t.Fatalf("invalid replay: %d", code)
	}

	// 2) Profiles: a gzip'ed Go pprof with endpoint labels and a folded JVM profile.
	start := time.Now().Add(-10 * time.Second).UnixMilli()
	q := fmt.Sprintf("service=%s&env=e2e&version=1.2.3&host=h1&start=%d&end=%d", svc, start, start+10_000)
	if code := post(t, base+"/v1/profiles?"+q+"&runtime=go", "application/octet-stream", ingestKey, goProfile(t)); code != 200 {
		t.Fatalf("pprof: %d", code)
	}
	folded := "# trooper.route=/orders/:id trooper.method=GET\ncom.acme.Main.main;com.acme.Orders.get;com.acme.Db.query 7000000\n#\ncom.acme.Main.main;com.acme.Gc.run 3000000\n"
	if code := post(t, base+"/v1/profiles?"+q+"&runtime=jvm&format=folded&type=wall&unit=nanoseconds", "text/plain", ingestKey, []byte(folded)); code != 200 {
		t.Fatalf("folded: %d", code)
	}
	if code := post(t, base+"/v1/profiles?"+q+"&key="+ingestKey, "application/octet-stream", "", goProfile(t)); code != 401 {
		t.Fatalf("profiles ?key= must be refused: %d", code)
	}

	// 3) GenAI spans (OTLP/JSON): server → internal chat span (with content) → HTTP client to api.openai.com.
	ns := time.Now().UnixNano()
	spans := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"%[1]s"}},{"key":"deployment.environment.name","value":{"stringValue":"e2e"}}]},
	 "scopeSpans":[{"spans":[
	  {"traceId":"5af7651916cd43dd8448eb211c80319c","spanId":"a000000000000001","name":"POST /chat","kind":2,"startTimeUnixNano":"%[2]d","endTimeUnixNano":"%[3]d",
	   "attributes":[{"key":"http.request.method","value":{"stringValue":"POST"}},{"key":"http.route","value":{"stringValue":"/chat"}},{"key":"http.response.status_code","value":{"intValue":"200"}}]},
	  {"traceId":"5af7651916cd43dd8448eb211c80319c","spanId":"a000000000000002","parentSpanId":"a000000000000001","name":"chat gpt-4o","kind":1,"startTimeUnixNano":"%[2]d","endTimeUnixNano":"%[3]d",
	   "attributes":[{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.provider.name","value":{"stringValue":"openai"}},
	    {"key":"gen_ai.request.model","value":{"stringValue":"gpt-4o"}},{"key":"gen_ai.response.model","value":{"stringValue":"gpt-4o-2024-08-06"}},
	    {"key":"gen_ai.usage.input_tokens","value":{"intValue":"1200"}},{"key":"gen_ai.usage.output_tokens","value":{"intValue":"80"}},
	    {"key":"trooper.llm.ttft_ms","value":{"doubleValue":240.5}},
	    {"key":"gen_ai.input.messages","value":{"stringValue":"[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"hi\"}]}]"}},
	    {"key":"gen_ai.output.messages","value":{"stringValue":"[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"hello\"}]}]"}}]},
	  {"traceId":"5af7651916cd43dd8448eb211c80319c","spanId":"a000000000000003","parentSpanId":"a000000000000002","name":"POST","kind":3,"startTimeUnixNano":"%[2]d","endTimeUnixNano":"%[3]d",
	   "attributes":[{"key":"http.request.method","value":{"stringValue":"POST"}},{"key":"server.address","value":{"stringValue":"api.openai.com"}}]}
	 ]}]}]}`, svc, ns, ns+900_000_000)
	if code := post(t, base+"/v1/traces", "application/json", ingestKey, []byte(spans)); code != 200 {
		t.Fatalf("traces: %d", code)
	}

	opts, err := clickhouse.ParseDSN(wcfg.Common.ClickHouseURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer cleanupBlobs(t, conn, svc)

	checks := []struct{ name, q string }{
		{"replay_segments", fmt.Sprintf("SELECT count() FROM replay_segments WHERE service = '%s-web' AND session_id = '%s' AND events = 2 AND bytes > 0 AND blob_key LIKE '1/1/%%/%s/00000_.json.gz'", svc, session, session)},
		{"replay_sessions", fmt.Sprintf("SELECT toUInt64(sum(segments) = 2 AND sum(clicks) = 4 AND sum(errors) = 1 AND argMinMerge(first_view) = '/checkout' AND max(user_id) = '' AND max(anonymous_id) = 'anon-1') FROM replay_sessions WHERE service = '%s-web' AND session_id = '%s'", svc, session)},
		{"profiles pprof", fmt.Sprintf("SELECT count() FROM profiles WHERE service = '%s' AND runtime = 'go' AND type = 'cpu' AND unit = 'nanoseconds' AND total = 60000000 AND stacks = 2", svc)},
		{"profile_stacks route", fmt.Sprintf("SELECT count() FROM profile_stacks WHERE service = '%s' AND type = 'cpu' AND route = '/users/:id' AND method = 'GET' AND stack = ['main.main', 'main.handler'] AND value = 40000000 AND stack_hash = cityHash64(stack)", svc)},
		{"profile_stacks folded", fmt.Sprintf("SELECT count() FROM profile_stacks WHERE service = '%s' AND runtime = 'jvm' AND type = 'wall' AND route = '/orders/:id' AND length(stack) = 3 AND value = 7000000", svc)},
		{"spans ai dep", fmt.Sprintf("SELECT count() FROM spans WHERE service = '%s' AND dep_kind = 'ai' AND dep = 'openai · gpt-4o-2024-08-06' AND entry_route = '/chat' AND NOT mapContains(attrs, 'gen_ai.input.messages')", svc)},
		{"http under llm skipped", fmt.Sprintf("SELECT toUInt64(countIf(dep = '') = 1) FROM spans WHERE service = '%s' AND kind = 'client'", svc)},
		{"llm_content", fmt.Sprintf("SELECT count() FROM llm_content WHERE service = '%s' AND input LIKE '%%hi%%' AND output LIKE '%%hello%%'", svc)},
		{"llm_1m", fmt.Sprintf("SELECT toUInt64(sum(calls) = 1 AND sum(input_tokens) = 1200 AND sum(output_tokens) = 80) FROM llm_1m WHERE service = '%s' AND provider = 'openai' AND model = 'gpt-4o-2024-08-06' AND operation = 'chat' AND route = '/chat' AND method = 'POST'", svc)},
		{"llm_1m ttft", fmt.Sprintf("SELECT toUInt64(quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(ttft_hist)[1] = 240.5) FROM llm_1m WHERE service = '%s'", svc)},
		{"edges_1m ai (internal span)", fmt.Sprintf("SELECT toUInt64(sum(calls)) FROM edges_1m WHERE parent_service = '%s' AND dep_kind = 'ai' AND parent_route = '/chat'", svc)},
	}
	deadline := time.Now().Add(60 * time.Second)
	for _, c := range checks {
		for {
			var n uint64
			err := conn.QueryRow(ctx, c.q).Scan(&n)
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

	// The S3 object is the gzip'ed segment as sent.
	var key string
	if err := conn.QueryRow(ctx, fmt.Sprintf("SELECT blob_key FROM replay_segments WHERE session_id = '%s' AND segment = 1 LIMIT 1", session)).Scan(&key); err != nil {
		t.Fatal(err)
	}
	s3c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(wcfg.S3.Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(wcfg.S3.AccessKey, wcfg.S3.SecretKey, "")})
	obj, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(wcfg.S3.Bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("s3 get %s: %v", key, err)
	}
	defer func() {
		obj.Body.Close()
		_, _ = s3c.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: aws.String(wcfg.S3.Bucket), Key: aws.String(key)})
	}()
	zr, err := gzip.NewReader(obj.Body)
	if err != nil {
		t.Fatalf("s3 object is not gzip: %v", err)
	}
	seg, _ := io.ReadAll(zr)
	if !strings.Contains(string(seg), `"segment":1`) || aws.ToString(obj.ContentEncoding) != "gzip" {
		t.Fatalf("s3 object: %q (encoding %q)", seg, aws.ToString(obj.ContentEncoding))
	}
	t.Logf("✓ s3 %s (%d bytes JSON)", key, len(seg))
}

func gzipBytes(s string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return buf.Bytes()
}

// goProfile: cpu 40 ms on main.main → main.handler tagged /users/:id GET, 20 ms on main.main → main.gc.
func goProfile(t *testing.T) []byte {
	fMain := &profile.Function{ID: 1, Name: "main.main"}
	fH := &profile.Function{ID: 2, Name: "main.handler"}
	fGC := &profile.Function{ID: 3, Name: "main.gc"}
	l1 := &profile.Location{ID: 1, Line: []profile.Line{{Function: fMain}}}
	l2 := &profile.Location{ID: 2, Line: []profile.Line{{Function: fH}}}
	l3 := &profile.Location{ID: 3, Line: []profile.Line{{Function: fGC}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"}, Period: 10_000_000,
		Function: []*profile.Function{fMain, fH, fGC}, Location: []*profile.Location{l1, l2, l3},
		Sample: []*profile.Sample{
			{Value: []int64{2, 20_000_000}, Location: []*profile.Location{l2, l1}, Label: map[string][]string{"trooper.route": {"/users/:id"}, "trooper.method": {"GET"}}},
			{Value: []int64{2, 20_000_000}, Location: []*profile.Location{l2, l1}, Label: map[string][]string{"trooper.route": {"/users/:id"}, "trooper.method": {"GET"}}},
			{Value: []int64{2, 20_000_000}, Location: []*profile.Location{l3, l1}},
		},
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func cleanupBlobs(t *testing.T, conn clickhouse.Conn, svc string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	like := svc + "%"
	for _, s := range []string{
		"ALTER TABLE replay_segments DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE replay_sessions DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE profile_stacks DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE profiles DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE spans DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE llm_content DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE llm_1m DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE llm_1h DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE red_1m DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE red_1h DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE callers_1m DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE edges_1m DELETE WHERE project_id = 1 AND parent_service LIKE '%s'",
		"ALTER TABLE trace_index DELETE WHERE project_id = 1 AND arrayExists(s -> s LIKE '%s', services)",
	} {
		if err := conn.Exec(ctx, fmt.Sprintf(s, like)); err != nil {
			t.Logf("cleanup (ignored): %v", err)
		}
	}
}
