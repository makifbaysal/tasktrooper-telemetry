//go:build integration

// Session replay, continuous profiling and AI monitoring integration test (contracts §23–25). It owns org/project 9821,
// seeds Postgres + ClickHouse (+ one replay blob in the dev S3, SeaweedFS :9002) for that tenant and deletes what it wrote.
// Needs deploy/clickhouse/005_replay_profiles_llm.sql and deploy/postgres/012_llm_prices.sql applied.
// Run: GOWORK=off go test -tags integration ./internal/server/ -run IntegrationObs -v
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/blob"
	"github.com/tasktrooper/telemetry/services/api/internal/flame"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/config"
)

const obsKey = "tt_it_9821_api_key"

var obsSeedCH = []string{
	// replay: session rs1, 3 segments of 5 s starting 120 s ago
	`INSERT INTO replay_segments (org_id, project_id, session_id, segment, service, env, version, platform, format, start_ts, end_ts, events, bytes,
	   blob_key, has_full_snapshot, clicks, errors, rage_clicks, view_name, user_id, anonymous_id, received_at)
	 SELECT 9821, 9821, 'rs1', number, 'it-web', 'prod', '1.4.0', 'web', 'rrweb', now64(3) - toIntervalSecond(120 - number * 5),
	   now64(3) - toIntervalSecond(115 - number * 5), 10, 100, concat('9821/9821/', toString(toDate(now())), '/rs1/', leftPad(toString(number), 6, '0'), '.json.gz'),
	   number = 0, 2, if(number = 1, 1, 0), 0, if(number = 0, '/home', '/checkout'), '', 'anon-1', now64(3)
	 FROM numbers(3)`,
	// a duplicate delivery of segment 2 (argMax(received_at) dedupes it)
	`INSERT INTO replay_segments (org_id, project_id, session_id, segment, service, env, version, platform, format, start_ts, end_ts, events, bytes,
	   blob_key, has_full_snapshot, clicks, errors, rage_clicks, view_name, user_id, anonymous_id, received_at)
	 VALUES (9821, 9821, 'rs1', 2, 'it-web', 'prod', '1.4.0', 'web', 'rrweb', now64(3) - toIntervalSecond(110), now64(3) - toIntervalSecond(105), 12, 120,
	   concat('9821/9821/', toString(toDate(now())), '/rs1/000002.json.gz'), 0, 0, 0, 0, '/checkout', '', 'anon-1', now64(3) + toIntervalSecond(1))`,
	`INSERT INTO rum_events (org_id, project_id, ts, received_at, app, env, platform, type, session_id, view_id, view_name, anonymous_id, user_id,
	   country, device_type, os, browser, app_version, duration_ms, action_name, error_type, error_message, error_fingerprint, resource_url,
	   resource_method, status_code, trace_id)
	 VALUES
	  (9821, 9821, now64(3) - toIntervalSecond(119), now64(3), 'it-web', 'prod', 'web', 'view_start', 'rs1', 'v1', '/home', 'anon-1', 'u-42', 'TR', 'desktop', 'macOS', 'Chrome', '1.4.0', 350, '', '', '', 0, '', '', 0, ''),
	  (9821, 9821, now64(3) - toIntervalSecond(117), now64(3), 'it-web', 'prod', 'web', 'action', 'rs1', 'v1', '/home', 'anon-1', 'u-42', 'TR', 'desktop', 'macOS', 'Chrome', '1.4.0', 0, 'click on Buy', '', '', 0, '', '', 0, ''),
	  (9821, 9821, now64(3) - toIntervalSecond(114), now64(3), 'it-web', 'prod', 'web', 'view_start', 'rs1', 'v2', '/checkout', 'anon-1', 'u-42', 'TR', 'desktop', 'macOS', 'Chrome', '1.4.0', 200, '', '', '', 0, '', '', 0, ''),
	  (9821, 9821, now64(3) - toIntervalSecond(112), now64(3), 'it-web', 'prod', 'web', 'error', 'rs1', 'v2', '/checkout', 'anon-1', 'u-42', 'TR', 'desktop', 'macOS', 'Chrome', '1.4.0', 0, '', 'TypeError', 'x is undefined', 555, '', '', 0, ''),
	  (9821, 9821, now64(3) - toIntervalSecond(110), now64(3), 'it-web', 'prod', 'web', 'resource', 'rs1', 'v2', '/checkout', 'anon-1', 'u-42', 'TR', 'desktop', 'macOS', 'Chrome', '1.4.0', 80, '', '', '', 0, '/api/orders', 'POST', 500, MD5('t1')),
	  (9821, 9821, now64(3) - toIntervalSecond(100), now64(3), 'it-web', 'prod', 'web', 'view_start', 'rs2', 'v9', '/home', 'anon-2', '', 'DE', 'mobile', 'iOS', 'Safari', '1.4.0', 500, '', '', '', 0, '', '', 0, '')`,
	// backend error in the trace the browser request started
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry, route, http_method, status_code,
	   is_error, error_type, error_fingerprint, entry_route, entry_method, attrs)
	 VALUES (9821, 9821, now64(6) - toIntervalSecond(110), 30000000, MD5('t1'), substring(MD5('t1s'), 1, 8), 'it-orders', 'prod', 'server', 'POST /api/orders',
	   1, '/api/orders', 'POST', 500, 1, 'OrderError', 888, '/api/orders', 'POST', map('exception.message', 'boom'))`,
	// AI: it-ai POST /chat → one LLM span each; 10 × gpt-4o (one failed), 5 × an unpriced model
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, service, env, kind, name, is_entry, route, http_method, status_code,
	   entry_route, entry_method)
	 SELECT 9821, 9821, now64(6) - toIntervalSecond(60 + number * 10), 900000000, MD5(concat('ai', toString(number))), substring(MD5(concat('ai', toString(number))), 1, 8),
	   'it-ai', 'prod', 'server', 'POST /chat', 1, '/chat', 'POST', 200, '/chat', 'POST'
	 FROM numbers(15)`,
	`INSERT INTO spans (org_id, project_id, ts, duration_ns, trace_id, span_id, parent_span_id, service, env, kind, name, is_entry, is_error, error_type,
	   entry_route, entry_method, dep, dep_kind, attrs)
	 SELECT 9821, 9821, now64(6) - toIntervalSecond(60 + number * 10) + toIntervalMillisecond(5), (400 + number * 10) * 1000000,
	   MD5(concat('ai', toString(number))), substring(MD5(concat('llm', toString(number))), 1, 8), substring(MD5(concat('ai', toString(number))), 1, 8),
	   'it-ai', 'prod', 'client', 'chat', 0, number = 3, if(number = 3, 'RateLimitError', ''), '/chat', 'POST',
	   if(number < 10, 'openai · gpt-4o-2024-08-06', 'acme · mystery-1'), 'ai',
	   map('gen_ai.operation.name', 'chat', 'gen_ai.provider.name', if(number < 10, 'openai', 'acme'),
	       'gen_ai.request.model', if(number < 10, 'gpt-4o', 'mystery-1'), 'gen_ai.response.model', if(number < 10, 'gpt-4o-2024-08-06', ''),
	       'gen_ai.usage.input_tokens', if(number < 10, '1000', '100'), 'gen_ai.usage.output_tokens', if(number < 10, '500', '10'),
	       'gen_ai.response.finish_reasons', '["stop"]', 'trooper.llm.ttft_ms', if(number < 10, '150', ''))
	 FROM numbers(15)`,
	`INSERT INTO llm_content (org_id, project_id, ts, trace_id, span_id, service, system, input, output)
	 VALUES (9821, 9821, now64(6) - toIntervalSecond(60), MD5('ai0'), substring(MD5('llm0'), 1, 8), 'it-ai', 'be nice',
	   '[{"role":"user","parts":[{"type":"text","content":"hi"}]}]', '[{"role":"assistant","parts":[{"type":"text","content":"hello"}]}]')`,
	// profiling: it-prof cpu, v1 30 min ago (a 1 s, b 1 s), v2 10 min ago (a 1 s, b 3 s, b tagged with route /x)
	`INSERT INTO profile_stacks (org_id, project_id, ts, service, env, version, host, runtime, type, unit, route, method, stack, value) VALUES
	  (9821, 9821, now() - 1800, 'it-prof', 'prod', 'v1', 'h1', 'go', 'cpu', 'nanoseconds', '', '', ['main', 'a'], 1000000000),
	  (9821, 9821, now() - 1800, 'it-prof', 'prod', 'v1', 'h1', 'go', 'cpu', 'nanoseconds', '', '', ['main', 'b'], 1000000000),
	  (9821, 9821, now() - 600, 'it-prof', 'prod', 'v2', 'h1', 'go', 'cpu', 'nanoseconds', '', '', ['main', 'a'], 1000000000),
	  (9821, 9821, now() - 600, 'it-prof', 'prod', 'v2', 'h1', 'go', 'cpu', 'nanoseconds', '/x', 'GET', ['main', 'b', 'b'], 3000000000)`,
	`INSERT INTO profiles (org_id, project_id, ts, end_ts, service, env, version, host, runtime, type, unit, total, stacks) VALUES
	  (9821, 9821, now64(3) - toIntervalSecond(1800), now64(3) - toIntervalSecond(1790), 'it-prof', 'prod', 'v1', 'h1', 'go', 'cpu', 'nanoseconds', 2000000000, 2),
	  (9821, 9821, now64(3) - toIntervalSecond(600), now64(3) - toIntervalSecond(590), 'it-prof', 'prod', 'v2', 'h1', 'go', 'cpu', 'nanoseconds', 4000000000, 2)`,
}

