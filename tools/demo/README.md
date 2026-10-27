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
alert-engine `:8090`, notifier `:8091`; built with `GOWORK=off go build` into `tools/demo/.run/bin`), then `seed`, then `run`.
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
`--infra-sim` (default `true`, see below).

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
