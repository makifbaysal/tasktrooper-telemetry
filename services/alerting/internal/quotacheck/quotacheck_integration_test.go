//go:build integration

// Against the docker-compose Postgres (make infra && make migrate):
//
//	GOWORK=off go test -tags integration ./internal/quotacheck/
package quotacheck

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"

	"github.com/tasktrooper/telemetry/services/alerting/internal/notify"
)

func TestCheckerOnce(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TT_POSTGRES_URL")
	if url == "" {
		url = "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"
	}
	pg, err := pgxpool.New(ctx, url)
	if err != nil || pg.Ping(ctx) != nil {
		t.Skip("postgres not reachable")
	}
	t.Cleanup(pg.Close) // registered first: runs after the row cleanup
	var orgID int64
	if err := pg.QueryRow(ctx, `INSERT INTO orgs (name, plan, ai_monthly_credits, ai_notify)
		VALUES ('quotacheck-it', 'free', 100, '{"slack:C0123ABCD#ai-usage","email:ops@example.com"}') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		bg := context.Background()
		_, _ = pg.Exec(bg, `DELETE FROM notifications WHERE kind = 'ai_quota' AND dedupe_key LIKE 'ai_quota:' || $1 || ':%'`, strconv.FormatInt(orgID, 10))
		_, _ = pg.Exec(bg, `DELETE FROM orgs WHERE id = $1`, orgID)
	}
	t.Cleanup(cleanup)
	if _, err := pg.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES (9501, $1, 'quotacheck', 'quotacheck-9501')`, orgID); err != nil {
		t.Fatal(err)
	}
	usage := func(credits float64, at time.Time) {
		t.Helper()
		if _, err := pg.Exec(ctx, `INSERT INTO ai_usage (org_id, project_id, feature, model, input_tokens, output_tokens, credits, created_at)
			VALUES ($1, 9501, 'codeintel.enrich', 'm', 1, 1, $2, $3)`, orgID, credits, at); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	plans, _ := aiquota.ParsePlans("")
	c := &Checker{DB: pg, Plans: plans, Now: func() time.Time { return now }}
	count := func() (n int, kinds []string) {
		rows, _ := pg.Query(ctx, `SELECT payload FROM notifications WHERE kind = 'ai_quota' AND dedupe_key LIKE 'ai_quota:' || $1 || ':%' ORDER BY id`, strconv.FormatInt(orgID, 10))
		defer rows.Close()
		for rows.Next() {
			var b []byte
			_ = rows.Scan(&b)
			var p notify.Payload
			_ = json.Unmarshal(b, &p)
			kinds = append(kinds, notify.AIQuotaText(p))
			n++
		}
		return n, kinds
	}

	usage(500, aiquota.MonthStart(now).Add(-time.Hour)) // last month: ignored
	usage(79, now)
	if _, err := c.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := count(); n != 0 {
		t.Fatalf("79%%: %d notifications", n)
	}
	usage(2, now) // 81 %
	for range 2 { // second run must dedupe
		if _, err := c.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	n, texts := count()
	if n != 2 || texts[0] == "" {
		t.Fatalf("80%%: %d %v", n, texts)
	}
	usage(30, now) // 111 %
	if _, err := c.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Once(ctx); err != nil {
		t.Fatal(err)
	}
	n, texts = count()
	if n != 4 {
		t.Fatalf("100%%: %d %v", n, texts)
	}
	var marks int
	_ = pg.QueryRow(ctx, `SELECT count(*) FROM ai_quota_alerts WHERE org_id = $1 AND period = $2`, orgID, aiquota.MonthStart(now)).Scan(&marks)
	if marks != 2 {
		t.Fatalf("marks %d", marks)
	}
	t.Log(texts)
}
