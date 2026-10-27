//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/pkg/config"

	"github.com/tasktrooper/telemetry/services/alerting/internal/conn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/engine"
	"github.com/tasktrooper/telemetry/services/alerting/internal/leader"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

func cleanupLLM(t *testing.T, ctx context.Context, pg *pgxpool.Pool, ch driver.Conn) {
	t.Helper()
	for _, q := range []string{`DELETE FROM projects WHERE id = 9602`, `DELETE FROM orgs WHERE id = 9602`} {
		if _, err := pg.Exec(ctx, q); err != nil {
			t.Fatalf("pg cleanup: %v", err)
		}
	}
	mctx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 2}))
	for _, tbl := range []string{"llm_1m", "llm_1h"} {
		if err := ch.Exec(mctx, "ALTER TABLE telemetry."+tbl+" DELETE WHERE project_id = 9602"); err != nil {
			t.Fatalf("ch cleanup %s: %v", tbl, err)
		}
	}
}

// TestLLM (org/project 9602): llm_1m rows (llm_1h fills through its MV) for three priced / unpriced models; llm
// monitors fire on cost per hour (with the top contributor in the root cause and payload), error rate, p95 and
// tokens over 24 h (llm_1h + llm_1m edges), an unpriced model is no_data, and an org llm_prices row prices it.
func TestLLM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	var hasLLM uint8
	if err := ch.QueryRow(ctx, "EXISTS TABLE telemetry.llm_1m").Scan(&hasLLM); err != nil || hasLLM == 0 {
		t.Skip("llm_1m missing (005_replay_profiles_llm.sql not applied)")
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pg.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	cleanupLLM(t, ctx, pg, ch)
	if os.Getenv("TT_ITEST_KEEP") == "" {
		t.Cleanup(func() { cleanupLLM(t, context.Background(), pg, ch) })
	}
	exec(`INSERT INTO orgs (id, name) VALUES (9602, 'itest llm')`)
	exec(`INSERT INTO projects (id, org_id, name, slug) VALUES (9602, 9602, 'itest llm', 'itest-llm')`)
	var hasPrices bool
	_ = pg.QueryRow(ctx, `SELECT to_regclass('llm_prices') IS NOT NULL`).Scan(&hasPrices)
	if hasPrices {
		exec(`INSERT INTO llm_prices (org_id, provider, model, input_per_mtok, output_per_mtok) VALUES (9602, 'ollama', 'llama3', 1, 0)`)
	}

	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	to := now.Truncate(time.Minute)
	// one llm_1m row per minute: calls, errors, input / output tokens per minute, constant duration (ms)
	ins := func(service, provider, model string, from time.Time, minutes int, calls, errs, in, out int, durMs float64) {
		mustExec(t, ctx, ch, `INSERT INTO telemetry.llm_1m (org_id, project_id, service, env, provider, model, operation, route, method, minute,
				calls, errors, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, duration_sum, duration_hist, ttft_hist)
			SELECT 9602, 9602, ?, 'production', ?, ?, 'chat', '/chat', 'POST', toDateTime(?) + number * 60,
				toUInt64(?), toUInt64(?), toUInt64(?), toUInt64(?), 0, 0, toFloat64(?) * ?,
				quantilesTDigestWeightedState(0.5, 0.95, 0.99)(toFloat64(?), toUInt32(?)),
				quantilesTDigestWeightedStateIf(0.5, 0.95, 0.99)(toFloat64(0), toUInt32(1), number < 0)
			FROM numbers(?) GROUP BY number`,
			service, provider, model, from.Unix(), calls, errs, in, out, durMs, calls, durMs, calls, minutes)
	}
	last10 := to.Add(-10 * time.Minute)
	// claude: 0.4M in × $3 + 0.1M out × $15 = $2.70/min; gpt-5: 0.2M × $1.25 + 0.05M × $10 = $0.75/min
	ins("storefront-bff", "anthropic", "claude-sonnet-4-5-20250929", last10, 10, 20, 0, 400_000, 100_000, 25_000)
	ins("search", "openai", "gpt-5", last10, 10, 10, 1, 200_000, 50_000, 2_000)
	ins("local-svc", "ollama", "llama3", last10, 10, 5, 0, 1_000, 1_000, 500)
	ins("lab", "acme", "mystery-model", last10, 10, 5, 0, 1_000, 1_000, 500)
	// older traffic of search (3 h ago): only a 24 h window sees it, through llm_1h
	ins("search", "openai", "gpt-5", to.Add(-3*time.Hour).Truncate(time.Hour), 4, 1, 0, 250_000, 0, 2_000)

	type mon struct {
		id          int64
		target, q   string
		cond, want  string
		value, tol  float64
		summaryPart string
	}
	mons := []mon{
		{9602101, `{}`, `{"metric":"cost_usd_per_hour","filter":{}}`, `{"op":">","threshold":20,"window":"1h"}`, "firing", 34.51, 1e-6,
			"LLM cost $34.51/h above $20/h — claude-sonnet-4-5-20250929 in storefront-bff 78%"},
		{9602102, `{}`, `{"metric":"cost_usd","filter":{"model":"mystery-model"}}`, `{"op":">","threshold":0}`, "no_data", 0, 0, ""},
		{9602103, `{}`, `{"metric":"error_rate","filter":{"provider":"openai"}}`, `{"op":">","threshold":0.05,"window":"15m","minRequests":20}`, "firing", 0.1, 1e-9,
			"LLM error rate 10% above 5% — gpt-5 in search 100% of errors"},
		{9602104, `{}`, `{"metric":"error_rate","filter":{"provider":"openai"}}`, `{"op":">","threshold":0.05,"window":"15m","minRequests":500}`, "ok", 0.1, 1e-9, ""},
		{9602105, `{"service":"storefront-bff"}`, `{"metric":"p95_ms","filter":{}}`, `{"op":">","threshold":20000,"window":"15m"}`, "firing", 25_000, 1, ""},
		{9602106, `{}`, `{"metric":"tokens","filter":{"service":"search"}}`, `{"op":">","threshold":3000000,"window":"24h"}`, "firing", 3_500_000, 0, ""},
		{9602107, `{}`, `{"metric":"tokens","filter":{"service":"search"}}`, `{"op":">","threshold":3000000,"window":"1h"}`, "ok", 2_500_000, 0, ""},
		{9602108, `{}`, `{"metric":"ttft_p95_ms","filter":{}}`, `{"op":">","threshold":1}`, "no_data", 0, 0, ""},
	}
	if hasPrices {
		// 10 min × 1000 input tokens × $1 per 1M = $0.01 (org override; no default price)
		mons = append(mons, mon{9602109, `{}`, `{"metric":"cost_usd","filter":{"model":"llama3"}}`, `{"op":">","threshold":0.005,"window":"15m"}`, "firing", 0.01, 1e-9, ""})
	}
	for _, m := range mons {
		exec(`INSERT INTO monitors (id, project_id, name, kind, target, query, condition, severity, channels, source)
			VALUES ($1, 9602, $2, 'llm', $3, $4, $5, 'warning', '{webhook:itest}', 'template')`, m.id, "llm "+itoa(m.id), m.target, m.q, m.cond)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	el := &leader.Elector{Pool: pg, Base: leader.DefaultBase + 9602_101, Shards: 1}
	defer el.Release()
	eng := engine.New(engine.Config{ProjectIDs: []int64{9602}}, log, &store.Store{DB: pg}, ch, el, obs.NewRegistry())
	eng.Now = func() time.Time { return now }
	if err := eng.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	for _, m := range mons {
		var state string
		var last *float64
		if err := pg.QueryRow(ctx, `SELECT state, last_value FROM monitors WHERE id = $1`, m.id).Scan(&state, &last); err != nil {
			t.Fatal(err)
		}
		if state != m.want {
			t.Errorf("monitor %d (%s %s): state %s, want %s", m.id, m.q, m.cond, state, m.want)
			continue
		}
		if m.want != "no_data" && (last == nil || abs(*last-m.value) > m.tol) {
			t.Errorf("monitor %d (%s): value %v, want %v", m.id, m.q, deref(last), m.value)
		}
		if m.summaryPart == "" {
			continue
		}
		var summary, payload string
		var detail []byte
		if err := pg.QueryRow(ctx, `SELECT coalesce(i.root_cause->>'summary', ''), coalesce(n.payload->>'summary', ''),
				(SELECT e.data FROM incident_events e WHERE e.incident_id = i.id AND e.kind = 'fired' ORDER BY e.id LIMIT 1)
			FROM incidents i JOIN notifications n ON n.incident_id = i.id AND n.kind = 'firing' WHERE i.monitor_id = $1 LIMIT 1`, m.id).
			Scan(&summary, &payload, &detail); err != nil {
			t.Errorf("monitor %d: incident: %v", m.id, err)
			continue
		}
		if summary != m.summaryPart || payload != summary {
			t.Errorf("monitor %d: root cause %q / payload %q, want %q", m.id, summary, payload, m.summaryPart)
		}
		var ev struct {
			Detail struct {
				TopModels   []map[string]any `json:"topModels"`
				TopServices []map[string]any `json:"topServices"`
				Unpriced    []string         `json:"unpricedModels"`
			} `json:"detail"`
		}
		_ = json.Unmarshal(detail, &ev)
		if len(ev.Detail.TopModels) == 0 || len(ev.Detail.TopServices) == 0 {
			t.Errorf("monitor %d: detail %s", m.id, detail)
		}
		if m.id == 9602101 && !strings.Contains(strings.Join(ev.Detail.Unpriced, ","), "mystery-model") {
			t.Errorf("unpriced models not reported: %s", detail)
		}
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