func obsCleanup(t *testing.T, pool *pgxpool.Pool, ch driver.Conn) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM orgs WHERE id = 9821`); err != nil {
		t.Errorf("pg cleanup: %v", err)
	}
	for _, tb := range []string{"replay_segments", "replay_sessions", "rum_events", "rum_views_1m", "rum_vitals_1m", "rum_resources_1m", "spans",
		"red_1m", "red_1h", "callers_1m", "edges_1m", "errors_1h", "trace_index", "deploys", "llm_1m", "llm_1h", "llm_content", "profile_stacks", "profiles"} {
		if err := ch.Exec(ctx, fmt.Sprintf("ALTER TABLE telemetry.`%s` DELETE WHERE project_id = 9821 SETTINGS mutations_sync = 1", tb)); err != nil &&
			!strings.Contains(err.Error(), "UNKNOWN_TABLE") {
			t.Errorf("ch cleanup %s: %v", tb, err)
		}
	}
}

// obsS3 returns a writable client on the dev S3 (skips the blob checks when unreachable).
func obsS3(t *testing.T) (*s3.Client, string) {
	ep := config.String("TT_S3_ENDPOINT", "http://localhost:9002")
	bucket := config.String("TT_REPLAY_S3_BUCKET", "tt-replays")
	c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(ep), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider(config.String("TT_S3_ACCESS_KEY", "trooper"), config.String("TT_S3_SECRET_KEY", "trooper-secret"), ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err != nil {
		_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
			t.Logf("s3 unavailable (%v): segment blob checks skipped", err)
			return nil, bucket
		}
	}
	return c, bucket
}

func TestIntegrationObs(t *testing.T) {
	pool, ch := itEnv(t)
	defer ch.Close()
	defer pool.Close()
	ctx := context.Background()
	var n uint64
	if err := ch.QueryRow(ctx, `SELECT count() FROM system.tables WHERE database = 'telemetry' AND name IN ('replay_segments', 'llm_1m', 'profile_stacks')`).Scan(&n); err != nil || n != 3 {
		t.Skipf("deploy/clickhouse/005_replay_profiles_llm.sql not applied (%v)", err)
	}
	obsCleanup(t, pool, ch)
	defer obsCleanup(t, pool, ch)
	for _, q := range []string{
		`INSERT INTO orgs (id, name) VALUES (9821, 'it-9821')`,
		`INSERT INTO projects (id, org_id, name, slug) VALUES (9821, 9821, 'it', 'it-9821')`,
		`INSERT INTO api_keys (project_id, name, kind, hash, prefix) VALUES (9821, 'it', 'api', digest('` + obsKey + `', 'sha256'), 'tt_it_98')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("pg seed: %v", err)
		}
	}
	for _, q := range obsSeedCH {
		if err := ch.Exec(ctx, q); err != nil {
			t.Fatalf("ch seed: %v\n%s", err, q)
		}
	}

	segBody := []byte(`{"session_id":"rs1","segment":1,"format":"rrweb","events":[{"type":3,"data":{},"timestamp":1}]}`)
	var gzBody bytes.Buffer
	zw := gzip.NewWriter(&gzBody)
	_, _ = zw.Write(segBody)
	_ = zw.Close()
	s3c, bucket := obsS3(t)
	segKey := fmt.Sprintf("9821/9821/%s/rs1/000001.json.gz", time.Now().UTC().Format("2006-01-02"))
	srv := &Server{Store: store.New(ch, pool), Auth: auth.NewPG(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Origins: []string{"http://localhost:5173"}}
	if s3c != nil {
		if _, err := s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &segKey, Body: bytes.NewReader(gzBody.Bytes()),
			ContentLength: aws.Int64(int64(gzBody.Len()))}); err != nil {
			t.Fatalf("s3 put: %v", err)
		}
		defer s3c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &segKey})
		srv.Replays = blob.NewS3(blob.Config{Endpoint: config.String("TT_S3_ENDPOINT", "http://localhost:9002"), Bucket: bucket,
			AccessKey: config.String("TT_S3_ACCESS_KEY", "trooper"), SecretKey: config.String("TT_S3_SECRET_KEY", "trooper-secret")})
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := itClient{t: t, base: ts.URL, key: obsKey}

	t.Run("replays", func(t *testing.T) {
		c.t = t
		var list model.ReplayList
		c.json("GET", "/replays?app=it-web&range=1h", nil, 200, &list)
		if len(list.Replays) != 1 {
			t.Fatalf("replays: %+v", list)
		}
		r := list.Replays[0]
		if r.SessionID != "rs1" || r.App != "it-web" || r.Format != "rrweb" || r.Segments < 3 || r.User.ID != "u-42" || r.User.AnonymousID != "anon-1" ||
			r.Device != "desktop" || r.Browser != "Chrome" || r.OS != "macOS" || r.Country != "TR" || r.AppVersion != "1.4.0" || r.FirstView != "/home" ||
			r.Views != 2 || r.Errors != 1 || r.Clicks < 6 || math.Abs(r.DurationMs-15000) > 1500 {
			t.Errorf("summary: %+v", r)
		}
		for q, want := range map[string]int{"&q=u-42": 1, "&q=checkout": 1, "&q=nobody": 0, "&hasErrors=true": 1, "&minDurationS=60": 0} {
			c.json("GET", "/replays?range=1h"+q, nil, 200, &list)
			if len(list.Replays) != want {
				t.Errorf("filter %s: %d replays", q, len(list.Replays))
			}
		}
		var d model.ReplayDetail
		c.json("GET", "/replays/rs1", nil, 200, &d)
		if len(d.Segments) != 3 || d.Segments[2].Events != 12 || !d.Segments[0].HasFullSnapshot || d.Summary.User.ID != "u-42" {
			t.Errorf("detail segments: %+v / %+v", d.Segments, d.Summary)
		}
		kinds := map[string]model.TimelineItem{}
		for _, it := range d.Timeline {
			kinds[it.Kind] = it
		}
		if len(d.Timeline) != 5 || kinds["error"].Fingerprint != "555" || kinds["resource"].TraceID == "" || kinds["resource"].Status != 500 ||
			kinds["action"].Label != "click on Buy" || kinds["view"].DurationMs == nil {
			t.Errorf("timeline: %+v", d.Timeline)
		}
		if code, _ := c.do("GET", "/replays/nope", nil); code != 404 {
			t.Errorf("unknown session: %d", code)
		}
		var sessions []model.Session
		c.json("GET", "/apps/it-web/sessions?range=1h", nil, 200, &sessions)
		got := map[string]bool{}
		for _, s := range sessions {
			got[s.ID] = s.HasReplay
		}
		if len(got) != 2 || !got["rs1"] || got["rs2"] {
			t.Errorf("hasReplay: %+v", got)
		}
		var re model.RumErrorDetail
		c.json("GET", "/apps/it-web/errors/555?range=1h", nil, 200, &re)
		if re.Replay == nil || re.Replay.SessionID != "rs1" || math.Abs(float64(re.Replay.OffsetMs)-8000) > 1500 {
			t.Errorf("rum error replay: %+v", re.Replay)
		}
		var be model.ErrorDetail
		c.json("GET", "/errors/888?range=1h", nil, 200, &be)
		if be.Replay == nil || be.Replay.SessionID != "rs1" || math.Abs(float64(be.Replay.OffsetMs)-10000) > 1500 {
			t.Errorf("backend error replay: %+v", be.Replay)
		}
	})

	t.Run("replay segment blob", func(t *testing.T) {
		c.t = t
		if s3c == nil {
			t.Skip("no s3")
		}
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/replays/rs1/segments/1", nil)
		req.Header.Set("Authorization", "Bearer "+obsKey)
		tr := &http.Transport{DisableCompression: true} // keep the gzip body as sent
		res, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(raw, gzBody.Bytes()) {
			t.Errorf("segment: %d %q %d bytes", res.StatusCode, res.Header.Get("Content-Encoding"), len(raw))
		}
		if code, _ := c.do("GET", "/replays/rs1/segments/0", nil); code != 404 { // indexed, blob never uploaded
			t.Errorf("missing blob: %d", code)
		}
		if code, _ := c.do("GET", "/replays/rs1/segments/9", nil); code != 404 {
			t.Errorf("unknown segment: %d", code)
		}
	})

	t.Run("profiles", func(t *testing.T) {
		c.t = t
		var types model.ProfileTypes
		c.json("GET", "/profiles/types?service=it-prof&range=1h", nil, 200, &types)
		if len(types.Types) != 1 || types.Types[0].Type != "cpu" || types.Types[0].Unit != "nanoseconds" || types.Types[0].Runtime != "go" ||
			strings.Join(types.Types[0].Hosts, ",") != "h1" || strings.Join(types.Types[0].Versions, ",") != "v1,v2" || types.Types[0].Routes != 1 {
			t.Errorf("types: %+v", types)
		}
		var fg model.Flamegraph
		c.json("GET", "/profiles/flamegraph?service=it-prof&type=cpu&range=1h", nil, 200, &fg)
		if fg.Total != 6e9 || fg.Profiles != 2 || fg.Root.Name != flame.RootName || len(fg.Root.Children) != 1 {
			t.Fatalf("flamegraph: %+v", fg)
		}
		main := fg.Root.Children[0]
		if main.Value != 6e9 || len(main.Children) != 2 || main.Children[0].Name != "b" || main.Children[0].Value != 4e9 || main.Children[1].Value != 2e9 {
			t.Errorf("flamegraph main: %+v", main)
		}
		c.json("GET", "/profiles/flamegraph?service=it-prof&type=cpu&range=1h&route=/x&method=get", nil, 200, &fg)
		if fg.Total != 3e9 {
			t.Errorf("route filter total: %d", fg.Total)
		}
		var diff model.FlameDiff
		c.json("GET", "/profiles/diff?service=it-prof&type=cpu&baseVersion=v1&version=v2", nil, 200, &diff)
		if diff.Total != 4e9 || diff.BaseTotal != 2e9 || diff.Root.Children[0].Children[0].Name != "b" || diff.Root.Children[0].Children[0].BaseValue != 1e9 {
			t.Errorf("diff: %+v", diff)
		}
		now := time.Now().UnixMilli()
		c.json("GET", fmt.Sprintf("/profiles/diff?service=it-prof&type=cpu&baseFrom=%d&baseTo=%d&from=%d&to=%d", now-3600_000, now-1200_000, now-1200_000, now),
			nil, 200, &diff)
		if diff.Total != 4e9 || diff.BaseTotal != 2e9 {
			t.Errorf("window diff: %d / %d", diff.Total, diff.BaseTotal)
		}
		var top model.TopFunctions
		c.json("GET", "/profiles/top?service=it-prof&type=cpu&range=1h", nil, 200, &top)
		if top.Total != 6e9 || len(top.Functions) != 3 || top.Functions[0].Name != "b" || top.Functions[0].Self != 4e9 || top.Functions[0].Total != 4e9 {
			t.Errorf("top: %+v", top)
		}
		var s series
		c.json("GET", "/profiles/series?service=it-prof&type=cpu&range=1h", nil, 200, &s)
		if len(s.Series) != 1 || s.Series[0].ID != "h1" || math.Abs(s.sum("h1")-0.6) > 0.01 {
			t.Errorf("series: %+v", s)
		}
		if code, _ := c.do("GET", "/profiles/flamegraph?service=it-prof&type=heap", nil); code != 400 {
			t.Errorf("bad type: %d", code)
		}
	})

	t.Run("ai", func(t *testing.T) {
		c.t = t
		var ov model.AIOverview
		c.json("GET", "/ai/overview?range=1h", nil, 200, &ov)
		tot := ov.Totals
		if tot.Calls != 15 || tot.Errors != 1 || tot.InputTokens != 10500 || tot.OutputTokens != 5050 || tot.CostUsd == nil ||
			math.Abs(*tot.CostUsd-0.075) > 1e-9 || !tot.CostPartial || tot.P95Ms < 400 || tot.TtftP95Ms == nil || *tot.TtftP95Ms != 150 {
			t.Errorf("totals: %+v", tot)
		}
		if len(ov.ByModel) != 2 || ov.ByModel[0].Model != "gpt-4o-2024-08-06" || !ov.ByModel[0].PriceKnown || ov.ByModel[1].PriceKnown || ov.ByModel[1].CostUsd != nil {
			t.Errorf("byModel: %+v", ov.ByModel)
		}
		if len(ov.ByService) != 1 || ov.ByService[0].Service != "it-ai" || len(ov.ByService[0].Models) != 2 {
			t.Errorf("byService: %+v", ov.ByService)
		}
		if len(ov.ByEndpoint) != 1 || ov.ByEndpoint[0].Route != "/chat" || ov.ByEndpoint[0].Method != "POST" || ov.ByEndpoint[0].AvgInputTokens != 700 ||
			ov.ByEndpoint[0].CostPerCallUsd == nil || math.Abs(*ov.ByEndpoint[0].CostPerCallUsd-0.005) > 1e-9 {
			t.Errorf("byEndpoint: %+v", ov.ByEndpoint)
		}
		var calls float64
		for _, sr := range ov.Series.Calls.Series {
			if sr.ID == "calls" {
				calls = sumData(sr.Data)
			}
		}
		if calls != 15 || len(ov.Series.Cost.Series) != 1 || len(ov.Series.Latency.Series) != 1 {
			t.Errorf("series: calls %v, cost %+v", calls, ov.Series.Cost.Series)
		}
		c.json("GET", "/ai/overview?range=1h&provider=acme", nil, 200, &ov)
		if ov.Totals.Calls != 5 || ov.Totals.CostUsd != nil || !ov.Totals.CostPartial {
			t.Errorf("acme totals: %+v", ov.Totals)
		}

		var list model.AICalls
		c.json("GET", "/ai/calls?range=1h&limit=100", nil, 200, &list)
		var withContent *model.AICall
		for i, cl := range list.Calls {
			if cl.HasContent {
				cp := list.Calls[i]
				withContent = &cp
			}
		}
		if len(list.Calls) != 15 || withContent == nil || withContent.Route != "/chat" || withContent.FinishReason != "stop" || withContent.CostUsd == nil ||
			withContent.TtftMs == nil {
			t.Fatalf("calls: %d %+v", len(list.Calls), withContent)
		}
		c.json("GET", "/ai/calls?range=1h&errorsOnly=true", nil, 200, &list)
		if len(list.Calls) != 1 || list.Calls[0].Error != "RateLimitError" {
			t.Errorf("errors only: %+v", list.Calls)
		}
		var det model.AICallDetail
		c.json("GET", "/ai/calls/"+withContent.TraceID+"/"+withContent.SpanID, nil, 200, &det)
		if det.Content == nil || det.Content.System != "be nice" || !strings.Contains(det.Content.Output, "hello") || det.Attributes["gen_ai.provider.name"] != "openai" {
			t.Errorf("call detail: %+v", det)
		}
		var tr model.Trace
		c.json("GET", "/traces/"+withContent.TraceID, nil, 200, &tr)
		var llm *model.SpanLLM
		for _, sp := range tr.Spans {
			if sp.LLM != nil {
				llm = sp.LLM
			}
		}
		if llm == nil || llm.Provider != "openai" || llm.InputTokens != 1000 || llm.CostUsd == nil || !llm.HasContent || llm.Operation != "chat" {
			t.Errorf("trace llm: %+v", llm)
		}
		var svc model.ServiceDetail
		c.json("GET", "/services/it-ai?range=1h", nil, 200, &svc)
		if !svc.HasAi {
			t.Errorf("it-ai hasAi false")
		}
		c.json("GET", "/services/it-orders?range=1h", nil, 200, &svc)
		if svc.HasAi {
			t.Errorf("it-orders hasAi true")
		}

		// llm monitors (contracts §25): create / validate / preview
		llmMon := func(q map[string]any, cond map[string]any) map[string]any {
			return map[string]any{"name": "it llm", "kind": "llm", "target": map[string]any{"service": "it-ai"}, "query": q,
				"condition": cond, "severity": "warning", "channels": []string{}}
		}
		c.json("POST", "/monitors", llmMon(map[string]any{"metric": "cost_usd_per_hour", "filter": map[string]any{"service": "it-ai"}},
			map[string]any{"op": ">", "threshold": 20, "window": "1h"}), 201, nil)
		c.json("POST", "/monitors", llmMon(map[string]any{"metric": "gb_seconds"}, map[string]any{"op": ">", "threshold": 1}), 400, nil)
		var pv model.Preview
		maxOf := func(xs []float64) float64 {
			m := 0.0
			for _, x := range xs {
				m = math.Max(m, x)
			}
			return m
		}
		c.json("POST", "/monitors/preview?range=1h", llmMon(map[string]any{"metric": "calls", "filter": map[string]any{}},
			map[string]any{"op": ">", "threshold": 10, "window": "1h"}), 200, &pv)
		if maxOf(pv.Data) != 15 || pv.Episodes != 1 {
			t.Errorf("llm calls preview: max %v episodes %d %v", maxOf(pv.Data), pv.Episodes, pv.Data)
		}
		c.json("POST", "/monitors/preview?range=1h", llmMon(map[string]any{"metric": "cost_usd", "filter": map[string]any{}},
			map[string]any{"op": ">", "threshold": 1, "window": "1h"}), 200, &pv)
		if math.Abs(maxOf(pv.Data)-0.075) > 1e-9 || pv.Episodes != 0 {
			t.Errorf("llm cost preview: max %v", maxOf(pv.Data))
		}
		c.json("POST", "/monitors/preview?range=1h", llmMon(map[string]any{"metric": "p95_ms", "filter": map[string]any{"provider": "openai"}},
			map[string]any{"op": ">", "threshold": 1, "minRequests": 1000}), 200, &pv)
		if maxOf(pv.Data) < 400 || pv.Episodes != 0 {
			t.Errorf("llm p95 preview (minRequests): max %v episodes %d", maxOf(pv.Data), pv.Episodes)
		}

		// org price override → cost changes; delete → default again
		c.json("PUT", "/ai/prices", map[string]any{"provider": "acme", "model": "mystery", "inputPerMTok": 1000, "outputPerMTok": 0}, 200, nil)
		var prices model.AIPrices
		c.json("GET", "/ai/prices", nil, 200, &prices)
		if len(prices.Prices) == 0 || prices.Prices[0].Source != "org" || prices.Prices[0].Model != "mystery" || prices.Updated == "" {
			t.Errorf("prices: %+v", prices.Prices[:1])
		}
		c.json("GET", "/ai/overview?range=1h", nil, 200, &ov)
		if ov.Totals.CostUsd == nil || math.Abs(*ov.Totals.CostUsd-(0.075+0.5)) > 1e-9 || ov.Totals.CostPartial {
			t.Errorf("cost with override: %v partial=%v", ov.Totals.CostUsd, ov.Totals.CostPartial)
		}
		c.json("DELETE", "/ai/prices?provider=acme&model=mystery", nil, 204, nil)
		if code, _ := c.do("DELETE", "/ai/prices?provider=acme&model=mystery", nil); code != 404 {
			t.Errorf("second delete: %d", code)
		}
		if code, _ := c.do("PUT", "/ai/prices", map[string]any{"provider": "acme", "model": "", "inputPerMTok": 1}); code != 400 {
			t.Errorf("invalid price: %d", code)
		}
	})
}
