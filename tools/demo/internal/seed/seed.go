// Package seed registers the mock world's metadata in project 1 through the api (databases, broker
// clusters, monitors, KPIs, load-test scenarios). Notification channels and service metadata
// (language, type) have no api endpoint, so they are upserted in Postgres directly. Idempotent:
// existing rows (by id / name) are left alone.
package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

type Seeder struct {
	API, Key, PostgresURL string
	Log                   *slog.Logger
	hc                    *http.Client
}

func (s *Seeder) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	if s.hc == nil {
		s.hc = &http.Client{Timeout: 30 * time.Second}
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.API+"/api/v1"+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 && resp.StatusCode != 409 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(b))
	}
	if out != nil && resp.StatusCode < 300 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

// names returns the "name" fields of a list endpoint.
func (s *Seeder) names(ctx context.Context, path string) (map[string]bool, error) {
	var rows []struct {
		Name string `json:"name"`
	}
	if _, err := s.do(ctx, "GET", path, nil, &rows); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Name] = true
	}
	return out, nil
}

// Run seeds everything.
func (s *Seeder) Run(ctx context.Context) error {
	if err := s.postgres(ctx); err != nil {
		return fmt.Errorf("postgres metadata: %w", err)
	}
	steps := []struct {
		name string
		f    func(context.Context) (int, error)
	}{{"databases", s.databases}, {"clusters", s.clusters}, {"monitors", s.monitors}, {"kpis", s.kpis}, {"load-test scenarios", s.scenarios}}
	for _, st := range steps {
		n, err := st.f(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
		s.Log.Info("seeded", "what", st.name, "created", n)
	}
	return nil
}

// ---------------------------------------------------------------- postgres (no api endpoint)

var channels = []string{"#payments-alerts", "PagerDuty", "#orders-oncall", "#data-platform", "#web-team", "#platform", "#billing", "#catalog", "#mobile", "#seller-team"}

func (s *Seeder) postgres(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, s.PostgresURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, c := range channels {
		cfg, _ := json.Marshal(map[string]any{"url": fmt.Sprintf("http://127.0.0.1:%d/hook/%s", world.WebhookSinkPort, url.PathEscape(c))})
		if _, err := conn.Exec(ctx, `INSERT INTO notification_channels (project_id, name, type, config) VALUES (1, $1, 'webhook', $2)
		  ON CONFLICT (project_id, name) DO UPDATE SET type = 'webhook', config = EXCLUDED.config`, c, cfg); err != nil {
			return err
		}
	}
	for _, sv := range world.Services {
		if sv.PGType == "" {
			continue // order-events-consumer: type comes from telemetry (consumer)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO services (project_id, name, type, language) VALUES (1, $1, $2, $3)
		  ON CONFLICT (project_id, name) DO UPDATE SET language = EXCLUDED.language`, sv.Name, sv.PGType, sv.Language); err != nil {
			return err
		}
	}
	for _, a := range [][3]string{{"shop-web", "web", "React"}, {"shop-mobile", "mobile", "React Native"}, {"backoffice-web", "web", "React"},
		{"seller-portal", "web", "Vue"}, {"landing-web", "web", "Next.js"}, {"courier-app", "mobile", "Flutter"}} {
		if _, err := conn.Exec(ctx, `INSERT INTO services (project_id, name, type, language) VALUES (1, $1, $2, $3)
		  ON CONFLICT (project_id, name) DO UPDATE SET language = EXCLUDED.language`, a[0], a[1], a[2]); err != nil {
			return err
		}
	}
	s.Log.Info("seeded", "what", "notification channels + service metadata", "channels", len(channels))
	return nil
}

// ---------------------------------------------------------------- infra

func (s *Seeder) databases(ctx context.Context) (int, error) {
	type db struct {
		ID, Engine, Collector string
		Port                  int
	}
	var dbs []db
	for _, p := range world.PGs {
		dbs = append(dbs, db{p.ID, "postgres", "agent", p.Port})
	}
	for _, r := range world.Redises {
		dbs = append(dbs, db{r.ID, "redis", "agent", r.Port})
	}
	dbs = append(dbs, db{"notifications-db", "mysql", "direct", world.MySQLPort})
	n := 0
	for _, d := range dbs {
		st, err := s.do(ctx, "POST", "/infra/databases", map[string]any{
			"id": d.ID, "name": d.ID, "engine": d.Engine, "host": "localhost", "port": d.Port, "collector": d.Collector,
			"env": world.Environment, "user": "trooper_monitor", "credentialsRef": "env:TTDEMO_" + envName(d.ID) + "_DSN",
		}, nil)
		if err != nil {
			return n, err
		}
		if st == 201 {
			n++
		}
	}
	return n, nil
}

func envName(id string) string {
	b := []byte(id)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 32
		case c == '-':
			b[i] = '_'
		}
	}
	return string(b)
}

func (s *Seeder) clusters(ctx context.Context) (int, error) {
	n := 0
	for _, c := range []map[string]any{
		{"id": "kafka-main", "name": "kafka-main", "kind": "kafka", "endpoint": world.DefaultKafka, "auth": map[string]any{"mechanism": "none", "tls": false}, "collector": "agent"},
		{"id": "rabbit-main", "name": "rabbit-main", "kind": "rabbitmq", "endpoint": fmt.Sprintf("http://localhost:%d", world.RabbitMgmt), "vhost": world.RabbitVhost,
			"auth": map[string]any{"mechanism": "plain", "tls": false}, "credentialsRef": "env:TTDEMO_RABBIT_CREDS", "collector": "agent"},
	} {
		st, err := s.do(ctx, "POST", "/infra/clusters", c, nil)
		if err != nil {
			return n, err
		}
		if st == 201 {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- monitors (mock.ts monitors; m9–m11 are KPI rules)

func (s *Seeder) monitors(ctx context.Context) (int, error) {
	have, err := s.names(ctx, "/monitors")
	if err != nil {
		return 0, err
	}
	type m = map[string]any
	cond := func(op string, th, forMin float64) m { return m{"op": op, "threshold": th, "forMin": forMin} }
	defs := []m{
		{"name": "Charges error rate", "kind": "metric_threshold", "target": m{"service": "payments-api", "endpoint": "POST /v1/charges"},
			"query": m{"metric": "error_rate"}, "condition": m{"op": ">", "threshold": 0.02, "forMin": 5, "minRequests": 10}, "severity": "critical", "source": "code",
			"channels": []string{"#payments-alerts", "PagerDuty"}},
		{"name": "Charges p95 latency", "kind": "latency", "target": m{"service": "payments-api", "endpoint": "POST /v1/charges"},
			"query": m{"percentile": "p95"}, "condition": cond(">", 600, 10), "severity": "warning", "source": "code", "channels": []string{"#payments-alerts"}},
		{"name": "order-created-handler lag", "kind": "consumer_lag", "target": m{"service": "order-events-consumer", "process": "order-created-handler"},
			"queue":     m{"clusterId": "kafka-main", "group": world.GroupFulfilment, "topic": "orders.created", "metric": "lag", "threshold": 10000, "forMin": 5},
			"condition": cond(">", 10000, 5), "severity": "warning", "source": "code", "channels": []string{"#orders-oncall"}},
		{"name": "reco-trainer drains every run", "kind": "consumer_lag", "target": m{},
			"queue":     m{"clusterId": "kafka-main", "group": world.GroupRecoTrainer, "topic": "reco.events", "metric": "not_drained", "threshold": 2, "forMin": 0},
			"condition": cond(">", 2, 0), "severity": "warning", "source": "manual", "channels": []string{"#data-platform"}},
		{"name": "ledger-sync rebalancing", "kind": "consumer_lag", "target": m{},
			"queue":     m{"clusterId": "kafka-main", "group": world.GroupLedger, "topic": "payments.events", "metric": "rebalances", "threshold": 6, "forMin": 10},
			"condition": cond(">", 6, 10), "severity": "warning", "source": "manual", "channels": []string{"#payments-alerts"}},
		{"name": "Create order errors", "kind": "metric_threshold", "target": m{"service": "orders-api", "endpoint": "POST /v1/orders"},
			"query": m{"metric": "error_rate"}, "condition": m{"op": ">", "threshold": 0.01, "forMin": 5, "minRequests": 10}, "severity": "critical", "source": "manual",
			"channels": []string{"#orders-oncall", "PagerDuty"}},
		{"name": "Payment provider timeouts", "kind": "log_match", "target": m{"service": "payments-api"},
			"query": m{"contains": "provider timeout", "window": "5m"}, "condition": cond(">", 20, 0), "severity": "critical", "source": "code", "channels": []string{"#payments-alerts"}},
		{"name": "Checkout LCP", "kind": "rum_vital", "target": m{"service": "shop-web", "view": "/checkout"},
			"query": m{"app": "shop-web", "view": "/checkout", "vital": "LCP", "percentile": "p75"}, "condition": cond(">", 2500, 15), "severity": "warning", "source": "template",
			"channels": []string{"#web-team"}},
		{"name": "Catalog throughput drop", "kind": "throughput_change", "target": m{"service": "catalog-api"},
			"query": m{"direction": "drop", "baselineOffset": "168h"}, "condition": cond(">", 0.5, 15), "severity": "warning", "source": "manual", "channels": []string{"#platform"}},
		{"name": "Nightly invoice job", "kind": "job_missed", "target": m{"service": "email-worker", "process": "invoice-batch"},
			"query": m{"schedule": "0 2 * * *", "graceHours": 26}, "condition": cond(">", 26, 0), "severity": "warning", "source": "code", "status": "muted", "channels": []string{"#billing"}},
		{"name": "Reco DB slow queries", "kind": "latency", "target": m{"service": "recommendation-api", "endpoint": "GET /v1/recommendations"},
			"query": m{"percentile": "p95"}, "condition": cond(">", 800, 5), "severity": "critical", "source": "manual", "channels": []string{"#platform"}},
	}
	n := 0
	for _, d := range defs {
		if have[d["name"].(string)] {
			continue
		}
		if _, err := s.do(ctx, "POST", "/monitors", d, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------- KPIs (kpi.ts)

func (s *Seeder) kpis(ctx context.Context) (int, error) {
	have, err := s.names(ctx, "/kpis")
	if err != nil {
		return 0, err
	}
	type m = map[string]any
	burn := func(th float64, long, short, sev string, ch ...string) m {
		return m{"kind": "burn_rate", "threshold": th, "longWindow": long, "shortWindow": short, "severity": sev, "channels": ch}
	}
	defs := []m{
		{"name": "Homepage loads fast", "owner": "web-team", "description": "Visitors see the homepage quickly. The first thing every shopper sees.",
			"target": m{"kind": "page", "serviceId": "shop-web", "ref": "/"}, "sli": "latency",
			"good":      m{"metric": "LCP", "thresholdMs": 2500, "noError": true, "description": "Page view with LCP ≤ 2.5 s and no JS error"},
			"objective": 0.95, "window": "28d", "source": "code",
			"alerts": []m{burn(6, "1h", "5m", "critical", "#web-team", "PagerDuty"), burn(3, "6h", "30m", "warning", "#web-team")}},
		{"name": "Product page loads fast", "owner": "web-team", "description": "Product detail pages render quickly; most purchases start here.",
			"target": m{"kind": "page", "serviceId": "shop-web", "ref": "/p/:slug"}, "sli": "latency",
			"good":      m{"metric": "LCP", "thresholdMs": 2500, "noError": true, "description": "Page view with LCP ≤ 2.5 s and no JS error"},
			"objective": 0.9, "window": "28d", "source": "template",
			"alerts": []m{burn(6, "1h", "5m", "critical", "#web-team"), burn(2, "6h", "30m", "warning", "#web-team")}},
		{"name": "Checkout succeeds", "owner": "payments-team", "description": "A shopper who submits checkout ends up with a paid order.",
			"target": m{"kind": "journey", "serviceId": "shop-web", "ref": "Checkout: /checkout → POST /v1/orders → POST /v1/charges"}, "sli": "success_rate",
			"good":      m{"description": "Checkout submit ends with order created and payment captured"},
			"objective": 0.99, "window": "28d", "source": "code",
			"alerts": []m{burn(3, "1h", "5m", "critical", "#payments-alerts", "PagerDuty"),
				{"kind": "sli_below", "threshold": 0.97, "longWindow": "15m", "severity": "warning", "channels": []string{"#payments-alerts"}}}},
		{"name": "Order placement API succeeds", "owner": "orders-team", "description": "POST /v1/orders answers successfully; partners and apps rely on it.",
			"target": m{"kind": "endpoint", "serviceId": "orders-api", "ref": "POST /v1/orders", "endpointId": "post-v1-orders"}, "sli": "success_rate",
			"good": m{"statusBelow": 500, "description": "Response is not 5xx"}, "objective": 0.995, "window": "28d", "source": "code",
			"alerts": []m{burn(1.5, "6h", "30m", "warning", "#orders-oncall")}},
		{"name": "Homepage is up", "owner": "web-team", "description": "The homepage renders at all — document served and no fatal script error.",
			"target": m{"kind": "page", "serviceId": "shop-web", "ref": "/"}, "sli": "availability",
			"good": m{"noError": true, "description": "Document 2xx and app mounted (no fatal JS error)"}, "objective": 0.999, "window": "28d", "source": "code",
			"alerts": []m{burn(14, "1h", "5m", "critical", "#web-team", "PagerDuty")}},
		{"name": "Search answers fast", "owner": "catalog-team", "description": "Search results come back quickly for apps and the storefront.",
			"target": m{"kind": "endpoint", "serviceId": "catalog-api", "ref": "GET /v1/search", "endpointId": "get-v1-search"}, "sli": "latency",
			"good": m{"metric": "Duration", "thresholdMs": 300, "statusBelow": 500, "description": "Response ≤ 300 ms and not 5xx"}, "objective": 0.99, "window": "28d", "source": "template",
			"alerts": []m{burn(6, "1h", "5m", "warning", "#catalog")}},
		{"name": "Courier app opens fast", "owner": "mobile-team", "description": "Couriers can start working right away after opening the app.",
			"target": m{"kind": "app_start", "serviceId": "courier-app", "ref": "Cold start"}, "sli": "latency",
			"good": m{"metric": "Cold start", "thresholdMs": 2000, "noError": true, "description": "Cold start ≤ 2 s and no crash"}, "objective": 0.95, "window": "7d", "source": "manual",
			"alerts": []m{burn(2, "6h", "30m", "warning", "#mobile")}},
		{"name": "Seller order list loads", "owner": "seller-team", "description": "Sellers see their orders quickly in the seller portal.",
			"target": m{"kind": "page", "serviceId": "seller-portal", "ref": "/orders"}, "sli": "latency",
			"good": m{"metric": "LCP", "thresholdMs": 3000, "noError": true, "description": "Page view with LCP ≤ 3 s and no JS error"}, "objective": 0.9, "window": "28d", "source": "manual",
			"alerts": []m{burn(2, "6h", "30m", "warning", "#seller-team")}},
	}
	n := 0
	for _, d := range defs {
		if have[d["name"].(string)] {
			continue
		}
		if _, err := s.do(ctx, "POST", "/kpis", d, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------- load tests (mock.ts scenarios)

func (s *Seeder) scenarios(ctx context.Context) (int, error) {
	have, err := s.names(ctx, "/loadtests/scenarios")
	if err != nil {
		return 0, err
	}
	type m = map[string]any
	defs := []m{
		{"name": "Checkout flow", "source": "generated", "spec": m{"target": "https://staging.shop.com", "regions": []string{"eu-central", "us-east"},
			"steps": []string{"GET /v1/home", "GET /v1/cart", "POST /v1/orders", "POST /v1/charges"}, "mode": "vus", "thinkTime": 1,
			"stages": []m{{"duration": 120, "target": 100}, {"duration": 240, "target": 200}, {"duration": 120, "target": 0}}}},
		{"name": "Catalog browse 3× prod", "source": "generated", "spec": m{"target": "https://staging.shop.com", "regions": []string{"eu-central"},
			"steps": []string{"GET /v1/items", "GET /v1/items/:id", "GET /v1/search"}, "mode": "vus", "thinkTime": 0.5,
			"stages": []m{{"duration": 180, "target": 300}, {"duration": 540, "target": 600}, {"duration": 180, "target": 0}}}},
		{"name": "Auth token refresh soak", "source": "manual", "spec": m{"target": "https://auth.staging.shop.com", "regions": []string{"eu-central"},
			"steps": []string{"POST /v1/auth/login", "POST /v1/auth/refresh"}, "mode": "vus", "thinkTime": 2,
			"stages": []m{{"duration": 300, "target": 80}, {"duration": 3000, "target": 80}, {"duration": 300, "target": 0}}}},
	}
	n := 0
	for _, d := range defs {
		if have[d["name"].(string)] {
			continue
		}
		if _, err := s.do(ctx, "POST", "/loadtests/scenarios", d, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
