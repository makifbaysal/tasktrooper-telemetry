# demo — live demo environment

`tools/demo` (`github.com/tasktrooper/telemetry/tools/demo`) rebuilds the dashboard's **mock world**
(`apps/web/src/data/mock.ts`, `infra.ts`, `kpi.ts`, `frontend.ts`) as real, running software, so the dashboard in
real mode (`VITE_DATA=api`, ADR-013) shows the same services, dependencies, consumers, databases, queues, apps and
incidents as mock mode, all produced by genuine instrumentation and the real pipeline:

```
 RUM sessions (6 apps) ──traceparent──▶ 8 services (Go SDK, 1 OS process per instance) ──▶ ingest-gw ──▶ Redpanda ──▶ writer ──▶ ClickHouse ──▶ api / alerting
   └─ RUM JSON batches ─────────────────────────────────────────────────────────────────────▶ ingest-gw /v1/rum
 ttdemo-* containers (5 × Postgres, 4 × Redis, MySQL, RabbitMQ) + shared Redpanda ◀── trooper-agent (collector, generated config)
```

## One command

```bash
make infra migrate && make demo          # builds tools/demo/.run/demo, runs `demo up` from the repo root
make demo DEMO_ARGS="--rps 40 --incident-after 15m --keep"
```

`demo up` starts every platform service that is not already healthy (ingest-writer `:4320`, ingest-gw `:4318`, api `:8080`,
alert-engine `:8090`, notifier `:8091`; built with `GOWORK=off go build` into `tools/demo/.run/bin`; started with
`TT_S3_ENDPOINT=http://localhost:9002` + the compose S3 credentials unless set, so the writer stores replay blobs and the api
serves them), then `seed`, then `run`. A writer started elsewhere without `TT_S3_ENDPOINT` drops replay segments — `demo run`
warns when it sees `tt_writer_replay_disabled 1`.
Ctrl-C stops everything in order (traffic → services → collector → platform) and removes the `ttdemo-*` containers unless
`--keep`. Logs of every process: `tools/demo/.run/logs/` (alert notifications: `.run/logs/notifications.log`).
Then `cd apps/web && VITE_DATA=api pnpm dev`.

| Command | |
|---|---|
| `demo up` | platform (if not running) + `seed` + `run` |
| `demo seed` (`make demo-seed`) | containers, schemas + data, Kafka topics, RabbitMQ topology, api metadata. Idempotent |
| `demo run` | services, client traffic, external consumers, collector, webhook sink until Ctrl-C / `--duration` |
| `demo down` | remove the `ttdemo-*` containers, the demo topics and consumer groups |
| `demo scenarios` | list incident scenarios |

