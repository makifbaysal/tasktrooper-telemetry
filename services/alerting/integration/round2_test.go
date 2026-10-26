//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/alerting/internal/conn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/engine"
	"github.com/tasktrooper/telemetry/services/alerting/internal/leader"
	"github.com/tasktrooper/telemetry/services/alerting/internal/notify"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
	"github.com/tasktrooper/telemetry/services/alerting/internal/stream"
)

// TestRound2 covers pending → quiet resolve, mute, maintenance windows, reminders (+ acknowledged), anomaly,
// composite, job_missed, consumer_lag oldest_age (SQS vs Kafka), persisted KPI rule state, a stream rule and
// the Teams payload, on org/project 9201.
func TestRound2(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	common := config.Load()
	pg, err := conn.Postgres(ctx, common.PostgresURL)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(pg.Close)
	ch, err := conn.ClickHouse(ctx, common.ClickHouseURL)
	if err != nil {
		t.Skipf("clickhouse unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	cleanup(t, ctx, pg, ch)
	if os.Getenv("TT_ITEST_KEEP") == "" {
		t.Cleanup(func() { cleanup(t, context.Background(), pg, ch) })
	}

	var mu sync.Mutex
	bodies := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.URL.Path] = append(bodies[r.URL.Path], string(b))
		mu.Unlock()
	}))
	defer srv.Close()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pg.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`INSERT INTO orgs (id, name) VALUES (9201, 'itest')`)
	exec(`INSERT INTO projects (id, org_id, name, slug) VALUES (9201, 9201, 'itest', 'itest')`)
	// the Teams webhook URL is stored sealed (secret:<id>), as the api does
	key, err := secrets.Key()
	if err != nil {
		t.Fatal(err)
	}
	nonce, sealed, err := secrets.Seal(key, pid, []byte(srv.URL+"/teams"))
	if err != nil {
		t.Fatal(err)
	}
	var secretID int64
	if err := pg.QueryRow(ctx, `INSERT INTO secrets (project_id, name, nonce, ciphertext) VALUES (9201, 'teams webhook', $1, $2) RETURNING id`, nonce, sealed).Scan(&secretID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO notification_channels (project_id, name, type, config) VALUES (9201, '#r2-hook', 'webhook', $1), (9201, '#r2-teams', 'teams', $2)`,
		map[string]any{"url": srv.URL + "/hook"}, map[string]any{"webhookUrl": fmt.Sprintf("secret:%d", secretID)})
	exec(`INSERT INTO broker_clusters (project_id, slug, name, kind, endpoint) VALUES
		(9201, 'r2-sqs', 'orders queue', 'sqs', 'https://sqs.eu-west-1.amazonaws.com'), (9201, 'r2-kafka', 'kafka', 'kafka', 'localhost:9092')`)

	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	to := now.Truncate(time.Minute)

	// ── ClickHouse fixtures ──
	ent := func(svc, route string, errEvery int) span {
		return span{service: svc, kind: "server", entry: true, route: route, method: "GET", entryRoute: route, caller: "web", durMs: 50, errEvery: errEvery, version: "v1"}
	}
	insertSpans(t, ctx, ch, ent("r2-api", "/pending", 2), to.Add(-10*time.Minute), 10, 20)
	insertSpans(t, ctx, ch, ent("r2-api", "/pending", 0), to.Add(time.Minute), 15, 20) // recovers later
	insertSpans(t, ctx, ch, ent("r2-api", "/muted", 2), to.Add(-10*time.Minute), 10, 20)
	insertSpans(t, ctx, ch, ent("r2-maint", "/maint", 2), to.Add(-10*time.Minute), 10, 20)
	insertSpans(t, ctx, ch, ent("r2-api", "/remind", 2), to.Add(-10*time.Minute), 30, 20)
	insertSpans(t, ctx, ch, ent("r2-api", "/acked", 2), to.Add(-10*time.Minute), 30, 20)
	insertSpans(t, ctx, ch, ent("r2-api", "/healthy", 0), to.Add(-10*time.Minute), 30, 20)
	// anomaly: ~1 % errors at the same minute-of-week over the last 4 weeks, 33 % now
	for k, every := range []int{100, 90, 110, 80} {
		c := to.Add(-time.Duration(k+1) * 7 * 24 * time.Hour)
		insertSpans(t, ctx, ch, ent("r2-anom", "/x", every), c.Add(-65*time.Minute), 125, 20)
	}
	insertSpans(t, ctx, ch, ent("r2-anom", "/x", 3), to.Add(-5*time.Minute), 5, 20)
	// jobs: r2-cron last ran 2 h ago (hourly), r2-cron-ok 10 min ago, /jobs/sync entry spans 3 min ago (every 15 min)
	cons := func(proc string) span {
		return span{service: "r2-jobs", kind: "consumer", entry: true, route: proc, topic: "jobs", group: proc, durMs: 10}
	}
	insertSpans(t, ctx, ch, cons("r2-cron"), to.Add(-2*time.Hour), 1, 5)
	insertSpans(t, ctx, ch, cons("r2-cron-ok"), to.Add(-10*time.Minute), 1, 5)
	insertSpans(t, ctx, ch, ent("r2-jobs", "/jobs/sync", 0), to.Add(-3*time.Minute), 1, 5)
	// queue age: SQS oldest message 900 s for the last 10 minutes; the Kafka group reports none
	mustExec(t, ctx, ch, `INSERT INTO telemetry.broker_lag_1m (org_id, project_id, cluster_id, consumer_group, topic, partition, minute, lag, produce_rate, oldest_age_s)
		SELECT 9201, 9201, 'r2-sqs', 'orders-worker', 'orders', 0, toDateTime(?) + toIntervalMinute(number), 500, 2, 900 FROM numbers(10)`, to.Add(-9*time.Minute).Unix())
	mustExec(t, ctx, ch, `INSERT INTO telemetry.broker_lag_1m (org_id, project_id, cluster_id, consumer_group, topic, partition, minute, lag, produce_rate, oldest_age_s)
		SELECT 9201, 9201, 'r2-kafka', 'g', 't', 0, toDateTime(?) + toIntervalMinute(number), 500, 2, -inf FROM numbers(10)`, to.Add(-9*time.Minute).Unix())

	// ── monitors ──
	ids := map[string]int64{}
	add := func(name, kind, target, query, cond string, channels ...string) {
		t.Helper()
		var id int64
		if err := pg.QueryRow(ctx, `INSERT INTO monitors (project_id, name, kind, target, query, condition, severity, channels, source)
			VALUES (9201, $1, $2, $3, $4, $5, 'critical', $6, 'manual') RETURNING id`, name, kind, target, query, cond, append([]string{}, channels...)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	er := `{"op":">","threshold":0.1,"forMin":0,"window":"5m"}`
	add("pending", "error_rate", `{"service":"r2-api","endpoint":"GET /pending"}`, `{}`, `{"op":">","threshold":0.1,"forMin":10,"window":"5m"}`, "#r2-hook")
	add("muted", "error_rate", `{"service":"r2-api","endpoint":"GET /muted"}`, `{}`, er, "#r2-hook")
	add("maint", "error_rate", `{"service":"r2-maint","endpoint":"GET /maint"}`, `{}`, er, "#r2-hook")
	add("remind", "error_rate", `{"service":"r2-api","endpoint":"GET /remind"}`, `{}`, `{"op":">","threshold":0.1,"window":"5m","renotifyMin":5}`, "#r2-hook", "teams:#r2-teams")
	add("acked", "error_rate", `{"service":"r2-api","endpoint":"GET /acked"}`, `{}`, `{"op":">","threshold":0.1,"window":"5m","renotifyMin":5}`, "#r2-hook")
	add("healthy", "error_rate", `{"service":"r2-api","endpoint":"GET /healthy"}`, `{}`, er)
	add("anomaly up", "anomaly", `{"service":"r2-anom","endpoint":"GET /x"}`, `{"metric":"error_rate","direction":"up","minDeviation":0.05}`, `{"threshold":4,"window":"5m"}`)
	add("anomaly down", "anomaly", `{"service":"r2-anom","endpoint":"GET /x"}`, `{"metric":"error_rate","direction":"down"}`, `{"threshold":4,"window":"5m"}`)
	add("cron missed", "job_missed", `{"service":"r2-jobs","process":"r2-cron"}`, `{"intervalMin":60,"graceMin":5}`, `{}`)
	add("cron ok", "job_missed", `{"service":"r2-jobs","process":"r2-cron-ok"}`, `{"intervalMin":60}`, `{}`)
	add("sync ok", "job_missed", `{"service":"r2-jobs","endpoint":"GET /jobs/sync"}`, `{"intervalMin":15}`, `{}`)
	add("sqs age", "consumer_lag", `{}`, `{"clusterId":"r2-sqs","group":"orders-worker","metric":"oldest_age","threshold":600,"forMin":5}`, `{}`)
	add("kafka age", "consumer_lag", `{}`, `{"clusterId":"r2-kafka","group":"g","metric":"oldest_age","threshold":600,"forMin":5}`, `{}`)
	add("stream boom", "log_match", `{"service":"r2-api"}`, `{"contains":"boom","minSeverity":"error"}`, `{"threshold":2,"window":"5m","stream":true}`)
	add("all down (and)", "composite", `{}`, `{"op":"and","monitorIds":[`+itoa(ids["muted"])+`,"`+itoa(ids["maint"])+`"]}`, `{}`)
	add("any (or)", "composite", `{}`, `{"op":"or","monitorIds":[`+itoa(ids["pending"])+`,`+itoa(ids["healthy"])+`]}`, `{}`)

	exec(`UPDATE monitors SET muted_until = $2 WHERE id = $1`, ids["muted"], now.Add(time.Hour))
	var mwID int64
	if err := pg.QueryRow(ctx, `INSERT INTO maintenance_windows (project_id, name, starts_at, ends_at, scope) VALUES (9201, 'db upgrade', $1, $2, '{"services":["r2-maint"]}') RETURNING id`,
		now.Add(-time.Hour), now.Add(time.Hour)).Scan(&mwID); err != nil {
		t.Fatal(err)
	}
	var kpiID, ruleID int64
	if err := pg.QueryRow(ctx, `INSERT INTO kpis (project_id, name, target, sli, good_when, objective, "window", source)
		VALUES (9201, 'Muted endpoint succeeds', '{"kind":"endpoint","service":"r2-api","ref":"GET /muted"}', 'availability', '{}', 0.99, '28d', 'manual') RETURNING id`).Scan(&kpiID); err != nil {
		t.Fatal(err)
	}
	if err := pg.QueryRow(ctx, `INSERT INTO kpi_alert_rules (kpi_id, kind, long_window, short_window, threshold, severity, channels)
		VALUES ($1, 'burn_rate', '1h', '5m', 6, 'critical', '{#r2-hook}') RETURNING id`, kpiID).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}

	// ── tick 1 ──
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := obs.NewRegistry()
	el := &leader.Elector{Pool: pg, Base: leader.DefaultBase + 9201_001, Shards: 1}
	defer el.Release()
	eng := engine.New(engine.Config{ProjectIDs: []int64{pid}, PublicURL: "http://dash.local"}, log, &store.Store{DB: pg}, ch, el, reg)
	eng.Now = func() time.Time { return now }
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	state := func(name string) (string, float64) {
		var s string
		var v *float64
		_ = pg.QueryRow(ctx, `SELECT state, last_value FROM monitors WHERE id = $1`, ids[name]).Scan(&s, &v)
		if v == nil {
			return s, -1
		}
		return s, *v
	}
	for name, want := range map[string]string{
		"pending": "pending", "muted": "firing", "maint": "firing", "remind": "firing", "acked": "firing", "healthy": "ok",
		"anomaly up": "firing", "anomaly down": "ok", "cron missed": "firing", "cron ok": "ok", "sync ok": "ok",
		"sqs age": "firing", "kafka age": "no_data", "stream boom": "ok", "all down (and)": "firing", "any (or)": "ok",
	} {
		s, v := state(name)
		t.Logf("%-16s state=%-8s value=%.4g", name, s, v)
		if s != want {
			t.Errorf("%s: state %s want %s", name, s, want)
		}
	}
	if _, v := state("cron missed"); v < 115 || v > 125 {
		t.Errorf("cron missed value %v, want ~119 min since last run", v)
	}
	if _, v := state("anomaly up"); v < 4 {
		t.Errorf("anomaly z %v", v)
	}

	type inc struct {
		id     int64
		state  string
		quiet  bool
		notifs int
	}
	incOf := func(name string) inc {
		var i inc
		_ = pg.QueryRow(ctx, `SELECT id, state, quiet, (SELECT count(*) FROM notifications n WHERE n.incident_id = i.id)
			FROM incidents i WHERE monitor_id = $1 ORDER BY id DESC LIMIT 1`, ids[name]).Scan(&i.id, &i.state, &i.quiet, &i.notifs)
		return i
	}
	if i := incOf("pending"); i.state != "pending" || i.notifs != 0 {
		t.Errorf("pending incident %+v", i)
	}
	evKinds := func(id int64) string {
		var s string
		_ = pg.QueryRow(ctx, `SELECT string_agg(kind || coalesce(':' || (data->>'reason'), ''), ',' ORDER BY id) FROM incident_events WHERE incident_id = $1`, id).Scan(&s)
		return s
	}
	if i := incOf("muted"); i.state != "firing" || i.notifs != 0 || evKinds(i.id) != "fired:muted,suppressed:muted" {
		t.Errorf("muted incident %+v events %s", i, evKinds(i.id))
	}
	if i := incOf("maint"); i.state != "firing" || i.notifs != 0 || !strings.Contains(evKinds(i.id), "suppressed:maintenance:"+itoa(mwID)) {
		t.Errorf("maintenance incident %+v events %s", i, evKinds(i.id))
	}
	if i := incOf("remind"); i.notifs != 2 {
		t.Errorf("remind firing notifications %d", i.notifs)
	}
	// KPI rule state persisted + incident by kpi_rule_id (maintenance does not cover r2-api)
	var rstate string
	var rlast *float64
	_ = pg.QueryRow(ctx, `SELECT state, last_value FROM kpi_alert_rules WHERE id = $1`, ruleID).Scan(&rstate, &rlast)
	var kpiInc int64
	_ = pg.QueryRow(ctx, `SELECT id FROM incidents WHERE kpi_rule_id = $1 AND state = 'firing'`, ruleID).Scan(&kpiInc)
	if rstate != "firing" || rlast == nil || kpiInc == 0 {
		t.Errorf("kpi rule state=%s last=%v incident=%d", rstate, rlast, kpiInc)
	}

	// stream rule: three "boom" errors straight from the stream fire within the call (no tick)
	lrs := []*logspb.LogRecord{}
	for range 3 {
		lrs = append(lrs, &logspb.LogRecord{SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "boom in handler"}}})
	}
	b, _ := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "r2-api"}}}}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: lrs}},
	}}})
	sp := stream.NewProcessor(eng, reg, log)
	sp.Refresh()
	sp.Handle(ctx, &kgo.Record{Topic: "tt.otlp.logs", Value: b, Headers: []kgo.RecordHeader{{Key: "project_id", Value: []byte("9201")}}})
	if s, v := state("stream boom"); s != "firing" || v != 3 {
		t.Errorf("stream rule state %s value %v", s, v)
	}

	// acknowledge one incident: it must not remind
	exec(`UPDATE incidents SET state = 'acknowledged', acked_at = now() WHERE id = $1`, incOf("acked").id)

	// ── tick 2 at +8 min: /pending recovered before forMin → quiet; reminders due (5 min) ──
	eng.Now = func() time.Time { return now.Add(8 * time.Minute) }
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if i := incOf("pending"); i.state != "resolved" || !i.quiet || i.notifs != 0 {
		t.Errorf("pending should resolve quietly: %+v", i)
	}
	var rem, remAcked int
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE incident_id = $1 AND kind = 'reminder'`, incOf("remind").id).Scan(&rem)
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE incident_id = $1 AND kind = 'reminder'`, incOf("acked").id).Scan(&remAcked)
	if rem != 2 || remAcked != 0 {
		t.Errorf("reminders: remind=%d acked=%d", rem, remAcked)
	}
	if err := eng.Tick(ctx); err != nil { // same reminder slot: idempotent
		t.Fatal(err)
	}
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE incident_id = $1 AND kind = 'reminder'`, incOf("remind").id).Scan(&rem)
	if rem != 2 {
		t.Errorf("reminders duplicated: %d", rem)
	}

	// ── mute through the api path (muted_until + updated_at bump): picked up on reload, reminder 2 suppressed ──
	exec(`UPDATE monitors SET muted_until = $2, updated_at = now() WHERE id = $1`, ids["remind"], now.Add(time.Hour))
	eng.Now = func() time.Time { return now.Add(11 * time.Minute) }
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var supRem int
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE incident_id = $1 AND kind = 'reminder'`, incOf("remind").id).Scan(&rem)
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM incident_events WHERE incident_id = $1 AND kind = 'suppressed' AND data->>'n' = '2'`, incOf("remind").id).Scan(&supRem)
	if rem != 2 || supRem != 1 {
		t.Errorf("muted reminder: notifications=%d suppressed events=%d", rem, supRem)
	}

	// ── notifier: webhook + Teams adaptive cards (firing and reminder), Teams URL resolved from secrets ──
	w := &notify.Worker{DB: pg, D: notify.NewDeliverer(notify.SMTPConfig{}), Log: log, Reg: reg, Secrets: notify.NewSecretResolver(&store.Store{DB: pg}, key)}
	if _, err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	teams := bodies["/teams"]
	if len(teams) != 2 {
		t.Fatalf("teams deliveries %d", len(teams))
	}
	var sawFiring, sawReminder bool
	for _, body := range teams {
		var m struct {
			Type        string `json:"type"`
			Attachments []struct {
				ContentType string         `json:"contentType"`
				Content     map[string]any `json:"content"`
			} `json:"attachments"`
		}
		if err := json.Unmarshal([]byte(body), &m); err != nil || m.Type != "message" || m.Attachments[0].ContentType != "application/vnd.microsoft.card.adaptive" ||
			m.Attachments[0].Content["type"] != "AdaptiveCard" {
			t.Errorf("teams body %s", body)
		}
		sawFiring = sawFiring || strings.Contains(body, "[CRITICAL] Firing: remind")
		sawReminder = sawReminder || strings.Contains(body, "Still firing (reminder 1): remind")
	}
	if !sawFiring || !sawReminder {
		t.Errorf("teams firing=%v reminder=%v", sawFiring, sawReminder)
	}
	var hookReminder bool
	for _, body := range bodies["/hook"] {
		if strings.Contains(body, `"event":"reminder"`) {
			hookReminder = true
		}
		if strings.Contains(body, `"title":"muted"`) || strings.Contains(body, `"title":"maint"`) || strings.Contains(body, `"title":"pending"`) {
			t.Errorf("suppressed/pending incident notified: %s", body)
		}
	}
	if !hookReminder {
		t.Error("no reminder webhook")
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }
