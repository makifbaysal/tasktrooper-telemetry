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

# browser-style session (TT_AUTH_DEV=true): dev@tasktrooper.local is owner of org 1 (seeded by 007_auth.sql)
curl -c /tmp/tt.jar -XPOST -H 'Content-Type: application/json' -d '{}' localhost:8080/api/v1/auth/dev-login
curl -b /tmp/tt.jar -H 'X-Trooper-Project: 1' localhost:8080/api/v1/me
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
| `TT_AUTH_DEV` | `false` | enables `POST /auth/dev-login` (no IdP; logs a warning). Never in production |
| `TT_SESSION_TTL` | `168h` | sliding session lifetime (`tt_session` cookie, httpOnly, SameSite=Lax) |
| `TT_COOKIE_SECURE` | `false` | force `Secure` cookies (also set automatically on TLS / `X-Forwarded-Proto: https`) |
| `TT_WEB_URL` | `http://localhost:5173` | where login / install callbacks redirect (`redirect` params may only point here or to an allowed origin) |
| `TT_OIDC_ISSUER`, `TT_OIDC_CLIENT_ID`, `TT_OIDC_CLIENT_SECRET` | – | OIDC Authorization Code + PKCE (S256); unset issuer = OIDC disabled |
| `TT_OIDC_REDIRECT_URL` | `http://localhost:8080/api/v1/auth/callback` | must be registered at the IdP |
| `TT_OIDC_SCOPES` | `openid,email,profile` | |
| `TT_PUBLIC_API_URL` | `http://localhost:8080` | public base of this api (Slack OAuth redirect URI = `<it>/api/v1/integrations/slack/callback`) |
| `TT_SLACK_CLIENT_ID`, `TT_SLACK_CLIENT_SECRET`, `TT_SLACK_SIGNING_SECRET` | – | Slack App (ADR-017); unset = Slack endpoints answer 501 |
| `TT_SLACK_API_URL` | `https://slack.com/api` | tests / proxies |
| `TT_GITHUB_APP_ID`, `TT_GITHUB_APP_PRIVATE_KEY` (PEM or `env:NAME` / `file:/path`), `TT_GITHUB_API_URL` | – | GitHub App (`services/pkg/githubapp`); unset = GitHub endpoints answer 501 |
| `TT_GITHUB_APP_SLUG` | – | install page `https://github.com/apps/<slug>/installations/new` |
| `TT_GITHUB_URL` | `https://github.com` | GitHub Enterprise |
| `TT_AI_PRICES` | built-in table | LLM prices per model, `<model>=<in>/<out>,...` (USD per 1M tokens → credits; `services/pkg/aiquota`). Invalid value = startup error |
| `TT_AI_DEFAULT_PRICE` | aiquota default | `<in>/<out>` for models missing from `TT_AI_PRICES` |
| `TT_AI_PLAN_CREDITS` | `free=100,team=5000,enterprise=-1` | monthly AI credits per plan (`-1` = unlimited); org override via `PUT /orgs/{org}/ai-quota` |
| `TT_FAAS_LAMBDA_REQUEST_PRICE` | `0.20` | USD per 1M Lambda requests (serverless cost estimate) |
| `TT_FAAS_LAMBDA_GBS_PRICE`, `TT_FAAS_LAMBDA_GBS_PRICE_ARM64` | `0.0000166667`, `0.0000133334` | USD per GB-second (x86_64 / arm64) |
| `TT_FAAS_AZURE_GBS_PRICE`, `TT_FAAS_AZURE_REQUEST_PRICE` | `0.000016`, `0.20` | Azure Functions consumption plan: USD per GB-s, per 1M executions |
| `TT_AZURE_LOGIN_URL`, `TT_AZURE_ARM_URL` | `https://login.microsoftonline.com`, `https://management.azure.com` | Azure endpoints for `POST /integrations/test` (tests / sovereign clouds) |

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
| `internal/timerange` | `range` → aligned buckets (15m → 15×1m, 30m → 30×1m, 1h → 60×1m, 6h → 72×5m, 24h → 96×15m, 7d → 84×2h), zero-filled series builder |
| `internal/rules` | consumer-lag rule math mirroring `apps/web/src/lib/lagAlerts.ts` (monitor preview) |
| `internal/slo` | SLI ratio, burn rate, budget, KPI status |
| `internal/auth` | `Authorization: Bearer <key>` → sha256 → `api_keys` (kind `api`, not revoked), 60 s cache (dropped on revoke); OIDC client (go-oidc discovery, PKCE, nonce, id-token verification) |
| `internal/slack` | minimal Slack Web API client (oauth.v2.access, conversations.list, users.info, response_url) + request signature verification |
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
* Code insights (ADR-014, written by `services/codeintel`): `POST /repos` seals `token` (`credentials_ref = secret:<id>`)
  or links a GitHub App installation of the project, and stamps `analyze_requested_at`; status is derived (`analyzing`
  while a request is newer than `analyzed_at`). `/inventory` reads `inventory_items.project_id` (shadow items have no
  repo); criticality critical|high|medium|low → critical|important|important|low. Accepting a suggestion creates the
  monitor from `monitor_draft` (`source = suggestion`) in one transaction. Monitors with `source = code` are managed in
  the repo: PUT / PATCH / DELETE → 409 (a PATCH that only sets `mutedUntil` is allowed).
