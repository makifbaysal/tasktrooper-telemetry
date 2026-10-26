# api — dashboard query API

`net/http` + chi over ClickHouse (telemetry) and Postgres (metadata). The HTTP contract is
[`openapi.yaml`](openapi.yaml) (OpenAPI 3.1, single source of truth; `TestOpenAPICoversRoutes` fails when a route
and the spec drift apart). Response shapes follow `apps/web/src/data/types.ts` / `infra.ts`.

## Run

```bash
docker compose -f deploy/docker-compose.yml up -d          # infra (from the repo root)
cd services/api
GOWORK=off go run ./cmd/api                                 # :8080

curl -H 'Authorization: Bearer tt_dev_api_key' 'localhost:8080/api/v1/services?range=1h'
curl -H 'Authorization: Bearer tt_dev_api_key' 'localhost:8080/api/v1/services/checkout/endpoints/post-checkout/series?range=24h&env=production'
curl localhost:8080/healthz
```

Docker (build context `services/`): `docker build -f api/Dockerfile -t tasktrooper/api services/`, or the `api`
service of `deploy/docker-compose.yml` (profile `app`).

| Env | Default | |
|---|---|---|
| `TT_API_ADDR` | `:8080` | listen address |
| `TT_API_CORS_ORIGINS` | `http://localhost:*,http://127.0.0.1:*` | allowed browser origins: exact origins, `*` = any, `scheme://host:*` = any port on that host (dev default: every localhost port). Set exact origins in production |
| `TT_SECRETS_KEY` | dev key (warns) | AES-256-GCM key for the `secrets` table (`services/pkg/secrets`; AAD = project id). Integration and notification-channel secrets are sealed with it and never returned |
| `TT_SYMBOLICATOR_URL` | `http://localhost:8095` | `POST /v1/symbolicate` for error / RUM error detail (2 s timeout, raw stack on any failure; empty = disabled) |
| `TT_POSTGRES_URL` | `postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable` | |
| `TT_CLICKHOUSE_URL` | `clickhouse://trooper:trooper@localhost:9000/telemetry` | clickhouse-go DSN |
| `TT_API_CH_MAX_CONNS` | `20` | |
| `TT_LOG_LEVEL` | `info` | JSON slog |

## Test

```bash
GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...
# live infra: seeds org/project 9101 in Postgres + ClickHouse, calls every endpoint group, deletes what it wrote
GOWORK=off go test -tags integration -count=1 ./internal/server/ -run Integration -v
npx @redocly/cli lint openapi.yaml
```

## Layout

