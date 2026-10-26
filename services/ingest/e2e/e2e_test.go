//go:build e2e

// End-to-end test against the local infra (deploy/docker-compose.yml): starts ingest-gw and ingest-writer
// in-process, sends spans with the real OpenTelemetry Go SDK (OTLP/HTTP), a RUM JSON batch, OTLP/JSON
// logs and metrics, a signed Vercel drain request (logs + speed insights), broker topic / queue-age metrics,
// then waits until the rows show up in ClickHouse (raw tables and rollups). Test data uses the service
// prefix "ingest2-e2e-" in project 1 and is deleted at the end (plus the temporary Vercel integration row).
//
//	cd services/ingest && go test -tags e2e -v ./e2e/
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry/services/ingest/internal/app"
	"github.com/tasktrooper/telemetry/services/ingest/internal/geo/geotest"
	"github.com/tasktrooper/telemetry/services/ingest/internal/vercel"
)

const ingestKey = "tt_dev_ingest_key"

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := fmt.Sprintf("ingest2-e2e-%d", rand.Uint32())

	// GeoIP: a tiny generated database (no real one is bundled).
	geoPath := filepath.Join(t.TempDir(), "geo.mmdb")
	if err := geotest.Write(geoPath, map[string]string{"203.0.113.0/24": "NL"}); err != nil {
		t.Fatal(err)
	}

	// Vercel integration row (project 1) whose drain secret comes from an env reference.
	const drainSecret = "e2e-drain-secret"
	t.Setenv("TT_E2E_VERCEL_DRAIN_SECRET", drainSecret)
	pg, err := pgxpool.New(ctx, app.GatewayFromEnv().Common.PostgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var integrationID int64
	cfgJSON := fmt.Sprintf(`{"projectMap":{"%[1]s-vercel":"%[1]s-vercel","prj_%[1]s":"%[1]s-vweb"},"drainSecretRef":"env:TT_E2E_VERCEL_DRAIN_SECRET","verifyToken":"tok-%[1]s"}`, svc)
	if err := pg.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config, status) VALUES (1, 'vercel', $1, $2::jsonb, 'ok') RETURNING id`,
		svc, cfgJSON).Scan(&integrationID); err != nil {
		t.Fatalf("insert integration: %v", err)
	}
	defer func() { _, _ = pg.Exec(context.Background(), `DELETE FROM integrations WHERE id = $1`, integrationID) }()

	// Writer (in-process, standard consumer group).
	wcfg := app.WriterFromEnv()
	wcfg.HealthAddr, wcfg.MetricsAddr, wcfg.GeoIPDB = "", "", geoPath
	wcfg.Writer.FlushInterval = 500 * time.Millisecond
	wctx, wstop := context.WithCancel(ctx)
	wdone := make(chan error, 1)
	go func() { wdone <- app.RunWriter(wctx, wcfg, log) }()
	defer func() { wstop(); <-wdone }()

	// Gateway on random ports.
	gcfg := app.GatewayFromEnv()
	gcfg.GW.HTTPAddr, gcfg.GW.GRPCAddr = "127.0.0.1:0", "127.0.0.1:0"
	gcfg.MetricsAddr = "" // /metrics on the HTTP port
	addrs := make(chan [2]string, 1)
	gctx, gstop := context.WithCancel(ctx)
	gdone := make(chan error, 1)
	go func() {
		gdone <- app.RunGateway(gctx, gcfg, log, func(h, g string) { addrs <- [2]string{h, g} })
	}()
	defer func() { gstop(); <-gdone }()
	var httpAddr string
	select {
	case a := <-addrs:
		httpAddr = a[0]
	case err := <-gdone:
		t.Fatalf("gateway failed: %v", err)
	}
	base := "http://" + httpAddr

	t.Logf("service %s, gateway %s", svc, base)

	// Auth: unknown key → 401.
	if code := post(t, base+"/v1/logs", "application/json", "nope", []byte(`{}`)); code != 401 {
		t.Fatalf("bad key: got %d, want 401", code)
	}

	// 1) Spans via the real OTel SDK + OTLP/HTTP protobuf exporter.
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(httpAddr), otlptracehttp.WithInsecure(),
		otlptracehttp.WithHeaders(map[string]string{"x-trooper-key": ingestKey}), otlptracehttp.WithCompression(otlptracehttp.GzipCompression))
	if err != nil {
		t.Fatal(err)
	}
	res := resource.NewSchemaless(attribute.String("service.name", svc), attribute.String("service.version", "1.2.3"),
		attribute.String("deployment.environment.name", "e2e"))
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exp))
	tr := tp.Tracer("e2e")
	for i := range 5 {
		sctx, srv := tr.Start(ctx, "GET /orders/:id", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
			attribute.String("http.request.method", "GET"), attribute.String("url.path", fmt.Sprintf("/orders/%d", 1000+i)),
			attribute.Int("http.response.status_code", 200), attribute.String("user_agent.original", "Mozilla/5.0 (Macintosh) Chrome/128.0 Safari/537.36")))
		_, db := tr.Start(sctx, "SELECT orders", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
			attribute.String("db.system", "postgresql"), attribute.String("db.namespace", "orders"),
			attribute.String("db.query.text", fmt.Sprintf("SELECT * FROM orders WHERE id = %d", 1000+i))))
		db.End()
		srv.End()
	}
	_, bad := tr.Start(ctx, "POST /checkout", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		attribute.String("http.request.method", "POST"), attribute.String("http.route", "/checkout"),
		attribute.Int("http.response.status_code", 500), attribute.String("trooper.caller", "shop-web")))
	bad.RecordError(errors.New("payment declined for order 77"))
	bad.SetStatus(codes.Error, "payment declined")
	bad.End()
	// SQS consumer span: messaging.system is normalized into spans.messaging_system / consumer_1m.
	_, cons := tr.Start(ctx, "emails process", trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(
		attribute.String("messaging.system", "AWS.SQS"), attribute.String("messaging.destination.name", svc+"-emails"),
		attribute.String("trooper.process", "send-email")))
	cons.End()
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("otel export: %v", err)
	}

	// 2) RUM JSON batch (protojson, proto field names), key in the query string like sendBeacon.
	now := time.Now().UnixMilli()
	rum := fmt.Sprintf(`{"resource":{"app":"%[1]s-web","version":"2.0.0","environment":"e2e","platform":"PLATFORM_WEB"},
	 "events":[
	  {"type":"VIEW_START","ts_ms":%[2]d,"session_id":"s1","view_id":"v1","view_name":"/products/:slug","anonymous_id":"a1","view":{"url":"https://shop.test/products/shoe?ref=x","loading_time_ms":812}},
	  {"type":"VITAL","ts_ms":%[2]d,"session_id":"s1","view_id":"v1","view_name":"/products/:slug","anonymous_id":"a1","vital":{"name":"LCP","value":1234.5,"rating":"good"}},
	  {"type":"RESOURCE","ts_ms":%[2]d,"session_id":"s1","view_id":"v1","view_name":"/products/:slug","resource":{"url":"https://api.shop.test/v1/orders/123?x=1","method":"GET","status_code":200,"duration_ms":88,"trace_id":"0af7651916cd43dd8448eb211c80319c"}},
	  {"type":"ERROR","ts_ms":%[2]d,"session_id":"s1","view_id":"v1","view_name":"/products/:slug","error":{"type":"TypeError","message":"x is undefined","stack":"TypeError: x is undefined\n    at render (https://shop.test/main.3f2a.js:10:5)","is_crash":false}}
	 ]}`, svc, now)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/rum?key="+ingestKey, strings.NewReader(rum))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://shop.test")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 Version/17.4 Mobile/15E148 Safari/604.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.7") // → NL via the generated GeoIP db
	if code := do(t, req); code != 200 {
		t.Fatalf("rum: got %d", code)
	}

	// 3) Logs via OTLP/JSON (hex ids).
	logs := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"%s"}}]},
	 "scopeLogs":[{"logRecords":[{"timeUnixNano":"%d","severityNumber":17,"body":{"stringValue":"order 42 failed after 3 retries"},
	 "traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331",
	 "attributes":[{"key":"trooper.entry.route","value":{"stringValue":"/checkout"}}]}]}]}]}`, svc, time.Now().UnixNano())
	if code := post(t, base+"/v1/logs", "application/json", ingestKey, []byte(logs)); code != 200 {
		t.Fatalf("logs: got %d", code)
	}

	// 4) Metrics via OTLP/JSON: a collector DB gauge and a custom metric.
	ts := time.Now().UnixNano()
	metrics := fmt.Sprintf(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"%[1]s"}}]},
	 "scopeMetrics":[{"metrics":[
	  {"name":"trooper.db.cpu","gauge":{"dataPoints":[{"timeUnixNano":"%[2]d","asDouble":0.42,"attributes":[
	    {"key":"trooper.db.id","value":{"stringValue":"%[1]s-db"}},{"key":"db.system","value":{"stringValue":"postgresql"}},{"key":"trooper.db.node","value":{"stringValue":"n1"}}]}]}},
	  {"name":"%[1]s.queue_depth","unit":"1","gauge":{"dataPoints":[{"timeUnixNano":"%[2]d","asInt":"7","attributes":[{"key":"queue","value":{"stringValue":"emails"}}]}]}}
	 ]}]},
	 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"trooper-cloud"}},{"key":"cloud.provider","value":{"stringValue":"aws"}},
	   {"key":"trooper.broker.cluster","value":{"stringValue":"%[1]s-sqs"}},{"key":"messaging.system","value":{"stringValue":"aws_sqs"}}]},
	 "scopeMetrics":[{"metrics":[
	  {"name":"trooper.broker.topic.in_rate","gauge":{"dataPoints":[{"timeUnixNano":"%[2]d","asDouble":12.5,"attributes":[{"key":"messaging.destination.name","value":{"stringValue":"emails"}}]}]}},
	  {"name":"trooper.broker.oldest_age_s","gauge":{"dataPoints":[{"timeUnixNano":"%[2]d","asDouble":310,"attributes":[{"key":"messaging.destination.name","value":{"stringValue":"emails"}}]}]}}
	 ]}]}]}`, svc, ts)
	if code := post(t, base+"/v1/metrics", "application/json", ingestKey, []byte(metrics)); code != 200 {
		t.Fatalf("metrics: got %d", code)
	}

	// 5) Vercel drain: verification, a bad signature, then a signed log + speed-insights NDJSON body.
	drainURL := fmt.Sprintf("%s/v1/drains/vercel?key=%s&integration=%d", base, ingestKey, integrationID)
	vreq, _ := http.NewRequest(http.MethodGet, drainURL, nil)
	vresp, err := http.DefaultClient.Do(vreq)
	if err != nil {
		t.Fatal(err)
	}
	vresp.Body.Close()
	if vresp.StatusCode != 200 || vresp.Header.Get("x-vercel-verify") != "tok-"+svc {
		t.Fatalf("drain verification: %d %q", vresp.StatusCode, vresp.Header.Get("x-vercel-verify"))
	}
	drain := fmt.Sprintf(`{"id":"1","deploymentId":"dpl_e2e","source":"lambda","host":"x.vercel.app","timestamp":%[2]d,"projectId":"prj_x","projectName":"%[1]s-vercel","level":"error","message":"vercel boom 42","path":"/api/orders/[id]","statusCode":500,"environment":"production","traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331"}
{"schema":"vercel.speed_insights.v1","timestamp":%[2]d,"projectId":"prj_%[1]s","ownerId":"team_x","deviceId":7,"metricType":"LCP","value":1834,"path":"/products/42","route":"/products/[id]","country":"TR","deviceType":"desktop","clientName":"Chrome","osName":"macOS","vercelEnvironment":"production"}
`, svc, time.Now().UnixMilli())
	sreq := func(sig string) int {
		r, _ := http.NewRequest(http.MethodPost, drainURL, strings.NewReader(drain))
		r.Header.Set("Content-Type", "application/x-ndjson")
		r.Header.Set("x-vercel-signature", sig)
		return do(t, r)
	}
	if code := sreq(vercel.Sign([]byte("wrong"), []byte(drain))); code != 403 {
		t.Fatalf("drain bad signature: got %d, want 403", code)
	}
	if code := sreq(vercel.Sign([]byte(drainSecret), []byte(drain))); code != 200 {
		t.Fatalf("drain: got %d", code)
	}

	// /metrics on the gateway HTTP port.
	mresp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	for _, want := range []string{`tt_ingest_gw_requests_total{endpoint="/v1/drains/vercel",status="200"}`, `tt_ingest_gw_auth_failures_total{endpoint="/v1/drains/vercel",reason="signature"}`} {
		if !strings.Contains(string(mb), want) {
			t.Errorf("gw /metrics lacks %s", want)
		}
	}

	// Wait for ClickHouse.
	opts, err := clickhouse.ParseDSN(wcfg.Common.ClickHouseURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer cleanup(t, conn, svc)

	checks := []struct{ name, q string }{
		{"spans", fmt.Sprintf("SELECT count() FROM spans WHERE service = '%s'", svc)},
		{"spans entry+route", fmt.Sprintf("SELECT count() FROM spans WHERE service = '%s' AND is_entry = 1 AND route = '/orders/:id' AND caller_kind = 'web'", svc)},
		{"spans dep", fmt.Sprintf("SELECT count() FROM spans WHERE service = '%s' AND dep = 'postgresql · orders' AND entry_route = '/orders/:id' AND db_query_hash != 0", svc)},
		{"spans error", fmt.Sprintf("SELECT count() FROM spans WHERE service = '%s' AND is_error = 1 AND error_fingerprint != 0 AND caller = 'shop-web'", svc)},
		{"red_1m", fmt.Sprintf("SELECT toUInt64(sum(count)) FROM red_1m WHERE service = '%s'", svc)},
		{"edges_1m", fmt.Sprintf("SELECT toUInt64(sum(calls)) FROM edges_1m WHERE parent_service = '%s'", svc)},
		{"errors_1h", fmt.Sprintf("SELECT count() FROM errors_1h WHERE service = '%s'", svc)},
		{"rum_events", fmt.Sprintf("SELECT count() FROM rum_events WHERE app = '%s-web'", svc)},
		{"rum resource url", fmt.Sprintf("SELECT count() FROM rum_events WHERE app = '%s-web' AND resource_url = 'https://api.shop.test/v1/orders/:id' AND browser LIKE 'Safari%%' AND device_type = 'mobile'", svc)},
		{"rum_views_1m", fmt.Sprintf("SELECT toUInt64(sum(views)) FROM rum_views_1m WHERE app = '%s-web'", svc)},
		{"logs", fmt.Sprintf("SELECT count() FROM logs WHERE service = '%s' AND severity = 'error' AND route = '/checkout' AND pattern_id != 0", svc)},
		{"db_1m", fmt.Sprintf("SELECT count() FROM db_1m WHERE database_id = '%s-db' AND cpu = 0.42", svc)},
		{"metrics", fmt.Sprintf("SELECT count() FROM metrics WHERE name = '%s.queue_depth' AND value = 7", svc)},
		{"spans messaging_system", fmt.Sprintf("SELECT count() FROM spans WHERE service = '%s' AND messaging_system = 'aws_sqs' AND is_entry = 1", svc)},
		{"consumer_1m sqs", fmt.Sprintf("SELECT toUInt64(sum(processed)) FROM consumer_1m WHERE service = '%s' AND messaging_system = 'aws_sqs'", svc)},
		{"rum country (GeoIP)", fmt.Sprintf("SELECT count() FROM rum_events WHERE app = '%s-web' AND country = 'NL'", svc)},
		{"broker_topics_1m", fmt.Sprintf("SELECT count() FROM broker_topics_1m WHERE cluster_id = '%s-sqs' AND topic = 'emails' AND in_rate = 12.5 AND isInfinite(partitions)", svc)},
		{"broker_lag oldest_age", fmt.Sprintf("SELECT count() FROM broker_lag_1m WHERE cluster_id = '%s-sqs' AND topic = 'emails' AND oldest_age_s = 310", svc)},
		{"vercel log drain", fmt.Sprintf("SELECT count() FROM logs WHERE service = '%s-vercel' AND severity = 'error' AND route = '/api/orders/:id' AND attrs['vercel.source'] = 'lambda' AND hex(trace_id) = '0AF7651916CD43DD8448EB211C80319C'", svc)},
		{"vercel speed insights", fmt.Sprintf("SELECT count() FROM rum_events WHERE app = '%s-vweb' AND type = 'vital' AND vital_name = 'LCP' AND vital_value = 1834 AND view_name = '/products/:id' AND country = 'TR'", svc)},
	}
	deadline := time.Now().Add(60 * time.Second)
	for _, c := range checks {
		for {
			var n uint64
			err := conn.QueryRow(ctx, c.q).Scan(&n)
			if err == nil && n > 0 {
				t.Logf("✓ %-18s %d", c.name, n)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no rows (err=%v): %s", c.name, err, c.q)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// cleanup removes this run's rows (service / app / cluster prefixed with the run's svc) from project 1.
func cleanup(t *testing.T, conn clickhouse.Conn, svc string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	like := svc + "%"
	stmts := []string{
		"ALTER TABLE spans DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE red_1m DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE red_1h DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE edges_1m DELETE WHERE project_id = 1 AND parent_service LIKE '%s'",
		"ALTER TABLE callers_1m DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE consumer_1m DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE errors_1h DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE trace_index DELETE WHERE project_id = 1 AND arrayExists(s -> s LIKE '%s', services)",
		"ALTER TABLE logs DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE log_patterns_1m DELETE WHERE project_id = 1 AND service LIKE '%s'",
		"ALTER TABLE metrics DELETE WHERE project_id = 1 AND (service LIKE '%[1]s' OR name LIKE '%[1]s')",
		"ALTER TABLE rum_events DELETE WHERE project_id = 1 AND app LIKE '%s'",
		"ALTER TABLE rum_views_1m DELETE WHERE project_id = 1 AND app LIKE '%s'",
		"ALTER TABLE rum_vitals_1m DELETE WHERE project_id = 1 AND app LIKE '%s'",
		"ALTER TABLE rum_resources_1m DELETE WHERE project_id = 1 AND app LIKE '%s'",
		"ALTER TABLE db_1m DELETE WHERE project_id = 1 AND database_id LIKE '%s'",
		"ALTER TABLE broker_topics_1m DELETE WHERE project_id = 1 AND cluster_id LIKE '%s'",
		"ALTER TABLE broker_lag_1m DELETE WHERE project_id = 1 AND cluster_id LIKE '%s'",
	}
	for _, s := range stmts {
		q := fmt.Sprintf(s, like)
		if err := conn.Exec(ctx, q); err != nil {
			t.Logf("cleanup (ignored): %v: %s", err, q)
		}
	}
}

func post(t *testing.T, url, ct, key string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("x-trooper-key", key)
	return do(t, req)
}

func do(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Logf("%s %s → %d %s", req.Method, req.URL.Path, resp.StatusCode, b)
	}
	return resp.StatusCode
}