* Load tests (ADR-015): `/loadtests/runs/{id}/series` reads runner metrics (`service = loadtest`, attr
  `trooper.loadtest.run`; delta sums per bucket, p95 from merged histogram buckets, VUs = Σ regions) and falls back to
  `loadtest_runs.live`; `/target` compares the service's `red_1m` with `callers_1m` rows of caller `loadtest:<id>`.
  Draft scenarios cannot run (409). Stop: queued → aborted, running → `abort_requested_at` (loadtest-ctl stops runners).
  Domains: `verify` stamps `verify_requested_at` for loadtest-ctl.
* Logs `?source=` filters `attrs['vercel.source']`.

## Auth, roles, audit (ADR-016)

* `Authorization: Bearer <api key>` → the key's project, acting as **admin** of its org. Otherwise the `tt_session`
  cookie (sessions table stores sha256 of the token; sliding expiry, refreshed at most once a minute). Cookie requests
  choose the project with `X-Trooper-Project` (must be in one of the user's orgs → else 403); state-changing cookie
  requests must come from an allowed `Origin`.
* Roles (`memberships.role`): viewer = GET only; member = normal writes; admin = members, api keys, integrations,
  Slack/GitHub, load test domains, audit log; owner = granting/removing owner (the last owner cannot leave).
* OIDC: `/auth/login?redirect=` → IdP (state + PKCE verifier + nonce in `oidc_login_states`, 10 min) →
  `/auth/callback` → user upserted by `sub` (or linked by verified email) → cookie → redirect to the web app;
  failures redirect to `<web>/login?error=...`.
* Every non-GET request is written to `audit_log` (action = method + route pattern, target = last path id or created
  id); logins and Slack actions too.

## Slack App and GitHub App (ADR-017)

* Install: browser (cookie) → `GET /integrations/{slack|github}/install?redirect=` (302); api-key clients →
  `POST /integrations/{slack|github}/install-url` → `{url}`. Both carry an HMAC-signed state (org, project, user,
  redirect; 15 min). Callbacks redirect back with `slack=connected|error` / `github=<integration id>|pending|error`.
* Slack: one installation per org (`integrations(provider='slack')`, bot token sealed); `GET /integrations/slack/channels?q=`
  (all pages, 5 min cache, `refresh=true`). `POST /slack/interactions` verifies `X-Slack-Signature` (±5 min) and handles
  `tt_ack:<incidentId>` / `tt_mute1h:<monitorId>`; the Slack user is mapped by email to an org member (member+). Acks
  do not enqueue notifications (the alert engine announces `acknowledged`).
* GitHub: `integrations(provider='github', config.installationId)` per project; `GET /integrations/github/{id}/repos?q=`
  lists installation repos (installation token) with `added`; `POST /repos {provider:'github', fullName, installationId}`.

## Known gaps (need contract/schema changes)

* Broker `net_in` is passed through as MB/s (`netInMBs`).
* `rum_resources_1m` has no p75 (API-call p75 reads raw `rum_events`); app-start kind comes from `attrs['app_start.kind']` (set by ingest-writer).
* Journey KPIs have data only once the alert engine writes `sli_1m`.
* The alert engine (not this service) must honor `oldest_age` lag rules, maintenance windows and `muted_until`;
  the notifier must resolve `secret:` refs in channel config.