| Package | |
|---|---|
| `cmd/api` | wiring, graceful shutdown |
| `internal/server` | router, middleware (request id, panic recovery, JSON request log, CORS, gzip), auth, handlers. Handlers only parse parameters and call the `Store` interface (unit tests use a fake store) |
| `internal/store` | all SQL. `apm.go`, `traces.go`, `logs.go`, `rum.go`, `alerting.go`, `kpi.go`, `infra.go`, `loadtest.go` |
| `internal/tq` | tenant guard: every query must contain `{{tenant}}` / `{{tenant:alias}}` (→ `org_id = O AND project_id = P` in ClickHouse, `project_id = P` in Postgres) or `{{project}}` / `{{org}}` for inserts; queries without one are rejected |
| `internal/timerange` | `range` → aligned buckets (1h → 60×1m, 6h → 72×5m, 24h → 96×15m, 7d → 84×2h), zero-filled series builder |
| `internal/rules` | consumer-lag rule math mirroring `apps/web/src/lib/lagAlerts.ts` (monitor preview) |
| `internal/slo` | SLI ratio, burn rate, budget, KPI status |
| `internal/auth` | `Authorization: Bearer <key>` → sha256 → `api_keys` (kind `api`, not revoked), 60 s cache |
| `internal/cloudcheck` | `POST /integrations/test`: minimal live provider checks (AWS STS GetCallerIdentity / AssumeRole, GCP token + Resource Manager, Vercel `/v2/user`, Neon `/projects`), 10 s timeout, secrets scrubbed from messages |
| `internal/symbolicate` | symbolicator client (forwards the caller's `Authorization`) |
| `internal/conntest` | real connection tests: Postgres login + `pg_stat_statements` check, Redis PING, Kafka metadata + groups (franz-go), RabbitMQ management API; MySQL/MongoDB TCP only |

## Data sources

* RED: `red_1m` for ranges ≤ 24h, `red_1h` for 7d; consumers `consumer_1m`; callers `callers_1m`; dependencies and
  time breakdown `edges_1m`; traces `trace_index` + `spans`; errors `errors_1h` + Postgres `error_issues` (status).
* Quantiles: `quantilesTDigestWeightedMerge` (span rollups), `quantilesTDigestMerge` (RUM rollups).
* Collector tables (`db_1m`, `broker_*`): non-finite values (unreported) are ignored; missing values are `0` in JSON.
* KPI SLI source: `sli_1m` when it has rows for the KPI, otherwise endpoint → `red_1m` (success) / `spans` (latency),
  page → `rum_events` vitals / `rum_views_1m`, app start → `rum_events`. Rule state comes from the alert engine
  (`kpi_alert_rules.state`) once it has evaluated the rule, otherwise it is computed here.
* DB top queries → calling endpoint: matched by normalized text (`$n`/literals → `?`, lowercase,
  `normalizeQuery`) between `db_queries_1m.query` and span `db.query.text`, because `query_hash` is the engine's query id.
* Monitor `query` JSON is stored as given (shapes: `services/alerting/README.md`); `consumer_lag` monitors take
  `queue` (QueueRule) and store it as `query`. Preview supports `consumer_lag`, `metric_threshold`, `latency`,
  `error_rate`, `throughput`, `log_count` / `log_match` (`log_patterns_1m`, raw `logs` when `contains` is set),
  `log_pattern` (new patterns), `dlq` / `consumer_failures`, `rum_vital`, `crash_rate` — same window / `minRequests` /
  severity math as `services/alerting`.
* Deploys: versions first seen in `spans` merged with Postgres `deploy_markers` (Vercel, CI, …); `?source=&service=`.
* Integrations (`/integrations`, ADR-012): secrets sealed into `secrets` (`credentials_ref = secret:<id>`), PATCH
  rotates in place, DELETE removes secrets; Vercel accepts `secret = {"token","drainSecret"}` (drain secret sealed
  separately → `config.drainSecretRef`). `POST /integrations/{id}/sync` stamps `config.syncRequestedAt` for
  trooper-cloud. `resources` = what discovery wrote (`databases`, `broker_clusters`, `deploy_markers`).
* Notification channels: secret config keys (slack/teams `webhookUrl`, pagerduty `routingKey`, opsgenie `apiKey`,
  webhook `secret`) are stored as `secret:<id>` refs and shown masked.
* Endpoint RCA (`/services/{s}/endpoints/{e}/rca?baseline=prev|1d|7d`): endpoint + each `edges_1m` child, range vs
  baseline, ms per request deltas. Client apps (web/mobile) get RUM-based `/services/{app}/series`; consumers list
  their topic sources as callers.
* Code insights (`/suggestions`, `/repos`, `/inventory`): no backend yet, empty lists.

## Known gaps (need contract/schema changes)

* Broker `net_in` is passed through as MB/s (`netInMBs`).
* `rum_resources_1m` has no p75 (API-call p75 reads raw `rum_events`); app-start kind comes from `attrs['app_start.kind']` (set by ingest-writer).
* Journey KPIs have data only once the alert engine writes `sli_1m`.
* The alert engine (not this service) must honor `oldest_age` lag rules, maintenance windows and `muted_until`;
  the notifier must resolve `secret:` refs in channel config.