Flags (`up`/`run`/`seed`): `--rps 20` (≈ client requests/s into the backend; internal fan-out, consumers and the
collector add more), `--scenario all|none|a,b`, `--incident-after 15m` (start healthy, then switch the scenarios on —
charts show the step like the mock's `DEGRADED_SINCE`), `--duration`, `--keep`, `--reco-trainer-every 10m`, `--api`,
`--api-key`, `--dsn` (default `http://tt_dev_ingest_key@localhost:4318/1`), `--kafka`, `--postgres`, `--debug`,
`--infra-sim` (default `true`, see below), and (all default on, see *Replay, profiles, AI* below) `--replay`,
`--replay-sample 0.3`, `--profiling`, `--profile-sim`, `--profile-backfill 30m`, `--llm`.

## What runs

**Services** (`internal/svc`, real HTTP between processes on random ports, `trooperhttp` server + client):
orders-api, payments-api, catalog-api, auth-api, storefront-bff, recommendation-api (HTTP), order-events-consumer
(3 instances; franz-go groups `order-events.fulfilment|search-index|projections|dlq-replay` → processes
`order-created-handler`, `order-search-indexer`, `order-status-projector`, `dlq-replayer`; retries with `tt-attempt`,
DLQ to `orders.created.dlq`), email-worker (2 instances; RabbitMQ `emails.transactional` → `email-sender`,
`emails.marketing` → `newsletter-sender`, cron `invoice-batch` every 5 min, MySQL `notifications-db`, publishes `email.events`).
Data access: pgx (`trooperpgx`, db names = `orders-db` …), go-redis (`trooperredis`, address host = cache name so the
dependency is `redis · cache-main`), database/sql MySQL (`troopersql`), franz-go + amqp091 producers/consumers, logs via
the slog bridge (the mock's log lines: `order created …`, `charge failed: provider timeout …`, `retrying message attempt=2 …`,
`SMTP 421 …`). The SQL is the mock's top queries (`infra.ts` `QUERY_DEFS`).

**Not instrumented** (`internal/sim`): inventory-api, iyzico, Stripe (local fake HTTP APIs), Kafka consumers
`analytics.s3-sink`, `audit-log.sink`, `ledger-sync`, `reco-trainer` (batch), RabbitMQ `crm-sync`, the backoffice
campaign publisher.

**Clients**: shop-web, shop-mobile (RN), backoffice-web, seller-portal (Vue), landing-web, courier-app (Flutter) as RUM
sessions: SESSION_START, APP_START (mobile), VIEW_START with `loading_time_ms` = TTFB + the slowest blocking API call
(measured for real) + render, Web Vitals (TTFB/FCP/LCP/CLS/INP) or frame vitals, RESOURCE events with the `trace_id`/
`span_id` sent as `traceparent` to the backend (like the JS/RN/Flutter SDKs: traceparent only, no baggage), actions,
JS errors / crashes, checkout journeys (POST /v1/orders → POST /v1/charges), VIEW_END. Plus kubelet probes, Stripe webhooks
(UA `Stripe/1.0`), a partner ERP and uninstrumented clients. Traffic follows the mock's daily curve (±25 %, peak ~15:00)
with random-walk noise; think time is compressed (~9 s per view).

**Collector**: `services/collector` `trooper-agent` (built, not modified) with `.run/collector.yaml` → db_1m / db_queries_1m
(pg_stat_statements, MySQL digests, Redis INFO) and broker_* for `kafka-main` (shared Redpanda, filtered to the demo's
topics/groups) and `rabbit-main` (vhost `/shop`).

**Simulated infrastructure** (`internal/infrasim`, `--infra-sim`, default on; `--infra-sim=false` turns it off): what
`trooper-agent --agent-modes host,cluster` and the AWS Lambda integration would send for a production-like setup, as
OTLP to ingest-gw with the demo ingest key (contracts §20, §21) — nothing of it runs for real:
- EKS cluster **`demo-eks`** (`eu-central-1`): 3 nodes `ip-10-0-{1-23,2-117,3-54}.eu-central-1.compute.internal`, every
  demo service (+ `inventory-api`) as a Deployment in namespace `shop` (≥ 2 replicas, `trooper.service` = service name),
  kube-system (`aws-node`, `kube-proxy` DaemonSets, `coredns`), `tasktrooper` (`trooper-agent-host` DaemonSet,
  `trooper-agent-cluster`), CronJob `nightly-report`. Every 15 s host / container / top-process metrics per node
  (resource `host.name` = node, `k8s.node.name`, `k8s.cluster.name`, `cloud.*`), every 30 s node / pod / workload state.
- Docker host **`ci-runner-1`** (no cluster): `gitlab-runner`, `buildkitd` (CPU burst every ~6 min).
- **Incident coupling:** while `reco-db-slow` is on, one `recommendation-api` pod leaks memory up to its 512 MiB limit and
  is OOM-killed every ~3–4 min (restarts ↑, `trooper.k8s.pod.reason=OOMKilled`, ready=0 for 40 s, workload ready 1/2)
  with Warning events `BackOff` (pod) + `OOMKilling` (node) → the RCA candidate "Pod … restarted (OOMKilled)".
  `payments-provider-errors` adds `Unhealthy` readiness-probe events on payments-api and lifts the error rate / latency
  of the `payment-webhook-handler` Lambda; a `FailedScheduling` event recurs every ~10 min.
- **Lambda** (resource `service.name=trooper-cloud`, `cloud.provider=aws`, per provider minute): `image-resizer`,
  `payment-webhook-handler`, `invoice-pdf-generator` (SQS, throttles at the afternoon peak), `catalog-search-indexer`
  (Kinesis, iterator age) — `trooper.faas.id` = `aws-eu-central-1-<name>`. At start the 4 rows are upserted into
  `serverless_functions` (project 1) **only when that table exists** (migration 011); otherwise only metrics are sent.

**Seeded metadata** (project 1, via the api; channels + service language via Postgres — no api endpoint): 11 databases
and 2 clusters pointing at the containers, monitors from `mock.ts` (charges error rate / p95, consumer lag rules incl.
`reco-trainer` not_drained and `ledger-sync` rebalances, log match, RUM vital, throughput drop, job_missed, …), the 8 KPIs
of `kpi.ts` with their burn-rate rules, notification channels (`#payments-alerts`, `PagerDuty`, … → webhook sink
`127.0.0.1:57999`), the 3 load-test scenarios. Re-running only adds what is missing (match by id / name).

## Scenarios (default: all on)

| Scenario | How it is produced for real |
|---|---|
| `reco-db-slow` | "migration 0042": `DROP INDEX idx_reco_items_user_score` on ~900k rows → the top query seq-scans; recommendation-api, storefront-bff `/v1/home`, shop-web `/` LCP and the *Homepage loads fast* KPI degrade. Restored on the next `run` |
| `orders-db-connections` | orders-api warms an idle pool of 44 (max_connections 60) |
| `fulfilment-hot-partition` | 31 % of orders are merchant `m-1042` (key = merchant_id → one partition); the member owning it is capped at 60 % of that partition's inflow → lag grows on one partition |
| `ledger-sync-rebalancing` | ledger-sync members process slowly and leave/rejoin every few minutes |
| `payments-provider-errors` | iyzico fake: slower, 3.5 % 502, 1.2 % timeouts → POST /v1/charges errors + p95 |
| `cache-evictions` | auth-api writes 2 KB `session:*` keys without TTL into cache-main (maxmemory 24 MB, allkeys-lru) |

## Containers / ports (127.0.0.1)

Postgres 16 + pg_stat_statements: orders-db 57432, payments-db 57433, reco-db 57434, catalog-db 57435, auth-db 57436
(user `postgres` / `ttdemo`, one container per DB because pg statistics are cluster-wide). Redis: cache-main 57379,
cache-payments 57380, cache-bff 57381, reco-cache 57382. MySQL 8.4 notifications-db 57306 (`root`/`ttdemo`). RabbitMQ
57672 (AMQP) / 57673 (management), `ttdemo`/`ttdemo`, vhost `/shop`. Kafka topics on the shared Redpanda keep the mock's
names (`orders.created`, `payments.events`, …) so the UI's links between consumers, dependencies and topics hold; `demo down`
deletes exactly those topics and groups.

## Notes

- One OS process per service instance because the Go SDK installs global OTel providers (one `service.name` per process).
- The mock's languages (Node.js, Python) are written to `services.language`; the code is Go.
- `orders-db`/`cache-main` etc. are registered with `collector: agent` (local containers), not `cloud` as in the mock.

## Replay, profiles, AI (ADR-021..023)

Everything goes through the real pipeline (ingest-gw → Redpanda → writer → ClickHouse / S3); `false` turns a part off.

**Session replay** (`--replay`, `--replay-sample 0.3`; `internal/sim/replay.go`, `replaylayout.go`). Every RUM session
gets a recorder with the SDK's sampling: `--replay-sample` of the sessions record from the start, the others keep the last
30–60 s in memory (checkout snapshot every 30 s) and are flushed and recorded from their first `ERROR` on
(`onErrorSampleRate` 1.0). Segments (≤ 5 s, gzip) go to `POST /v1/replay` with the session's `session_id`, `view_id/name`,
user / anonymous id and stats (clicks, errors, rage clicks, inputs); replay events are emitted at the same instants as the
RUM events (VIEW_START, ACTION, ERROR), so the player timeline lines up. Each page / screen is a small layout model
(header, hero, product grids, tables, forms, order tracking, …) per app:
- web apps (shop-web, backoffice-web, seller-portal, landing-web): **rrweb 2.x** — Meta + FullSnapshot at the first page,
  one Mutation per route change (skeleton while the data loads, then the content), mouse paths, clicks (touch on phones),
  rage clicks, scrolls, masked inputs (search box, checkout / edit forms; `*`). Text is masked like `maskAllText`, chrome
  labels carry `data-trooper-unmask`.
- mobile apps (shop-mobile, courier-app): **wireframe** — `screen` per view, full `frame`s (≤ 2/s) on load / scroll / typing,
  `touch` down/move/up, `keyboard`; platform `ios` / `android` from the device. (The SDKs record wireframes on native
  Android / iOS only; the demo also does it for its React Native and Flutter apps.)

**Profiles** — `--profiling`: every service process runs `trooper.WithProfiling()` (real pprof of the demo processes:
cpu 10 s/60 s, alloc_space, inuse_space, goroutines; host = this machine). `--profile-sim` (`internal/profsim`): per pod of
the simulated cluster (`host` = infrasim pod name) every 60 s a folded `cpu` (10 s window), `alloc_space` (60 s) and
`inuse_space` profile with realistic Go stacks per route (`# trooper.route=… trooper.method=…`; consumers: process +
`CONSUME`), scaled by the daily traffic curve. At start `--profile-backfill` (30 min) of the **previous** version is
uploaded, the current version is live from then on ("deployed at demo start"), so version and before/after diffs work at
once:
- orders-api `v2.40.3 → v2.41.0`: `pricing.ApplyDiscounts` compiles promo regexps per request (`regexp.Compile` new,
  `(*RuleSet).Match` ×3) on `POST /v1/orders` and `GET /v1/cart` — CPU and alloc up, +14 MiB retained.
- catalog-api `v3.1.4 → v3.2.0`: semantic search (`search.(*Embedder).Embed`, `search.Rerank`) on `GET /v1/search`.
- recommendation-api `v0.13.2 → v0.14.0`: `sort.Slice` replaced by `slices.SortFunc` — `rank.Blend` cheaper.
- email-worker `v0.9.11 → v0.9.12`: Gemini subject personalization in `newsletter-sender`.

**AI / LLM** (`--llm`; `internal/svc/llm.go`, Go SDK `llm.Start/End`, spans inside the real request / message traces →
`ai` dependencies, `llm_1m`):
- storefront-bff `POST /v1/assistant/messages` (new route; called by shop-web / shop-mobile from Account and order
  tracking, 30 % of those views): support assistant on **anthropic `claude-sonnet-4-5`** (response model
  `claude-sonnet-4-5-20250929`), streaming with TTFT (~0.6 s) and ~78 tok/s, 2,312-token cached system prompt (cache write
  on a first turn, cache read otherwise), tool use `get_order_status` (`execute_tool` span around a real orders-api call)
  then the answer, 2.5 % `429` (retried once) and 0.4 % `529`. **Content capture is on for this service only**
  (`llm.SetCaptureContent`) with invented, PII-free questions / answers → `llm_content`.
- catalog-api `GET /v1/search` (query) and `POST /v1/items` (item text): **openai `text-embedding-3-small`** embeddings,
  0.4 % `429` (falls back to keyword search).
- email-worker `newsletter-sender`: **gcp.gemini `gemini-2.5-flash`** `generate_content` for 75 % of newsletters
  (implicit cache reads, 1.5 % `429 RESOURCE_EXHAUSTED` → default subject).
