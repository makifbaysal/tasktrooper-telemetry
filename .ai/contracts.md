# Contracts between services and SDKs

This file defines everything where backend services and SDKs touch each other. Parallel development relies on this contract:
if something written here must change, **this file is updated first**, then the code. Architecture: `architecture.md`, decisions: `decisions.md`
(especially ADR-010, ADR-011).

## 1. Repo layout and ownership

```
services/                      Go 1.26, multi-module via go.work
  go.work
  pkg/                         github.com/tasktrooper/telemetry/services/pkg   (shared; no external dependencies)
  ingest/                      .../services/ingest    cmd/ingest-gw, cmd/ingest-writer
  api/                         .../services/api       cmd/api   (+ openapi.yaml)
  alerting/                    .../services/alerting  cmd/alert-engine, cmd/notifier
  collector/                   .../services/collector cmd/trooper-agent (DB + broker collector)
  integrations/                .../services/integrations cmd/trooper-cloud (AWS, GCP, Neon, Vercel — ADR-012)
  codeintel/                   .../services/codeintel   (repo analysis, webhook — ADR-014)
  loadtest/                    .../services/loadtest    loadtest-ctl + runner (ADR-015)
  mcp/                         .../services/mcp         cmd/tt-mcp (MCP server, stdio — ADR-025, §26)
                               toolset/: exported tool surface, imported by api for the dashboard assistant (ADR-033)
tools/
  trooper-cli/                 sourcemap / dSYM / mapping upload CLI (§12)
deploy/
  docker-compose.yml           clickhouse, postgres, redpanda, redis (+ services with profile "app")
  clickhouse/*.sql             ClickHouse schema (ordered, idempotent: CREATE ... IF NOT EXISTS)
  postgres/*.sql               Postgres schema
proto/rum/v1/rum.proto         RUM wire schema (JSON mapping is used)
sdks/
  go/                          github.com/makifbaysal/tasktrooper-telemetry/sdks/go (separate module, not in go.work)
  jvm/                         Gradle multi-project: javaagent extension, spring-boot-starter, quarkus, ktor
  dotnet/                      TaskTrooper.Telemetry (+ tests)
  js/packages/                 browser, react, vue, angular, react-native (pnpm workspace)
  android/                     Gradle library (Kotlin)
  ios/                         Swift Package
  flutter/                     Dart/Flutter package
```

Each module/folder has a single owner (agent/developer); do not touch another module's `go.mod`. If something must be added to
`services/pkg`, keep it small (the only external dependency is `google.golang.org/protobuf`) and add a note to this file.

`services/pkg` contents: `topics` (topic/header names), `tenant` (Tenant + context), `config` (TT_* env reading),
`logx` (JSON slog), `secrets` (AES-GCM + credentials ref resolution), `llmprice` (LLM price table + cost, embedded defaults, org override cache; §25), `rumpb` (generated from `proto/rum/v1` with buf: `cd proto && go run github.com/bufbuild/buf/cmd/buf@v1.57.0 generate`).

**Environment variables** (defaults assume host → docker-compose): `TT_POSTGRES_URL`, `TT_CLICKHOUSE_URL`
(`clickhouse://trooper:trooper@localhost:9000/telemetry`), `TT_KAFKA_BROKERS` (`localhost:19092`; from inside a container `redpanda:9092`),
`TT_REDIS_URL`, `TT_LOG_LEVEL`, `TT_ENV` (empty = development; `production` → production-only guards, e.g. the §10 secrets key;
`services/pkg/config.Production`). Service-specific variables use the `TT_<SERVICE>_*` prefix (e.g. `TT_INGEST_HTTP_ADDR`).

## 2. Network endpoints and authentication

| Service | Port | Note |
|---|---|---|
| ingest-gw HTTP | 4318 | `POST /v1/replay` (§23), `POST /v1/profiles` (§24), `POST /v1/traces`, `/v1/metrics`, `/v1/logs` (OTLP/HTTP protobuf **and** JSON; `Content-Encoding: gzip` supported), `POST /v1/rum` (RUM JSON, gzip optional), `GET /healthz` |
| ingest-gw gRPC | 4317 | OTLP gRPC (TraceService, MetricsService, LogsService) |
| api | 8080 | `/api/v1/...` (see `services/api/openapi.yaml`), `GET /healthz` |
| alert-engine | 8090 | only `/healthz`, `/metrics` |
| ingest-gw metrics | 4318 (or `TT_INGEST_METRICS_ADDR`) | Prometheus `GET /metrics` |
| ingest-writer metrics | 9464 (`TT_INGEST_WRITER_METRICS_ADDR`) | Prometheus `GET /metrics` |

### 2a. Production hosts and DNS (ADR-030)

| Host | Target | DNS record |
|---|---|---|
| `telemetry.tasktrooper.ai` | `apps/web` static build (SPA fallback: unknown path → `index.html`) | CNAME (or ALIAS if not apex) → web host / CDN |
| `api.telemetry.tasktrooper.ai` | api :8080 | CNAME → load balancer |
| `ingest.telemetry.tasktrooper.ai` | ingest-gw :4318 (HTTP) + :4317 (gRPC, h2) | CNAME → load balancer (with HTTP/2 support for gRPC) |
| `hooks.telemetry.tasktrooper.ai` | codeintel :8096 | CNAME → load balancer |
| `symbols.telemetry.tasktrooper.ai` | symbolicator :8095 | CNAME → load balancer |

All use TLS on 443 (a single `*.telemetry.tasktrooper.ai` + `telemetry.tasktrooper.ai` certificate is enough). Optional: a `CAA` record restricted
to the certificate provider.

**Env values (production):**

| Service | Env | Value |
|---|---|---|
| web build | `VITE_API_URL` | `https://api.telemetry.tasktrooper.ai` |
| api | `TT_WEB_URL` | `https://telemetry.tasktrooper.ai` |
| api | `TT_API_CORS_ORIGINS` | `https://telemetry.tasktrooper.ai` |
| api | `TT_PUBLIC_API_URL` | `https://api.telemetry.tasktrooper.ai` |
| api | `TT_PUBLIC_INGEST_URL` | `https://ingest.telemetry.tasktrooper.ai` |
| api | `TT_PUBLIC_WEBHOOK_URL` | `https://hooks.telemetry.tasktrooper.ai` |
| api | `TT_OIDC_REDIRECT_URL` | `https://api.telemetry.tasktrooper.ai/api/v1/auth/callback` (must be registered with the IdP) |
| api | `TT_COOKIE_SECURE` | `true` |
| integrations | `TT_PUBLIC_INGEST_URL` | `https://ingest.telemetry.tasktrooper.ai` (Vercel drain) |
| trooper-cli | `TROOPER_API_URL` | `https://symbols.telemetry.tasktrooper.ai` |
| mcp (`tt-mcp`) | `TT_API_URL` | `https://api.telemetry.tasktrooper.ai` (demo: `https://telemetry-api.tasktrooper.ai`, ADR-031) |

Third-party registrations: Slack App redirect URL `https://api.telemetry.tasktrooper.ai/api/v1/integrations/slack/callback`, GitHub App
callback `https://api.telemetry.tasktrooper.ai/api/v1/integrations/github/callback` and webhook URL
`https://hooks.telemetry.tasktrooper.ai/v1/webhooks/github`.

**Internal credential:** internal senders (trooper-cloud, loadtest-runner) send `x-trooper-internal: <TT_INTERNAL_TOKEN>` +
`x-trooper-project: <id>` (OTLP HTTP/gRPC metadata, `/v1/rum`); constant-time comparison, 403 if `Origin` is present, 401 if the token
is not set. Org comes from Postgres (60 s cache).
**Ingest auth:** `x-trooper-key: <ingest key>` header, or `Authorization: Bearer <key>`. Because browser `sendBeacon` cannot set
headers, a `?key=` query parameter is also accepted (only `/v1/rum` and `/v1/replay`). Keys are stored in the Postgres `api_keys` table as a sha256
hash; ingest-gw keeps an in-memory cache with a 60 s TTL. Key → `(org_id, project_id)`. Browser keys are checked against `allowed_origins`.
**DSN:** `https://<key>@<host>[:port]/<project_id>` — SDKs derive the endpoint and key from the DSN. Local: `http://<ingest-key>@localhost:4318/<project-id>`.
**No seed (ADR-028):** org/project/key are created in onboarding or with `api bootstrap`; ingest and api keys come from dashboard Settings → API keys (`tt_ingest_…` / `tt_api_…`).

**`/v1/rum` content types:** `application/json`, `application/x-protobuf` and `text/plain;charset=UTF-8` (body is JSON —
this is the only one `sendBeacon` can send without a preflight). **CORS:** the `OPTIONS` preflight allows the `content-type`, `x-trooper-key`,
`content-encoding` headers; `Access-Control-Allow-Origin` follows the key's `allowed_origins` (if empty, the request origin).
The customer's cross-origin APIs must allow the `traceparent` (and `baggage`) header in CORS — the SDK docs + onboarding say this.

Responses: 200 (OTLP: empty `Export*ServiceResponse`), 400 (parse), 401 (key), 413 (> 5 MB body), 429 (rate limit; `Retry-After`).
SDKs use exponential backoff on 429/5xx (1s→60s, jitter) and drop the batch on 4xx (except 429). Backend SDKs that use OTel exporters
may rely on the upstream retry policy (429/502/503/504, at most 5 attempts).

**Shared SDK config names** (the same in all languages; unset ones fall back to the OTel default, explicitly set `OTEL_*`/`otel.*` win):
`TROOPER_DSN`, `TROOPER_SERVICE`, `TROOPER_ENV`, `TROOPER_VERSION`, `TROOPER_SAMPLE_RATE`, `TROOPER_ENABLED`
(in property files `trooper.dsn`, `trooper.sample-rate`, `trooper.enabled` …). The resource carries `telemetry.distro.name`
(`trooper-go`, `tasktrooper-javaagent`, `trooper-dotnet` …).

**Demo (ADR-031):** single machine + Cloudflare Tunnel; hosts sit one level below the zone (the free certificate does not cover two levels): `telemetry.tasktrooper.ai` → web (`127.0.0.1:8081`), `telemetry-api.tasktrooper.ai` → api (8080), `telemetry-ingest.tasktrooper.ai` → ingest-gw OTLP/HTTP (4318; no gRPC). DSN: `https://<ingest-key>-ingest.tasktrooper.ai/<project-id>`. Env: `deploy/demo/.env.example`.

## 3. Kafka (Redpanda) topics — ingest-gw → ingest-writer

| Topic | Value | Key |
|---|---|---|
| `tt.otlp.traces` | `ExportTraceServiceRequest` protobuf (converted to protobuf even if it arrives as JSON) | `project_id` (string) |
| `tt.otlp.metrics` | `ExportMetricsServiceRequest` protobuf | `project_id` |
| `tt.otlp.logs` | `ExportLogsServiceRequest` protobuf | `project_id` |
| `tt.rum` | `RumBatch` protobuf (`proto/rum/v1`) | `project_id` |
| `tt.replay` | gzipped replay segment JSON (§23) | `project_id` |
| `tt.profiles` | raw profile body (pprof / folded, §24) | `project_id` |

Headers (all strings): `org_id`, `project_id`, `received_at` (unix ms), `client_ip` (only for geo, dropped after the writer),
`user_agent` (RUM only). Topics have 24 h retention, 24 partitions (dev: 3). Writer consumer group: `tt-ingest-writer`.

## 4. Semantic conventions (SDK → writer)

OTel semantic conventions are the basis. The writer reads these attributes (span or resource):

| Attribute | Written by | Meaning |
|---|---|---|
| `service.name`, `service.version`, `deployment.environment.name` | resource | service, version (deploy marker), environment |
| `trooper.route` | SDK (server span) | normalized route template (`/v1/orders/:id`). If missing, the writer normalizes `http.route`, and if that is missing too, `url.path` (number/uuid/hex → `:id`) |
| `trooper.entry.route` | SDK (**all** spans within the same request) | route of the entry span they belong to (process name for consumers) → `edges_1m` |
| `trooper.entry.method` | SDK (together with `trooper.entry.route`) | HTTP method of the entry span; `CONSUME` for consumers, `GRPC` for gRPC |
| `trooper.process` | SDK (consumer span) | consumer process name (if missing, `messaging.consumer.group.name`; if that is missing too — RabbitMQ / SmallRye — `messaging.destination.name`) |
| `messaging.system`, `messaging.destination.name`, `messaging.consumer.group.name`, `messaging.destination.partition.id` | OTel | consumer: source, topic/queue, group |
| `trooper.consumer.attempt` (int), `trooper.consumer.dlq` (bool) | SDK | retry count, sent to DLQ |
| `trooper.caller` | SDK (server span) | calling service name; the calling SDK puts `tt.svc=<service>` into W3C baggage and the other side reads it. If missing, the writer classifies from the user-agent (`web`, `mobile`, `probe`, `unknown`) |
| `peer.service` / `db.system` + `server.address` / `messaging.system` | OTel (client span) | dependency name: writer `dep` = `"<db.system> · <db.namespace or server.address>"`, for HTTP `"HTTP · <peer.service or server.address>"`, for queues `"<messaging.system> · <destination>"` |
| `db.query.text` | OTel | the writer normalizes it (literal → `?`), `query_hash` via `cityHash64` |

**Route normalization (all SDKs + writer):** `{id}`, `{id:regex}`, `<id>`, `:id` → `:id`; catch-all `{p...}`, `*p`, `{*p}` → `*p`;
gRPC route = full method name (`/pkg.Service/Method`). Raw number/uuid/hex/ulid segments → `:id`.

**Writer completions:** on non-entry spans `route = entry_route`; if the SDK did not write `trooper.entry.route`, the writer fills it from the
nearest entry-span ancestor in the same batch. Webhook callers get `caller=<vendor>` (e.g. `Stripe`), `caller_kind=webhook`.
Exception type/message are copied into `attrs` (read by `errors_1h`).

Entry span = `kind ∈ {server, consumer}` and its parent is from another service (or none). RED/consumer rollups come only from entry spans.

**Background job entry:** periodic work that does not come from a broker, such as a scheduler tick, a poll that finds work, or a batch, is also an entry:
span kind `consumer`, no parent from the same service (if there is a local parent, a new root + link; a remote parent — e.g. a `TRACEPARENT`
given by another service — is kept), `trooper.process` = job name (low cardinality: `alert.cycle`, `billing.sync`; tenant/id
go in attributes), `trooper.entry.route` = job name, `trooper.entry.method` = `JOB`; `messaging.system`, `messaging.destination.name`,
`messaging.consumer.group.name` are **not written** (it is not messaging). The writer fills this span's `messaging_system` column with `job`
(only when `kind=consumer`, there is no `messaging.system` and `trooper.entry.method=JOB`) → it lands in `consumer_1m` with `process` = job name,
empty `topic`/`consumer_group`, `messaging_system = 'job'` (throughput, errors, duration, `job_missed` monitor).
The api does not show `messaging_system = 'job'` rows as queue/topic/consumer group (in the process list the method is `JOB`,
no source/topic; no lag); only a service with job entries has type `worker`. A job entry is opened only in a run that does work —
an empty poll does not produce a span, otherwise throughput would equal the poll rate. Go: `trooper.StartJob` / `trooper.RunJob` (sdk.md).

**Sampling:** RED metrics must be counted before sampling → even when SDKs sample, they mark entry spans with `trooper.sample_rate` (float)
and the writer multiplies counts by `1/sample_rate`. With parent-based sampling the effective rate is the upstream's
rate: the SDK that starts the root span writes the rate to W3C `tracestate` as `tt=r:<rate>`; entry spans with a remote parent
take `trooper.sample_rate` from there (otherwise they write their own config rate).

**Carrying the retry count:** between retry topics / re-publishes the attempt count is carried in the message header `tt-attempt` (decimal string);
the consumer SDK writes it to `trooper.consumer.attempt` (on RabbitMQ `x-delivery-count`/`x-death` are also read,
on Spring Kafka `kafka_deliveryAttempt`).

**SDK internal metric:** every backend SDK counts the errors it swallows in the `trooper.sdk.errors` counter (meter `TaskTrooper.Telemetry`,
attr `component`). Profiling/LLM components: `profiler`, `profiler.cpu`, `exporter.profiles`, `llm`.

**SDK heartbeat (presence signal):** every backend SDK (Go, JVM, .NET, Python, Node), from the moment it starts and even without traffic, sends
the `trooper.sdk.heartbeat` gauge (value 1, meter `TaskTrooper.Telemetry`, no attributes — the info is on the resource: `service.name`,
`service.version`, `deployment.environment.name`, `service.instance.id`, `telemetry.sdk.*`) at the metric export interval (default 30–60 s);
the first export is ≤ 10 s after start. If metrics are disabled (`DisableMetrics` etc.), no heartbeat is sent either.
The api service list shows a service that has no traffic but sends a heartbeat (or any span/metric) as `worker`/known type with 0 rpm;
service detail shows "SDK connected, no traffic yet" + last seen time (`lastSeen`). Frontend/mobile SDKs
do not send a heartbeat because they already send a RUM event at session start.

## 5. Collector metrics (trooper-agent → OTLP metrics → writer)

The collector sends DB and broker data to ingest-gw as **OTLP gauge/sum**; the writer routes these names to typed tables.

DB (`db_1m`): `trooper.db.cpu` (0..1), `trooper.db.memory` (0..1), `trooper.db.disk` (0..1), `trooper.db.connections`,
`trooper.db.max_connections`, `trooper.db.ops` (/s), `trooper.db.latency.p95` (ms), `trooper.db.slow` (/min),
`trooper.db.hit_ratio` (0..1), `trooper.db.evictions` (/min), `trooper.db.replication_lag` (ms).
Attributes: `trooper.db.id`, `trooper.db.name`, `db.system` (postgresql|mysql|redis|mongodb), `trooper.db.node`, `trooper.db.role`.

DB queries (`db_queries_1m`): `trooper.db.query.calls`, `trooper.db.query.total_ms`, `trooper.db.query.p95_ms`, `trooper.db.query.rows`
+ `trooper.db.id`, `trooper.db.query.hash`, `db.query.text` (normalized).

Broker lag (`broker_lag_1m`): `trooper.broker.lag`, `trooper.broker.end_offset`, `trooper.broker.committed_offset`,
`trooper.broker.consume_rate`, `trooper.broker.produce_rate`, for RabbitMQ `trooper.broker.unacked`.
Attributes: `trooper.broker.cluster`, `messaging.system` (kafka|rabbitmq), `messaging.destination.name`,
`messaging.consumer.group.name`, `messaging.destination.partition.id` (RabbitMQ: `0`).
Group state (`broker_groups_1m`): `trooper.broker.group.members`, `trooper.broker.group.rebalances` (+ `trooper.broker.group.state` attr).
Broker node (`broker_nodes_1m`): `trooper.broker.node.cpu|memory|disk|net_in` + `trooper.broker.node` (on Kafka only the node name `broker-<id>`
coming from log-dir, maps to `nodes[].brokerId`). Extra RabbitMQ gauges `trooper.broker.node.fd` and `.proc`
(0..1 usage ratio) → generic `metrics` table.

Topic / queue (`broker_topics_1m`): `trooper.broker.topic.partitions`, `.replication`, `.retention_ms`, `.avg_msg_bytes`,
`.in_rate` (msg/s), `.bytes_in_rate` (byte/s), `.size_bytes` + `trooper.broker.cluster`, `messaging.destination.name`.
`.retention_ms = -1` → unlimited (UI shows "∞"). Kafka `size_bytes` is the (compressed) size on disk.
Queue age: `trooper.broker.oldest_age_s` → `broker_lag_1m.oldest_age_s` (SQS `ApproximateAgeOfOldestMessage`, Pub/Sub
`oldest_unacked_message_age`, RabbitMQ head message timestamp). `messaging.system` ∈ `kafka | rabbitmq | aws_sqs | gcp_pubsub`.
Broker node `net_in` unit is **MB/s**.

Source: metrics from cloud integrations carry `service.name=trooper-cloud`, `trooper.integration.id`,
`cloud.provider` (`aws|gcp|neon|vercel`), `cloud.region` on the resource; names are the same as above.

All other metrics go to the `metrics` table (custom).

**Unreported value:** in collector tables missing `max` columns are written as `-Inf`, `hit_ratio` (min) as `+Inf` (NaN gives a result that depends on row order in ClickHouse
max/min). Readers (api, alerting) treat infinite values as "no data" (`isFinite(x)`).

Extra rules:
- `trooper.db.query.calls|total_ms|rows` are **delta Sum** (the writer sums per minute); `p95_ms` and all other `trooper.db.*` /
  `trooper.broker.*` values are gauges (last snapshot).
- `trooper.db.query.hash` is the engine's own id (decimal uint64: pg `queryid`, first 64 bits of the MySQL digest, FNV of the Redis command name).
  It does not match `db_query_hash` (cityHash64) on spans; query ↔ endpoint matching is done via the normalized text.
- `trooper.db.id` and `trooper.broker.cluster` = the `slug` in Postgres.
- Collector health: `trooper.collector.up` (0/1) + `trooper.collector.target`, `trooper.collector.kind` → `metrics` table.
- `GET /api/v1/internal/collector-config` JSON shape: `services/collector/README.md` (DB user name `databases.username`,
  `trooper_monitor` if empty).

## 6. ClickHouse

Schema: `deploy/clickhouse/001_init.sql`. `MergeTree` for single-node dev; in prod the same DDL with `ON CLUSTER` + `Replicated*` + `Distributed`
(the migration tool picks via an environment variable). Writer rules:
- Batch: ≥ 50k rows per table or 1 s, whichever comes first. If the insert fails, the Kafka offset is not committed (at-least-once).
- Rollups (`red_1m`, `edges_1m`, `callers_1m`, `consumer_1m`, `rum_views_1m`, `rum_vitals_1m`, `trace_index`) are materialized views;
  the writer only writes to raw tables (+ collector typed tables).
- The query side (api, alerting) uses `sum(...)` on rollups, `quantilesTDigestWeightedMerge(...)` for quantiles (`red_1m/1h`,
  `consumer_1m`, `edges_1m`, `callers_1m` keep weighted digests), and `quantilesTDigestMerge(...)` on RUM rollups.
- Monitor `query` JSON shapes (per kind): `services/alerting/README.md`. A monitor update bumps `updated_at`
  (trigger, `deploy/postgres/003_alerting.sql`); the alert-engine monitor cache watches it.

## 7. Postgres

Schema: `deploy/postgres/001_init.sql`, seed `002_seed.sql`. Owner is `api`; alerting reads/writes `monitors`, `incidents`, `notification_channels`,
`kpis`, `kpi_alert_rules`. The collector reads `databases` and `broker_clusters` from the api (`GET /api/v1/internal/collector-config`,
with an api key) and does not connect to the DB directly.

Dashboard assistant (ADR-033, `deploy/postgres/015_assistant.sql`, owner api): `assistant_threads`, `assistant_messages`
(`content` UI shape + `transcript` model messages), `assistant_attachments` (images, bytea ≤ 5 MB). Private to the user, project scoped.

## 8. API (the dashboard's backend)

`services/api/openapi.yaml` is the single source. Rules: JSON, camelCase fields; time `ts` in unix ms; durations in ms; ratios 0..1;
`?range=1h|6h|24h|7d` (`TimeRange` in the UI) and `?env=`. Series are `{ timestamps: number[], series: [{ id, label, data: number[] }] }`.
Response shapes match `apps/web/src/data/types.ts` (+ `infra.ts`) exactly; in the UI the api mapping lives only in `src/data/*`
functions. Auth: `Authorization: Bearer <api key>` (dev) — OIDC session later.

`GET /service-map?range=&env=` returns the whole system graph: `nodes` (`tier` client|service|data|external, `kind`, `system`, `status`, rpm/errorRate/p95, heaviest open `incident`) + `edges` (from→to, callsPerMin, errorRate, p95, `status` from error rate: ≥1% warning, ≥5% critical). Incoming edges come from `callers_1m` (callee view), dependencies from `edges_1m`; an http call whose `child_service` resolves to a service that sends telemetry is a service→service edge, an unresolved `peer.service` becomes an external (`external`) node. Dependency node id is `dep:<system> · <name>`.

`/assistant/*` (ADR-033, `.ai/assistant.md`): session-only chat API; `POST …/messages` and `POST …/approvals/{toolUseId}` answer with
`text/event-stream` (events `message`, `text`, `tool`, `widget`, `approval`, `error`, `done`). Viewers may call it (writes made by its
tools are still checked per request). Env: `ANTHROPIC_API_KEY`, `TT_ASSISTANT_MODEL`, `TT_ASSISTANT_MAX_TOKENS`, `TT_ASSISTANT_MAX_STEPS`,
`TT_ASSISTANT_ALLOW_WRITE`.

## 9. RUM

`proto/rum/v1/rum.proto`. In JSON, field names are the snake_case names from the proto (`protojson` `UseProtoNames`); int64s may be sent
as numbers. A batch is one resource (app/device) + N events. Client SDK rules: batch ≤ 100 events or 5 s,
flush when the page/app goes to the background (web: `sendBeacon`/`fetch keepalive`), queue to disk/IndexedDB when offline (≤ 5 MB, FIFO drop).
For network spans a `traceparent` (W3C) is generated and `trace_id`/`span_id` are written to the `resource` event.

**View loading time:** `loading_time_ms` is carried on the **VIEW_START** event (the writer writes it to `duration_ms`,
`rum_views_1m.load` is computed only from view_start). So SDKs may delay VIEW_START until the screen's first frame / the page's
load; VIEW_END carries `time_spent_ms`. `loading_time_ms` missing/0 = unknown (page hidden before
load); the rollup excludes these with the `duration_ms > 0` condition.

**Known extra attributes** (`RumEvent.attributes`): ACTION → `click_count`; ERROR → `component_stack`, `error_boundary`,
`component`, `vue_info`; APP_START → `start_reference`; frame vitals → `total_frames`.

**Frame vitals:** `SLOW_FRAMES` and `FROZEN_FRAMES` are sent as a **count** per view, with `attributes.total_frames`
added; the backend computes the ratio (slow / total). Thresholds: slow > 16 ms (≤ 17.5 ms tolerance accepted for display-link jitter), frozen > 700 ms.

**APP_START:** `attributes.start_reference` = `process` (from OS process start) | `sdk_init` (when the native SDK
cannot get the process time, e.g. Android API < 24 or iOS pre-warm) | `foreground` (hot start: return from background) | `dart_init` / `js_init` (from framework init). Anything other than `process`
comes out shorter; the UI flags it.

**`connection`:** `wifi | cellular | 4g | 3g | offline | unknown`; ethernet/VPN/other → `unknown`.

**Freeze / ANR:** Android ANR and iOS hang use the same model: ERROR, `source=anr`, `type` = `ANR` | `AppHang`.
Crashes/hangs from MetricKit carry `attributes.metrickit=true`; the same crash may also arrive via the SDK report, and the server dedupes
(session + time window + fingerprint).

**Hybrid apps (Flutter / React Native + native SDK):** if a native SDK is present, session and crashes are shared. Channel name
`dev.tasktrooper.telemetry/rum` (Flutter MethodChannel — `tasktrooper_flutter` plugin, on iOS the `tasktrooper_flutter/Native` subspec for the native SDK; on RN the native module name is `TrooperRum` (TurboModule + old architecture), JS looks for `TrooperRum` first, then this channel name; RN ≥ 0.74, on iOS `TROOPER_RN_NATIVE_SDK=1 pod install` for the native SDK):
- `setContext({session_id, anonymous_id, view_id, view_name, user_id, sampled})` — JS/Dart → native; native crash reports are sent
  with this context. `sampled` (bool, optional) is the JS/Dart RUM sampling decision; native uses it (if missing, old behavior). An adopted
  session is an **external session** on the native side: native does not rotate it by the inactivity/4-hour rule; JS/Dart owns it.
- `getContext()` → same fields — native → JS/Dart (if native started first, its session is used).
- `getProcessStartTime()` → unix ms — for APP_START `start_reference=process`.
- `isNativeReplayActive()` → bool — Flutter does not start Dart wireframe recording if native replay is set up (Android reflection; iOS for now
  always `false` → "do not enable both" doc rule).
If there is no native handler, calls fail silently. The Dart/JS layer owns the session: when the native SDK takes over the session via
`setContext`, it does not send its own SESSION_START.

**Ingest behavior settings (env):** `TT_TAIL_SAMPLE_RATE` (default 1; traces with error/slow entry spans are kept in full, others
by a deterministic trace-id hash, with `sample_weight *= 1/r` on kept spans), `TT_TAIL_SLOW_MS` (default 1000), `TT_GEOIP_DB` (MaxMind
mmdb; `rum_events.country` from the client IP, the IP is not stored; `geo.country` from a drain takes precedence).
**RUM clock correction:** a batch is shifted only if its newest event is > 5 min ahead of or > 24 h behind received_at;
events ≤ 24 h old coming from the offline queue keep their own time.

## 10. Cloud integrations (ADR-012)

- Postgres: `integrations`, `secrets`, `deploy_markers` (`deploy/postgres/004_integrations.sql`). Discovery upserts `databases` / `broker_clusters`
  rows: `slug` = `<provider>-<resource name>` (e.g. `aws-orders-db`), `collector='cloud'`, `integration_id`,
  `source` ('AWS RDS · eu-central-1'), `version`, `zone`, `instance_class`, `disk_gb`, `meta`. Rows added manually by the user are not
  touched (if integration_id is NULL). If the resource is deleted, the row is not deleted; `meta.missing=true`.
- `trooper-cloud` connects to Postgres directly (internal; same DB as api) and updates `integrations.status/error/last_sync_at/resources`.
  Secret resolution: `secret:<id>` → `secrets` table, AES-256-GCM, key `TT_SECRETS_KEY` (base64 32 bytes;
  if empty, a fixed dev key + warning; with `TT_ENV=production`, if empty or the dev key, the service does not start — fail-closed,
  `secrets.ErrDevKeyInProduction`). Encryption format: `nonce(12) | ciphertext+tag`,
  AAD = `project_id` (decimal string). api writes, trooper-cloud reads — both use the same small package: `services/pkg/secrets`.
- Metric/discovery interval: discovery 10 min, metrics 60 s (CloudWatch/Monitoring 1 min resolution; the last full minute is read).
- **Vercel:** deploys from the REST API (`GET /v6/deployments`) every 60 s → `deploy_markers` (source `vercel`, service =
  `config.projectMap[project] ?? project name`). Drains go to ingest-gw `POST /v1/drains/vercel?key=<ingest key>&integration=<id>`:
  signature `x-vercel-signature` = hex(HMAC-SHA1(drainSecret, rawBody)); ingest-gw resolves the drain secret via `integrations.config.drainSecretRef`.
  Body is a JSON array or NDJSON. Mapping: `logs` → `telemetry.logs` (service = project name/projectMap, severity from level,
  `trace_id` if present), `traces` (OTLP JSON) → existing trace path, `speed_insights` → RUM VITAL (LCP/INP/CLS/FCP/TTFB, view_name =
  route), `web_analytics` → RUM VIEW_START (path → view_name, session/anonymous id is hashed). For Vercel's verification request
  (`x-vercel-verify`) the response header is `x-vercel-verify: <config.verifyToken>`.
  Drain config keys: `drainSecretRef`, `verifyToken`, `speedInsightsUnit`, `projectMap` (key is project name **or** id).
  Responses: no secret → 401, wrong signature → 403, another project's integration → 403, unknown integration → 404. For a gzip body the signature
  is tried over both the decompressed and the raw bytes. Client IP, user agent, JA3/JA4 are not stored. Integration + secret are cached for 60 s.
- **Neon:** `GET /api/v2/projects`, `/projects/{id}/branches`, `/projects/{id}/endpoints` → each compute endpoint is a `databases`
  row (engine postgres, `instance_class` = "0.25–2 CU", `source` = 'Neon · aws-eu-central-1'); metrics: compute state
  (active/idle → not `trooper.db.ops`, `meta.state`), `trooper.db.cpu`/`memory` from Neon's consumption/metrics API
  (empty if missing), `disk` = logical size / plan limit. Query statistics via the collector's direct mode.
- **AWS / GCP metric mapping** is kept as a table in `services/integrations/README.md` (CloudWatch/Monitoring metric →
  `trooper.*`).
- OTLP sent by trooper-cloud has the extra resource attribute `trooper.integration.name`. Queue and aggregate lag (SQS, Pub/Sub,
  MSK group total) is written with `messaging.destination.partition.id = "0"` (`broker_lag_1m.partition` UInt32; `-1` is not used).
  Neon consumption counters are custom metrics `trooper.cloud.neon.*` (`compute_time_seconds`, `active_time_seconds`,
  `cpu_used_seconds`, `written_data_bytes`, `data_transfer_bytes`, `synthetic_storage_bytes`, `logical_size_bytes`,
  `compute_active`, `compute_cu_max`) → generic `metrics` table. The datapoint time is the provider's own minute.
- **trooper-cloud ingest auth:** if `TT_INTERNAL_TOKEN` is set, the internal credential (§2); otherwise/on 401 a per-project ingest key
  `TT_CLOUD_INGEST_KEYS=projectId:key,...` (backward compatible). Vercel `createDrain` needs a real ingest key (the drain URL is public).
- Known gaps: no Neon CPU/memory/connection metric; RDS memory from the instance-class table (unknown class →
  `config.instanceMemoryGb`); no `max_connections` and p95 from CloudWatch/GCP; MSK Serverless has only lag.

## 11. Dashboard data mode (ADR-013, ADR-028)

- `apps/web`: api mode only (no mock, ADR-028). `VITE_API_URL` (default `http://localhost:8080`), `VITE_INGEST_URL` (host of the DSN
  in SDK snippets, default `http://localhost:4318` = the default of the api's `TT_PUBLIC_INGEST_URL`); identity is the session cookie
  (OIDC / dev login). Plumbing in `src/data/remote.ts`: `api()`, `remote(path, fallback, map?)`, `hydrateList(arr, path, map?)`,
  `registerHydrator(fn)`, `invalidate(prefix)`, `mutate(method, path, body, invalidates[])`, `useData()`, `remoteStatus()`.
- Each `src/data/*.ts` file registers its own hydrator; fallbacks are empty/zero (not fake data). With no data the screen
  shows an empty state.
- **Onboarding:** if `GET /me` returns empty `orgs`, the UI goes to the "create org + project" screen → `POST /api/v1/orgs
  {name, project: {name}}` → 201 `{org: {id, name, role: "owner"}, project: {id, name}, ingestKey: {id, prefix, key}, dsn}`
  (key and dsn once; dsn = `TT_PUBLIC_INGEST_URL` (default `http://localhost:4318`) + key + project id). Every signed-in user
  can create an org (becomes owner of their own org); audit_log `org.create`. Project in an existing org: the existing `POST /projects`.
  Details: session only (api key → 403); names trimmed, 1–100 characters; `project.slug` optional (derived from the name if missing, the response
  also has `project.slug`); at most `TT_MAX_ORGS_PER_USER` (default 20) owned orgs per user → 409. For a user without an org
  `GET /me` → `orgs: []`, `currentProjectId: null`; `/me`, `/projects`, `/orgs*` ignore an invalid `X-Trooper-Project`
  (not 403).
- **Bootstrap:** `api bootstrap --org --project --owner [--ingest-key-out <file>] [--json]` (ADR-028), idempotent (advisory
  lock); `--json` prints a single object to stdout: `{orgId, projectId, dsn, created: {org, project, key}}`, logs to stderr, error → exit ≠ 0.
  Org = the org with the same name that the owner owns; ingest key name `bootstrap`. If the file exists and the key in it (bare key or DSN)
  is active, it is reused (same DSN); if the file is missing/stale, old `bootstrap` keys are revoked and a new one is written to the file
  (directory 0700, file 0600). Without `--ingest-key-out` the existing key is kept and `dsn` is returned empty.

## 12. Symbolication (`services/symbolicator`)

- Blob store: S3 compatible (dev: SeaweedFS S3 `http://localhost:9002`, bucket `tt-artifacts` (the service creates it if missing), `trooper` / `trooper-secret`, path-style; env `TT_S3_ENDPOINT`,
  `TT_S3_BUCKET`, `TT_S3_ACCESS_KEY`, `TT_S3_SECRET_KEY`). Key: `<project>/<kind>/<app>/<release>/<file>`; one
  `index.json` per release (no Postgres table). If two replicas upload to the same release concurrently, an index entry may be lost — re-uploading fixes it.
- `symbolicator` :8095, with an api key (`Authorization: Bearer`, key cache 60 s, 503 if Postgres is missing):
  - `POST /v1/artifacts` (multipart: `kind` = `sourcemap | dsym | r8 | proguard | dart | ndk`, `app`, `release` (= RUM
    `resource.version` / `service.version`), file part name `file` (may be repeated)). Limit: debug file 200 MB (including inside a zip),
    map/mapping 50 MB.
  - `GET /v1/artifacts?app=&release=`, `DELETE /v1/artifacts?app=&release=[&kind=]`.
  - `POST /v1/symbolicate` `{ app, release, platform, stack }` → `{ stack, frames: [{ fn, file, line, col, inApp }], symbolicated,
    missing: [debugId] }` (`missing` = debug ids with no upload). If nothing is found, the raw stack is returned (`symbolicated: false`). The result
    is cached in Redis for 24 h; the cache key includes the release's upload version (a new upload does not hide behind an old result).
- **Stack formats:**
  - JS: Chrome/V8 and Firefox/Safari. Original function names are exact when uploaded together with the minified `.js` map (CLI default).
    Not supported: `data:` URL maps embedded in the file.
  - Android Java/Kotlin: R8/ProGuard mapping (inline frames, `sourceFile`, `rewriteFrame`). R8 outlines are not supported.
  - Android NDK: tombstone lines `#NN pc <rel hex>  <path> (BuildId: <hex>)`; frames ≥ 1 are already adjusted by the SDK
    (−4 arm64 / −2 thumb / −1 x86), the symbolicator uses `pc` as is; looks up `kind=ndk` (unstripped `.so`) by build-id.
    `platform=android` + tombstone frames → native path; in a mixed stack only the native part is resolved.
  - iOS: Apple format, all thread sections + `Binary Images`; release `"<short> (<build>)"` (e.g. `1.4.0 (212)`), dSYM zip/bundle/raw,
    both slices of a fat file. Swift demangling if `swift-demangle` is on the host (not in the container image → stays mangled).
  - Flutter: `kind=dart` split-debug-info symbols (Android ELF, iOS Mach-O). Dart obfuscation maps are not supported.
- In error detail (`/errors/{fingerprint}`, RUM app errors) the api asks the symbolicator for the stack (`TT_SYMBOLICATOR_URL`, timeout 2 s);
  if unreachable, the raw stack is returned.
- CLI: `tools/trooper-cli` → `trooper-cli upload-sourcemaps|upload-dsym|upload-mapping|upload-flutter-symbols|upload-ndk-symbols|delete
  --app --release <path>` (`TROOPER_API_URL`, `TROOPER_API_KEY`). Exit codes: 0 ok, 1 server/network, 2 usage, 3 unauthorized, 4 some
  files rejected. The Android Gradle plugin (`ai.tasktrooper.mapping-upload`) uploads only `r8`; for NDK `.so` use CLI
  `upload-ndk-symbols`. iOS: `sdks/ios/Scripts/upload-dsyms.sh`.

## 13. Code intelligence (`services/codeintel`, ADR-014)

- Port :8096 (`/healthz`, `/metrics`, `POST /v1/webhooks/github`, `POST /v1/webhooks/gitlab`, `POST /v1/webhooks/bitbucket`).
  Providers: `github` (App / token / public), `gitlab` (gitlab.com + self-managed `base_url`), `bitbucket` (Cloud),
  `local` (dev) — §18a. Migration `deploy/postgres/005_codeintel.sql` (owned by codeintel):
  - `repositories` + `installation_id bigint`, `credentials_ref text`, `local_path text` (provider `local` added),
    `analyze_requested_at timestamptz`, `analyzed_at timestamptz`, `error text`, `languages text[]`, `services text[]`,
    `webhook_secret_ref text`.
  - `inventory_items` + `service text`, `criticality text` (critical|high|medium|low), `observed bool`, `monitored bool`,
    `code_hash text`, `summary text`, `dead bool` (in code, no traffic for 30 days), `shadow bool` (has traffic, not in code →
    `repository_id` may be NULL), `llm jsonb` (failure_modes, loadtest_hint).
  - `alert_suggestions` + `title text`, `kind text`, `target text`, `condition text`, `criticality text`, `code_ref text`,
    `calibrated bool` (threshold tuned with observed data), `created_at`. `monitor_draft` = same shape as the api `POST /monitors` body.
  - `monitors` + `source text` ('manual'|'code'|'template'|'suggestion'), `code_ref text` (alert-as-code; read-only).
  - `loadtest_scenarios` + `state text` ('draft'|'ready', default 'ready'); codeintel writes `source='generated', state='draft'`.
- `.trooper/config.yaml`: `services: [{ path: services/orders, name: orders-api }]`; if missing, detected from SDK init calls.
- `.trooper/monitors/*.yaml`: alerting.md § Rule schema format (name, kind, query, condition, severity, notify, labels).
- Inventory `key` format: endpoint `"METHOD /route/:param"` (same as writer normalization), consumer = process name (`source` =
  topic/queue), cron = job name (`source` = schedule), view = route, dependency = `"<kind> · <name>"`.
- Observation matching: against `red_1m` (last 7 days) routes and `consumer_1m` processes; `observed`, `dead`, `shadow`.

## 14. Load testing (`services/loadtest`, ADR-015)

- `loadtest-ctl` :8097 (`/healthz`, `/metrics`). Migration `deploy/postgres/006_loadtest.sql` (owned by loadtest):
  - `loadtest_runs` + `region text`, `live jsonb` (`{ ts, vus, rps, p95, errorRate, progress }`), `abort_requested_at`,
    `error text`, `created_at`, `requested_by bigint`.
  - `loadtest_domains (id, project_id, host, token, verified_at, method 'dns'|'http', created_at, UNIQUE(project_id, host))`.
  - `loadtest_usage (project_id, month date, vu_minutes numeric, PRIMARY KEY(project_id, month))`.
- Scenario `spec` (jsonb) = the UI `ScenarioEditor` model: `{ target, mode: 'vus'|'rps', levels: [{durationS, target}], steps: [key],
  weights: {key: pct}, requests: {key: {method, path, headers?, json?, checks?}}, thinkTime, regions, gate: {...},
  thresholds: {p95Ms, errorRate}, script? }`. Templates: `{{int a-b}}`, `{{pool.<name>}}`, `{{weighted a:70 b:30}}`,
  `{{prev "KEY" .field}}`, `{{uuid}}`.
- Runner metrics (OTLP, `service.name=loadtest`, attr `trooper.loadtest.run`, `trooper.loadtest.step`):
  `trooper.loadtest.requests` (sum), `trooper.loadtest.errors` (sum), `trooper.loadtest.duration_ms` (histogram),
  `trooper.loadtest.vus` (gauge) → generic `metrics` table.
- Request headers: `traceparent`, `baggage: tt.svc=loadtest:<run_id>`, `x-trooper-loadtest-run`, `user-agent: TaskTrooper-LoadTest/1`.

## 15. Identity, org and admin API (ADR-016)

- Migration `deploy/postgres/007_auth.sql` (owned by api): `sessions (id text PK, user_id, created_at, expires_at, last_seen_at,
  user_agent)`, `org_members.role` ∈ owner|admin|member|viewer, `audit_log (id, org_id, project_id, user_id, api_key_id,
  action, target, meta jsonb, at)`, `users.oidc_subject`, `api_keys.created_by`, `api_keys.last_used_at`.
- Endpoints (`/api/v1`):
  - `GET /auth/config` (anonymous) → `{ oidc: bool, devLogin: bool }`; `GET /auth/login?redirect=` → redirects to OIDC;
    `GET /auth/callback`; `POST /auth/dev-login {email}`; `POST /auth/logout`; `GET /me` → `{ user, orgs: [{id, name, role,
    projects: [{id, name}]}], currentProjectId }`.
  - `GET/POST /orgs/{org}/members`, `PATCH/DELETE /orgs/{org}/members/{user}` (role), `GET/POST /projects`,
    `GET/POST /api-keys` (the POST response returns the key **once**), `DELETE /api-keys/{id}`, `GET /audit-log?limit=`.
- CORS: for requests with cookies, `Access-Control-Allow-Credentials: true` and the exact origin (not a wildcard).

## 16. Phase 4–5 api endpoints and other open items

- Code intelligence (api reads/writes Postgres):
  - `GET /repos` → UI `Repo[]`; `POST /repos {provider, fullName, defaultBranch?, token? | installationId? | localPath?, baseUrl?}`
    (`baseUrl` only for gitlab; for github/gitlab/bitbucket without an App the response has a one-time `webhook {url, secret}`, §18a);
    `DELETE /repos/{id}`; `POST /repos/{id}/analyze` (→ `analyze_requested_at`, `pending_paths='{*}'`; codeintel
    runs incrementally with the `last_analyzed_sha..HEAD` git diff, otherwise a full scan — code-intelligence.md § Incremental analysis); `POST /repos/{id}/webhook-secret` (new secret).
  - `GET /inventory?repoId=&kind=&service=` → UI `InventoryItem[]` (+ `dead`, `shadow`, `summary`).
  - `GET /suggestions?state=proposed` → UI `Suggestion[]`; `POST /suggestions/{id}/accept` (creates a monitor, `source=suggestion`)
    ; `POST /suggestions/{id}/reject`.
  - `PUT/PATCH/DELETE /monitors/{id}` returns 409 "managed in repository" if `source='code'`.
- Load test (api): existing endpoints + `GET /loadtests/runs/{id}/series?step=5s` (`{ ts[], vus[], rps[], p95[], errorRate[] }`,
  from the `metrics` table) and `GET /loadtests/runs/{id}/target?service=` (the target service's RED in the same window, split by caller
  `loadtest:<id>`); `GET/POST /loadtests/domains`, `POST /loadtests/domains/{id}/verify`, `DELETE /loadtests/domains/{id}`.
- Range: `15m`, `30m` are added (`timerange` + UI `TimeRange`).
- Logs: `GET /logs?source=` → `attrs['vercel.source']` filter (build|static|external|edge|lambda|redirect|firewall).
- Ingest internal credential: `x-trooper-internal: <TT_INTERNAL_TOKEN>` + `x-trooper-project: <id>` headers; if the
  token matches, ingest-gw does not look up a key. trooper-cloud and loadtest-runner use it (`TT_CLOUD_INGEST_KEYS` stays backward compatible).
- KPI rules: `kpi_alert_rules.condition jsonb` (`renotifyMin`) — migration `008_alerting_r3.sql` (owned by alerting).

## 17. Slack App (ADR-017)

- Env (api + notifier): `TT_SLACK_CLIENT_ID`, `TT_SLACK_CLIENT_SECRET`, `TT_SLACK_SIGNING_SECRET`, `TT_PUBLIC_API_URL`
  (base for OAuth redirect + interactivity URL), `TT_PUBLIC_APP_URL` (links in messages).
- Postgres (`009_slack_github.sql`, owned by api): `integrations.provider` += `slack`, `github`; the Slack installation is
  `integrations(provider='slack', config={teamId, teamName, botUserId}, credentials_ref=secret:<bot token>)`, one per
  org (project_id = the project that did the install; reads are org-wide). `notification_messages (id, incident_id, channel, provider,
  external_id /*slack ts*/, external_channel, created_at)`.
- api endpoints: `GET /integrations/slack/install` → redirects to Slack OAuth (signed state); `GET /integrations/slack/callback`;
  `GET /integrations/slack/channels?q=` → `[{id, name, isPrivate, isMember}]` (cache 5 min); `DELETE /integrations/slack`;
  `POST /integrations/slack/test {channel}` (admin+; test message to the channel, error 502 + readable message: bot not invited, token revoked etc.);
  `POST /slack/interactions` (Ack / Mute 1h buttons; signature verification; maps the Slack user to `users` by email).
- Notification kinds: `firing | resolved | reminder | acknowledged`; the ack notification is enqueued by **alert-engine** (not api;
  dedupe `incident:<id>:acknowledged:<channel>`). Button action ids: `tt_ack:<incidentId>`, `tt_mute1h:<monitorId>`.
  `not_in_channel` → `conversations.join` (scope `channels:join`) + one retry; private channel → permanent "invite the app"
  error. `TT_SLACK_API_URL` for tests. Alert-engine multi-shard: stream rule hits are handed over to the monitor's owning shard via `alert_stream_hits` (unlogged) +
  `NOTIFY tt_stream_hit`.
- Notification target format: `slack:<channel_id>#<channel_name>` (e.g. `slack:C0123ABCD#payments-alerts`). notifier: `chat.postMessage`
  with the bot token; the first message's `ts` goes to `notification_messages`; `resolved`/`reminder`/`acknowledged` → reply in the thread with
  `thread_ts` + `chat.update` on the first message (color/state). `slack:https://hooks.slack.com/...` webhook targets work as before.
- Message Block Kit: header (state emoji + title), fields (service, value vs threshold, duration, severity), **"Likely cause"** section
  (`incidents.root_cause` → `summary` + `round(confidence*100)%`; `< 30%` → "low confidence"), deploy context, buttons
  (Open incident, Ack, Mute 1h).

## 18. GitHub App (ADR-017)

- Env (api + codeintel): `TT_GITHUB_APP_ID`, `TT_GITHUB_APP_SLUG`, `TT_GITHUB_APP_PRIVATE_KEY` (env:/file: ref),
  `TT_GITHUB_WEBHOOK_SECRET`. Shared client `services/pkg/githubapp` (JWT, installation token cache, repo list, pagination).
- Installation: `integrations(provider='github', config={installationId, account, accountType}, project_id)`; `repositories.installation_id`.
- api endpoints: `GET /integrations/github/install` → `https://github.com/apps/<slug>/installations/new?state=<signed>`;
  `GET /integrations/github/callback?installation_id&setup_action&state` → integration record, returns to the UI with `/settings/integrations?github=<id>`;
  `GET /integrations/github/{id}/repos?q=` → `[{fullName, defaultBranch, private, language, added}]`;
  `POST /repos {provider:'github', fullName, installationId}` (existing endpoint; no token needed).
- Webhook (codeintel `POST /v1/webhooks/github`, the App webhook URL): `push` to default branch → `repositories.analyze_requested_at
  = now()` + changed file list (`pending_paths text[]`) → incremental analysis; `installation_repositories` removed → repo
  `status='disconnected'`; `installation` deleted → integration `status='error'`. Idempotent by delivery id (`X-GitHub-Delivery`).

## 18a. GitLab and Bitbucket sources

- Migration `deploy/postgres/013_vcs_sources.sql` (owned by codeintel): `repositories.base_url text` (only `provider='gitlab'`,
  self-managed; NULL → `https://gitlab.com`; CHECK `base_url IS NULL OR provider = 'gitlab'`), `repositories.webhook_received_at
  timestamptz` (last verified webhook delivery; all providers).
- Shared rules in `services/pkg/vcs`: `ValidFullName(provider, name)` (github `owner/name`, gitlab `group/…/project` 2–20
  segments, bitbucket exactly `workspace/repo`; segment `[A-Za-z0-9_][A-Za-z0-9_.-]*`, cannot end with `.git`/`.atom`),
  `NormalizeBaseURL` (https, host required, no userinfo/query/fragment, trailing `/` dropped), `CloneURL`, `AuthHeader`
  (gitlab `Basic oauth2:<token>`, bitbucket `Basic x-token-auth:<token>`; a token in `user:password` form becomes Basic
  as is on both providers).
- api: `POST /repos` for gitlab/bitbucket takes `fullName`, `defaultBranch?`, `token?` (public if missing), `baseUrl?` (gitlab).
  For github without an App, gitlab and bitbucket repos a 32-byte random secret (URL-safe base64) is sealed into `secrets`
  (`webhook_secret_ref`) and the response carries **once** `webhook: {url: <TT_PUBLIC_WEBHOOK_URL>/v1/webhooks/<provider>, secret}`.
  `POST /repos/{id}/webhook-secret` rotates the secret (the old ref is deleted) and returns the same shape (installation repos → 400).
  `Repo` + `baseUrl?`, `url` (browser link), `webhookActive` = has an installation **or** `webhook_received_at` is set.
- codeintel webhooks (setup: GitLab → Settings → Webhooks, "Push events", Secret token = secret; Bitbucket →
  Repository settings → Webhooks, "Repository push", Secret = secret):
  - `POST /v1/webhooks/gitlab`: `X-Gitlab-Token` constant-time comparison, `X-Gitlab-Event: Push Hook`, idempotency
    `Idempotency-Key` → `X-Gitlab-Event-UUID`; files from `commits[]`, `total_commits_count > len(commits)` → `{*}`.
  - `POST /v1/webhooks/bitbucket`: `X-Hub-Signature: sha256=<hex>` (HMAC-SHA256), `X-Event-Key: repo:push`, idempotency
    `X-Request-UUID`; the payload does not list files → `{*}` (incremental diff from `last_analyzed_sha`).
  - Responses are the same as GitHub: 202 "queued", 200 "ignored" / "duplicate" / "pong", 401 signature/token, 400 JSON, 413 > 25 MB.
    Delivery ids are written to `codeintel_webhook_deliveries` as `gitlab:<webhook uuid>:<id>` / `bitbucket:<hook uuid>:<id>`.
- Not supported: Bitbucket Server / Data Center, GitLab/Bitbucket OAuth app flow and repo picker (the token is entered manually),
  automatic webhook setup via the provider API.

## 19. AI usage quota (ADR-018)

**Postgres `deploy/postgres/010_ai_quota.sql`** (owner: the aiquota owner; idempotent):
```sql
ALTER TABLE orgs        ADD COLUMN IF NOT EXISTS ai_monthly_credits integer;          -- NULL = plan default, -1 = unlimited
ALTER TABLE orgs        ADD COLUMN IF NOT EXISTS ai_notify text[] NOT NULL DEFAULT '{}'; -- monitor channels format (slack:<id>#name, email:..)
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS ai_monthly_credits integer;          -- NULL = no member cap
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS analyze_requested_by bigint REFERENCES users(id) ON DELETE SET NULL;
CREATE TABLE IF NOT EXISTS ai_usage (
  id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id bigint REFERENCES projects(id) ON DELETE SET NULL,
  user_id bigint REFERENCES users(id) ON DELETE SET NULL,      -- NULL = automatic job
  feature text NOT NULL,                                       -- codeintel.enrich | codeintel.suggest | rca | assistant | other
  model text NOT NULL, input_tokens integer NOT NULL, output_tokens integer NOT NULL,
  credits numeric(12,4) NOT NULL, ref text NOT NULL DEFAULT '',  -- e.g. 'repo:12'
  created_at timestamptz NOT NULL DEFAULT now());
-- index: (org_id, created_at), (org_id, user_id, created_at)
CREATE TABLE IF NOT EXISTS ai_quota_alerts (org_id bigint, period date, threshold smallint CHECK (threshold IN (80,100)),
  sent_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (org_id, period, threshold));
-- notifications.kind CHECK: ('firing','resolved','reminder','acknowledged','ai_quota'); incident_id can already be NULL
```

**Credit calculation** (`services/pkg/aiquota`, no external dependencies):
- `credits = (inTok × inPrice + outTok × outPrice) / 1e6 / 0.01` — prices in USD / million tokens.
- `TT_AI_PRICES="<model>=<in>/<out>,..."`, unknown model → `TT_AI_DEFAULT_PRICE` (default `3/15`).
- `TT_AI_PLAN_CREDITS="free=100,team=5000,enterprise=-1"` (this is the default). Unknown plan → `free`.
- Effective org limit: `orgs.ai_monthly_credits` ?? plan default; `-1` unlimited. Member limit: `memberships.ai_monthly_credits`.
- `(*Config).Reserve(ctx, tx, Request{OrgID, ProjectID, UserID *int64, Feature, EstimateCredits})` → `Decision{Allowed, Reason
  ("org_limit"|"user_limit"), OrgUsed, OrgLimit, UserUsed, UserLimit}`. Inside a transaction
  `pg_advisory_xact_lock(7140, org_id)`; usage = this month's `ai_usage` SUM. `Record(ctx, db, Usage{...})` writes with real tokens.
  Postgres access goes through a small interface (`Exec`, `QueryRow(...).Scan`); the caller writes the pgx adapter.
- Estimate: `ceil(promptChars/4) × in + maxTokens × out`.

**codeintel:** `Reserve` before each LLM batch; on rejection the remaining items go without LLM (static), and the analysis summary is written to
`repositories.analysis` (jsonb, 010): `{sha, incremental, items, llmItems, llm: {status: "ok"|"disabled"|
"quota_exceeded", reason?: "org_limit"|"user_limit"|"check_failed", credits, skipped?}}` (api returns it as `inventory.analysis.llm`;
`check_failed` = the quota check failed, LLM is skipped fail-closed).
`UserID` = `repositories.analyze_requested_by` (api fills it on manual analysis; codeintel NULLs it on webhook/automatic requests).

**Threshold notification:** alert-engine every 5 min: if org usage ≥ 80% / ≥ 100% and `ai_quota_alerts` has no row for that month+threshold,
a notification is queued to the `orgs.ai_notify` targets (notification kind `ai_quota`, no incident; payload `{orgId, period, threshold,
used, limit}`). Notifier Slack/email text: "AI usage at 80% of this month's credits (4,012 / 5,000). Resets Nov 1."

**API** (owned by api):
- `GET /usage/ai?month=YYYY-MM` (member+, default this month) →
  `{ period, resetsAt, plan, org: {limit: number|null, used, remaining: number|null}, me: {limit: number|null, used},
     byUser: [{userId, email, name, limit: number|null, used}] (admin+ only; [] for others),
     byFeature: [{feature, credits, calls}], byProject: [{projectId, name, credits}],
     daily: SeriesSet (id 'credits'), aiQuota?: {monthlyCredits: number|null, notify: string[]} (admin+ only) }` —
  `limit: null` = unlimited. Credits with 2 decimals.
- `PUT /orgs/{org}/ai-quota {monthlyCredits: number|null, notify: string[]}` (admin+, audit `ai_quota.update`).
- `PUT /orgs/{org}/members/{user}/ai-quota {monthlyCredits: number|null}` (admin+, audit `ai_quota.member`).
- `GET /me` → extra field `ai: {period, limit, used, userLimit, userUsed}`; `GET /orgs/{org}/members` rows have `aiCredits: {limit, used}`.
- `POST /repos/{id}/analyze` → `analyze_requested_by` = session user (NULL with an api key).

## 20. Host, container, process and Kubernetes (ADR-019)

**trooper-agent modes:** `TT_AGENT_MODES` (comma-separated; default `db`) — `db`, `host`, `cluster`. `TT_K8S_CLUSTER` (cluster name;
also set in host mode when inside K8s), `NODE_NAME`/`NODE_IP` (downward API), `TT_AGENT_DOCKER_SOCKET`
(default `/var/run/docker.sock`; if missing, containers are not collected), `TT_AGENT_PROCFS` (`/host/proc`), `TT_AGENT_TOP_PROCESSES` (20).
Interval: host 15 s, cluster 30 s. Sending is OTLP metrics as today, `TT_DSN` ingest key.

Shared resource attrs: `service.name=trooper-agent`, `host.name`, `host.id` (machine-id), `os.type`, `host.arch`,
`k8s.cluster.name` (if any), `k8s.node.name` (if any), `cloud.provider`/`cloud.region` (if any).

| Metric (gauge) | Unit | Attrs | Table.column |
|---|---|---|---|
| `trooper.host.cpu` | 0..1 | — | `hosts_1m.cpu` |
| `trooper.host.memory` / `.memory_bytes` | 0..1 / byte | | `memory` / `memory_bytes` |
| `trooper.host.disk` | 0..1 (fullest fs) | | `disk` |
| `trooper.host.disk_read` / `.disk_write` | byte/s | | `disk_read` / `disk_write` |
| `trooper.host.net_in` / `.net_out` | byte/s | | `net_in` / `net_out` |
| `trooper.host.load1`, `trooper.host.uptime_s`, `trooper.host.cpu_cores` | | | `load1`, `uptime_s`, `cpu_cores` |
| `trooper.container.cpu` / `.cpu_limit` | core | `container.id`, `container.name`, `container.image.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, `trooper.k8s.workload.kind`, `trooper.k8s.workload.name`, `trooper.service` | `containers_1m.cpu` / `cpu_limit` |
| `trooper.container.memory_bytes` / `.memory_limit_bytes` | byte | same | `memory_bytes` / `memory_limit` |
| `trooper.container.restarts` | cumulative counter | same | `restarts` |
| `trooper.container.net_in` / `.net_out` | byte/s | same | `net_in` / `net_out` |
| `trooper.container.throttled` | 0..1 | same | `throttled` |
| `trooper.process.cpu` (core), `.memory_bytes`, `.threads`, `.fds` | | `process.pid`, `process.executable.name`, `process.command` (first token only, ≤ 128 characters), `container.id` | `processes_1m` |
| `trooper.k8s.node.ready` (0/1), `.allocatable_cpu` (core), `.allocatable_memory` (byte), `.pods` | | `k8s.node.name` | `k8s_nodes_1m` |
| `trooper.k8s.pod.ready` (0/1), `.restarts`, `.cpu_request`, `.cpu_limit`, `.memory_request`, `.memory_limit` | | `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, `k8s.node.name`, `k8s.pod.phase`, `trooper.k8s.pod.reason`, `trooper.k8s.workload.kind`, `trooper.k8s.workload.name`, `trooper.service` | `k8s_pods_1m` |
| `trooper.k8s.workload.desired` / `.ready` / `.available` / `.updated` | | `k8s.namespace.name`, `trooper.k8s.workload.kind` (Deployment\|StatefulSet\|DaemonSet\|Job\|CronJob), `trooper.k8s.workload.name` | `k8s_workloads_1m` |

- `trooper.k8s.pod.reason`: container waiting/terminated reason (`CrashLoopBackOff`, `OOMKilled`, `ImagePullBackOff`,
  `ErrImagePull`, `CreateContainerConfigError`, `Evicted`, `Error`), otherwise empty. Workload = ReplicaSet → resolved to the Deployment.
- `trooper.service` = pod label `tasktrooper.io/service` > `app.kubernetes.io/name` > `app`; for containers outside K8s the label
  `tasktrooper.io/service` or the container name.
- **Events** (cluster mode): only `type=Warning` → OTLP log, resource `service.name=k8s`, `k8s.cluster.name`; log attrs
  `k8s.event.reason`, `k8s.event.type`, `k8s.object.kind`, `k8s.object.name`, `k8s.namespace.name`, `k8s.event.count`;
  body = message; severity `warn`. The same event (uid+count) only once.
- SDK/service side: in K8s, if the app sets `OTEL_RESOURCE_ATTRIBUTES=k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE),k8s.node.name=$(NODE_NAME)`,
  spans are linked to the pod (example in sdk.md).
- **eBPF (OBI):** Helm `ebpf.enabled=true` → OBI DaemonSet; `OTEL_EXPORTER_OTLP_ENDPOINT=<ingest-gw>`, header `x-trooper-key`.
  The `telemetry.sdk.name` value of OBI spans is `opentelemetry-ebpf-instrumentation` (or `beyla`); the api service list returns
  `source: 'ebpf'`.

**trooper-agent implementation notes:** `process.pid` and `k8s.event.count` are OTLP int. In K8s the pod network is shared, so `net_in/out`
is only on the pod's first container. `trooper.k8s.pod.reason` keeps the last `lastState.terminated` reason for 15 min (CrashLoopBackOff after OOM
→ `OOMKilled`). Job: desired = completions, available = updated = succeeded (a running Job does not count as `unavailable`
— alerting and api exclude Jobs); CronJob: desired 1 (0 if suspended), available 0 if the last run failed. Extra env:
`TT_KUBELET_INSECURE`, `TT_KUBELET_URL`, `TT_K8S_API_URL`, `TT_K8S_TOKEN`, `TT_K8S_INSECURE`, `TT_AGENT_ROOTFS`, `TT_AGENT_SYSFS`,
`TT_AGENT_HOSTNAME`, `TT_CLOUD_PROVIDER`, `TT_CLOUD_REGION`.

**ClickHouse `deploy/clickhouse/003_infra_k8s_faas.sql`** (AggregatingMergeTree, gauges `SimpleAggregateFunction(max, Float64)`,
string state columns `SimpleAggregateFunction(anyLast, String)`; TTL 30 days for 1m tables, 7 days for processes; unreported value
per the §5 rule: max column `-Inf`):
- `hosts_1m (org_id, project_id, cluster, host, host_id, os, arch, node, minute | cpu, cpu_cores, memory, memory_bytes, disk,
  disk_read, disk_write, net_in, net_out, load1, uptime_s)` ORDER BY (org_id, project_id, host, minute).
- `containers_1m (org_id, project_id, cluster, host, namespace, pod, pod_uid, container, container_id, image, workload_kind,
  workload, service, minute | cpu, cpu_limit, memory_bytes, memory_limit, restarts, net_in, net_out, throttled)`
  ORDER BY (org_id, project_id, cluster, namespace, pod, container, minute).
- `processes_1m (org_id, project_id, host, container_id, pid UInt32, exe, command, minute | cpu, memory_bytes, threads, fds)`.
- `k8s_nodes_1m (org_id, project_id, cluster, node, minute | ready, allocatable_cpu, allocatable_memory, pods)`.
- `k8s_pods_1m (org_id, project_id, cluster, namespace, pod, pod_uid, node, workload_kind, workload, service, minute |
  phase anyLast, reason anyLast, ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit)`.
- `k8s_workloads_1m (org_id, project_id, cluster, namespace, kind, name, minute | desired, ready, available, updated)`.
Empty `cluster` = outside K8s. The writer routes the `trooper.host.*`, `trooper.container.*`, `trooper.process.*`, `trooper.k8s.*`, `trooper.faas.*`
prefixes to these tables.

**Monitor kind `infra`** — `query: {entity: 'host'|'node'|'cluster'|'container'|'pod'|'workload'|'database', metric, filter: {cluster?,
namespace?, host?, workload?, service?}, agg: 'max'|'avg'}` (entity `database`: `filter: {database?, engine?}`); metrics: host
`cpu|memory|disk|load1`, node `cpu|memory|disk` (from `hosts_1m` rows with `node` set; `filter.host` = node name) `|not_ready`
(count, `k8s_nodes_1m`), cluster `cpu|memory|disk` (per minute the **mean** over the cluster's nodes), container/pod `cpu_ratio`
(cpu/limit) `|memory_ratio|restarts` (increase within the window) `|throttled`, pod `not_ready` (count of pods with ready=0), workload
`unavailable` (desired − available), database (`db_1m`, one entity per `database_id` + `node`) `cpu|memory|disk|connections_ratio`
(connections / max_connections) `|p95_ms|repl_lag_ms|hit_ratio|evictions|slow`. Ratios are 0..1. Value = the worst value among
matching entities; incident `detail.entities` holds the worst 5 entities. Templates: pod CrashLoop (`restarts` > 3 / 10 min), node
NotReady (`not_ready` > 0, 5 min), host CPU > 0.9 (10 min), host memory > 0.9 (10 min), host disk > 0.9, node disk > 0.85, cluster disk
> 0.8 (15 min), database memory > 0.9 (10 min), database disk > 0.85, database connections > 0.8 of max, workload `unavailable` > 0
(10 min).
**RCA:** on a service incident, `reason ∈ {OOMKilled, CrashLoopBackOff}` or a restart increase within the window on pods of the same `service`
→ candidate "Pod <pod> restarted (OOMKilled)" (confidence 0.6–0.85); cpu > 0.9 on the pod's host → "Host <h> CPU saturated".

**API** (`?range=` everywhere; values are the last full minute of the selected range, series are range buckets):
- `GET /infra/hosts?cluster=&q=` → `[{id (host.name), hostId, os, arch, cluster, node, cpu, cpuCores, memory, memoryBytes,
  disk, netIn, netOut, load1, uptimeS, containers, lastSeen, status, spark: {cpu: number[], memory: number[]}}]`.
- `GET /infra/hosts/{host}` → host fields (current values, same as the list) + `series: {cpu, memory, disk (SeriesSet, 0..1),
  network (SeriesSet: `in`, `out` byte/s)}`, `containerList: Container[]`, `processes: [{pid, exe, command, containerId, cpu, memoryBytes, threads, fds}]` (last
  minute, by cpu), `services: string[]`.
- `GET /infra/containers?host=&service=` → `Container[] = [{id, name, image, host, cluster, namespace, pod, podUid, service,
  cpu, cpuLimit, memoryBytes, memoryLimit, restarts, netIn, netOut, throttled, status}]`.
- `GET /infra/k8s/clusters` → `[{name, nodes, nodesReady, pods, podsRunning, podsFailing, namespaces, workloads,
  cpuUsed, cpuAllocatable, memoryUsed, memoryAllocatable, warningEvents, lastSeen, status}]`.
- `GET /infra/k8s/clusters/{cluster}` → cluster fields (counts, same as the list) + `nodeList: [{name, ready, status, cpu,
  cpuAllocatable, memoryBytes, memoryAllocatable, pods, host}]`, `namespaceList: [{name, pods, workloads, failing}]`,
  `series: {cpu (SeriesSet core: `used`, `requested`, `allocatable`), memory (SeriesSet byte, same ids)}`.
- `GET /infra/k8s/workloads?cluster=&namespace=` → `[{cluster, namespace, kind, name, desired, ready, available, updated,
  restarts (increase in range), cpu, memoryBytes, service, status}]`.
- `GET /infra/k8s/pods?cluster=&namespace=&workload=&node=&service=` → `Pod[] = [{uid, name, namespace, cluster, node, phase,
  reason, ready, restarts, restartsInRange, cpu, cpuRequest, cpuLimit, memoryBytes, memoryRequest, memoryLimit, workloadKind,
  workload, service, status, containers: [{name, image, cpu, memoryBytes, restarts}]}]`.
- `GET /infra/k8s/pods/{uid}` → Pod (current values) + `series: {cpu (SeriesSet core: `usage`, `request`, `limit`), memory
  (byte, same ids), restarts (SeriesSet, restart increase per bucket)}`, `events: K8sEvent[]`.
- `GET /infra/k8s/events?cluster=&namespace=&kind=&name=&limit=` → `K8sEvent[] = [{ts, type, reason, message, objectKind,
  objectName, namespace, cluster, count}]` (logs table, `service='k8s'`).
- `GET /services/{svc}/infra` → `{pods: Pod[], hosts: [{id, cpu, memory}], containers: Container[], functions:
  [{id, name, provider}]}`.
- Status rules (`status`: `ok|warning|critical|stale`): host/node `critical` if cpu|memory|disk > 0.95 or NotReady,
  `warning` > 0.85, `stale` if last data > 5 min. Pod `critical`: phase Failed, reason ∈ {CrashLoopBackOff, OOMKilled,
  ImagePullBackOff, ErrImagePull} or Running+ready=0; `warning`: restartsInRange > 0 or Pending. Workload `critical`
  available=0 & desired>0, `warning` available < desired. Cluster: the worst node/pod status.
  Rule: detail endpoints carry the list row's fields **exactly**; series go under `series`, lists are named `*List` (no field name
  collisions).

## 21. Serverless (ADR-020)

**Postgres `deploy/postgres/011_serverless.sql`** (owner: integrations):
```sql
-- 'azure' is added to the integrations.provider CHECK; monitors.kind is free text (not touched if there is no check)
CREATE TABLE IF NOT EXISTS serverless_functions (
  id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  integration_id bigint REFERENCES integrations(id) ON DELETE SET NULL,
  provider text NOT NULL CHECK (provider IN ('aws','azure')),
  slug text NOT NULL,                 -- aws-<region>-<name> | azure-<app>
  name text NOT NULL, app text,       -- Azure: function app name (name = app)
  region text NOT NULL DEFAULT '', runtime text NOT NULL DEFAULT '', memory_mb integer, timeout_s integer,
  arch text NOT NULL DEFAULT '', triggers text[] NOT NULL DEFAULT '{}',   -- 'api', 'sqs:orders', 'schedule', 'http', 'kinesis:..'
  service text NOT NULL,              -- span service.name mapping (default name; tag/app setting 'tasktrooper.service')
  url text, meta jsonb NOT NULL DEFAULT '{}',   -- arn, plan (Consumption|Flex|Premium), functions[] (Azure), tags
  last_seen_at timestamptz NOT NULL DEFAULT now(), missing boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (project_id, slug));
```

**trooper-cloud:**
- AWS: `lambda` in `config.services` (on by default for new integrations). IAM: `lambda:ListFunctions`, `lambda:ListTags`,
  `lambda:ListEventSourceMappings`, `lambda:GetFunctionConcurrency`, `cloudwatch:GetMetricData`.
- Azure: `provider='azure'`, `config: {tenantId, clientId, subscriptionIds: [], resourceGroups?: [], env?}`,
  `credentials_ref` → client secret (plain sealed secret). Token: `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token`
  (client_credentials, scope `https://management.azure.com/.default`); `TT_AZURE_LOGIN_URL`, `TT_AZURE_ARM_URL` override
  for tests. Discovery `Microsoft.Web/sites` (kind `functionapp`), functions `{site}/functions`; metrics
  `{site}/providers/microsoft.insights/metrics` PT1M. Role: Reader + Monitoring Reader.
- OTLP metrics (gauge, provider minute; resource `service.name=trooper-cloud`, `cloud.provider`, `cloud.region`;
  datapoint attrs `trooper.faas.id` = slug, `faas.name`):

| Metric | AWS source | Azure source | `faas_1m` column |
|---|---|---|---|
| `trooper.faas.invocations` | Invocations Sum | FunctionExecutionCount Total | `invocations` |
| `trooper.faas.errors` | Errors Sum | Http5xx Total | `errors` |
| `trooper.faas.throttles` | Throttles Sum | — | `throttles` |
| `trooper.faas.duration.avg` / `.max` / `.p95` / `.p99` (ms) | Duration Average/Maximum/p95/p99 | HttpResponseTime Average/Maximum (s→ms) | `duration_avg` / `duration_max` / `duration_p95` / `duration_p99` |
| `trooper.faas.concurrency` | ConcurrentExecutions Maximum | — | `concurrency` |
| `trooper.faas.iterator_age_ms` | IteratorAge Maximum | — | `iterator_age_ms` |
| `trooper.faas.memory_bytes` | — | MemoryWorkingSet Average | `memory_bytes` |
| `trooper.faas.execution_units` (MB·ms) | — | FunctionExecutionUnits Total | `execution_units` |

**ClickHouse** `faas_1m (org_id, project_id, function_id, provider, region, minute | invocations, errors, throttles, duration_avg,
duration_max, duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units)` — in the §20 file,
`ORDER BY (org_id, project_id, function_id, minute)`. Minute values are gauge `max`; range total = `sum` of the minutes.

**Trace / cold start:** function spans carry `faas.name`, `faas.coldstart` (bool), `faas.trigger`, `cloud.provider`
(OTel semconv). `@tasktrooper/node` `wrapLambda(handler, opts?)`: sets `faas.coldstart=true` on the first call, `faas.invocation_id`,
`faas.trigger`, and flushes before returning. The api counts cold starts from `spans` (`attrs['faas.coldstart']='true'`,
`service` = the function's `service`, `is_entry=1`).

**Cost estimate** (api, USD; override via env): Lambda `invocations × 0.20/1e6 + GB-s × 0.0000166667` (arm64 `0.0000133334`),
GB-s = Σ(invocations × duration_avg ms / 1000) × memory_mb / 1024. Azure Consumption `execution_units / 1024 / 1000 × 0.000016
+ invocations × 0.20/1e6`. Free tier is not deducted.

**Monitor kind `faas`** — `query: {functionId? | filter: {provider?, region?, service?}, metric: 'invocations'|'errors'|
'error_rate'|'throttles'|'duration_p95'|'duration_p99'|'cold_starts'|'concurrency'|'iterator_age_ms'}`. Templates: error rate
> 0.05 (5 min), throttles > 0 (5 min), p95 > timeout × 0.8.

**API:**
- `GET /serverless/summary` → `{functions, invocations, errors, errorRate, throttles, coldStarts, costUsd,
  byProvider: [{provider, functions, invocations, costUsd}]}`.
- `GET /serverless/functions?provider=&q=` → `[{id, slug, name, provider, app, region, runtime, memoryMb, timeoutS, arch,
  triggers, service, invocations, errors, errorRate, throttles, p95Ms, p99Ms, coldStarts, coldStartRate, concurrencyMax, costUsd,
  status: 'ok'|'warning'|'critical'|'idle'|'missing', spark: number[]}]` (spark = invocation buckets). `idle` =
  no calls in range; `critical` errorRate > 0.05 or throttles > 0; `warning` errorRate > 0.01 or p95 > timeout × 0.8.
- `GET /serverless/functions/{id}` → function (current/range values, same as the list) + `series: {invocations (SeriesSet:
  `invocations`, `errors`, `throttles`), duration (SeriesSet ms: `avg`, `p95`, `p99`), concurrency (SeriesSet), coldStarts
  (SeriesSet)}`, `recentErrors: [{ts, traceId,
  message}]` (from spans), `functions?: string[]` (Azure), `integration: {id, name}`.
- `POST /integrations` `provider:'azure'` → `config` as above, client secret in the same `secret` field as aws/gcp.

**Monitor templates:** source `services/alerting/internal/eval/templates.go` (`eval.Templates`); the api returns the same list via
`GET /monitor-templates` → `[{id, name, description, kind, severity, query, condition}]`. The `faas` "p95 > timeout × 0.8"
template is `query.relativeToTimeout: true` + threshold 0.8. Extra incident `rootCause` fields: `infra: [{kind: pod|host, entity,
cluster?, node?, reason, restarts?, value?, title, confidence}]`, `treeSummary`, `treeConfidence`.

**Phase 6 api notes (from implementation):** the repo-level analysis summary is `Repo.analysis` (`analysis.llm`); the `analysis` of inventory rows
is item-level. `Pod.lastSeen` (ms), `K8sNode.status` extra field. Container status: `critical` memory > 0.95 × limit;
`warning` cpu|memory > 0.85, a restart in range or throttled > 0.25; `stale` no data for 5 min. eBPF detection over the last 6 hours; if all of a service's
spans are OBI/beyla, `source: 'ebpf'`. `GET /serverless/functions/{id}` accepts a numeric id or a slug; rows have `costUsd`.
Faas `relativeToTimeout` is only valid with `duration_p95|duration_p99`.

## 22. Phase 6 complements

**trooper-agent:**
- Cluster mode uses **watch**: for each resource an initial `list` + `watch?resourceVersion=` (allowWatchBookmarks); 410 Gone → re-list.
  Snapshot metrics are still produced every 30 s from the in-memory state. Events also use watch.
- Container source order (host mode, outside K8s): Docker Engine API → Podman (Docker-compatible socket; `TT_AGENT_PODMAN_SOCKET`,
  default `/run/podman/podman.sock`, rootless `$XDG_RUNTIME_DIR/podman/podman.sock`) → **cgroup v2 fallback** (Linux,
  `/sys/fs/cgroup`; containerd/nerdctl/others: `cpu.stat`, `memory.current`, `memory.max`, `cpu.max`; container id from the cgroup path,
  if name/image is unknown, the first 12 characters of the id). If the same container comes from two sources, the first wins.
- **GPU** (host mode, if `nvidia-smi` is on PATH or `TT_AGENT_NVIDIA_SMI`): `trooper.host.gpu.utilization` (0..1),
  `.memory` (0..1), `.memory_bytes`, `.temperature_c`, `.power_w`; attr `gpu.index`, `gpu.model`, `gpu.uuid`. ClickHouse
  `gpus_1m (org_id, project_id, cluster, host, gpu_index UInt8, gpu_uuid, model, minute | utilization, memory, memory_bytes,
  temperature_c, power_w)` (`deploy/clickhouse/004_gpu.sql`). Extra field on host: `hosts_1m` does not change; the api adds
  `gpus: [{index, model, uuid, utilization, memory, memoryBytes, temperatureC, powerW}]` to the host row (last minute); host
  detail has `series.gpu` (one series per GPU, id `gpu<index>`).
- **Windows** node/host: the agent is built with `GOOS=windows` (host + docker npipe `//./pipe/docker_engine`; no process `fds`);
  Helm `windows.enabled` → a separate DaemonSet with a `kubernetes.io/os: windows` nodeSelector (HostProcess container). `os.type=windows`.
- **Single Helm chart:** `deploy/helm/trooper-agent` is the single source; the old `services/collector/deploy/helm/trooper-agent` is deleted.
  DB/broker targets run in the cluster Deployment in the new chart with `db.enabled` + `db.targets` (static YAML, the old chart's configmap format) and/or
  `db.fromApi` (`TT_API_URL`/`TT_API_KEY`).

**Azure per-function metrics (Application Insights):**
- The App Insights component linked to the Function App is found via ARM: the `hidden-link:<siteId>` tag of
  `Microsoft.Insights/components` resources (Reader is enough). Query via the ARM proxy: `POST {componentId}/query?api-version=2018-04-20`
  (`{"query": KQL, "timespan": "PT15M"}`), same ARM token.
- KQL: `requests | where timestamp > ago(15m) | summarize invocations=sum(itemCount), errors=sumif(itemCount, success == false),
  avg=avg(duration), p95=percentile(duration,95), p99=percentile(duration,99), max=max(duration) by bin(timestamp, 1m),
  operation_Name, cloud_RoleName`. Cold start: empty if there is no `customMetrics`/trace.
- Each function is a separate `serverless_functions` row: slug `azure-<app>-<function>`, `name` = function, `app` = app name,
  `meta.parent` = app slug, `meta.source = 'appinsights'`, `triggers` = the function's trigger. The app row stays (app-level
  metrics). `service` = `cloud_RoleName` (otherwise tag, otherwise app). Metric attr `trooper.faas.id` = function slug.
- Function row `meta`: `parent, source, trigger, disabled?, language?, plan, appId, subscriptionId, resourceGroup`; `url` for an HTTP
  trigger. App row `meta.appInsightsComponent`. If `cloud_RoleName` equals the app name and the app has a tag, the tag wins.
  Summary/byProvider totals do not count rows that have `meta.parent` (to avoid double counting).
- If there is no App Insights or no access, only the app row (current behavior), `meta.appInsights = 'unavailable'`.
- Optional `config.readAppSettings: true` → reads `TASKTROOPER_SERVICE` / `OTEL_SERVICE_NAME` via `POST {site}/config/appsettings/list`
  (requires Website Contributor); silently skipped without permission.
- api: `GET /serverless/functions` rows get `parentId` (id of the app row, only on Azure function rows); the list returns all by
  default; `?parent=<id>` filter. Function detail for an Azure app has `children: Function[]`. Rows have
  `appInsights` (`connected|unavailable`). Summary: rows with `meta.parent` are not included in totals; an app with children is not counted in
  `functions`, cost is on the app row.

**Per-pod traces and logs:**
- `GET /traces` new filters: `pod`, `namespace`, `node`, `host` (span `resource_attrs['k8s.pod.name']`,
  `['k8s.namespace.name']`, `['k8s.node.name']`, `['host.name']`).
- `GET /logs` new filters: `pod`, `namespace` (`attrs['k8s.pod.name']`, `attrs['k8s.namespace.name']`).
- Pod page: the "Traces" section shows the `/traces?service=&pod=` list; the "Logs" link uses `/logs?pod=&namespace=` for a server-side filter.

## 23. Session replay (ADR-021)

**Ingest:** `POST /v1/replay` (ingest-gw :4318). Auth, CORS, `?key=`, internal credential and content types are the same as `/v1/rum`
(§2). The body is a single segment JSON; `Content-Encoding: gzip` is recommended (browser `CompressionStream`). Limit: compressed ≤ 1 MB,
decompressed ≤ 10 MB → 413 if exceeded. gw parses and validates the JSON (`session_id`, `segment`, `format`, `events` required) and sends the body
**gzipped** to `tt.replay` (compresses it if not gzip). Rate limit is shared with RUM.

```jsonc
{
  "resource": { "service": "shop-web", "env": "production", "version": "1.4.0", "platform": "web",   // web | android | ios
                "sdk_name": "@tasktrooper/replay", "sdk_version": "0.1.0" },
  "session_id": "…",            // same as the RUM session_id
  "segment": 0,                 // increasing from 0 within the session; web: per-session counter in localStorage (shared across tabs, persists across page loads)
  "start_ts": 1730000000000,    // unix ms, first event
  "end_ts":   1730000005000,    // unix ms, last event
  "format": "rrweb",            // rrweb (web) | wireframe (mobile)
  "has_full_snapshot": true,    // rrweb: segment contains a FullSnapshot (type 2); wireframe: segment has at least one frame (otherwise the player keeps the previous frame; each segment starts with `screen`)
  "view_id": "…", "view_name": "/checkout",          // view at segment start (optional)
  "user_id": "", "anonymous_id": "…",                 // same as RUM (optional)
  "stats": { "clicks": 3, "errors": 0, "rage_clicks": 0, "inputs": 1 },   // counted by the SDK (optional)
  "events": [ … ]
}
```

- **rrweb:** `events` = `@rrweb/record` 2.x events as is (`{type, data, timestamp}`). Every page load and every
  `checkoutEveryNms` (5 min) starts with a FullSnapshot. A segment is sent when it reaches ≤ 5 s or ≤ 256 KB (estimated before compression);
  on `pagehide` the last segment goes via `fetch keepalive` (≤ 64 KB).
- **wireframe** (mobile) event types:
  - `{"type":"screen","ts":…,"width":390,"height":844,"density":3,"name":"CheckoutScreen"}` — at the start, and when the screen/rotation changes.
  - `{"type":"frame","ts":…,"nodes":[Node…]}` — always a full frame (no diff), only when the hierarchy changes, ≤ 2 frames/s.
    `Node = {"id":int,"x","y","w","h" (dp/pt, screen coordinates),"kind":"view|text|image|button|input|switch|webview|list|icon",
    "bg"?:"#rrggbb[aa]","fg"?:"#rrggbb","radius"?:num,"border"?:"#rrggbb","text"?:string,"masked"?:bool,"alpha"?:0..1,
    "clip"?:[x,y,w,h],"checked"?:bool}`. The list is in draw order (back to front). Masked text keeps its length (`"•••• •••"`).
    `checked` only for `switch` (toggle/checkbox/radio). A labeled toggle = two nodes, `text` + `switch` (toggle area). A dialog scrim
    is a semi-transparent full-screen `view` node. A `button` without `bg` is drawn with a neutral fill in the player. Compose and SwiftUI nodes have no color;
    SwiftUI text is read only when the accessibility tree is on (otherwise content layers — text cannot be unmasked).
    Capture budget: Android main thread CPU time (~12 ms); a frame over budget is dropped and retried once; iOS drops a half frame
    when a full frame exists. `TrooperReplay.stop()` on both platforms (idempotent, seals and sends the open segment).
  - `{"type":"touch","ts":…,"x":…,"y":…,"phase":"down|move|up"}` (move ≤ 10/s), `{"type":"keyboard","ts":…,"visible":bool}`.
- **Privacy (SDK default):** web `maskAllText`, `maskAllInputs` true; `input[type=password]` always masked; selectors
  `data-trooper-block` (empty box of the element's size), `data-trooper-mask`, `data-trooper-unmask`; canvas, cross-origin iframes, network
  bodies, `<script>` are not recorded. Mobile: text masked, images = box, input/secure text always masked; Android
  `View.setTag(R.id.trooper_unmask, true)` / Compose `Modifier.trooperUnmask()`, iOS `view.trooperUnmask = true`.
- **Sampling:** `replay.sampleRate` (default 0.1), `replay.onErrorSampleRate` (default 1.0; last 60 s in an in-memory ring buffer, flushed on an ERROR
  event + recording continues). The decision is made at session start and is fixed for the session (web: localStorage, keyed by session id — since the RUM session is shared across tabs, the decision and segment counter are shared too; each tab starts with its own FullSnapshot). `replay.enabled=false` turns it off completely.

**Kafka:** `tt.replay` — value is the gzipped segment JSON; key `project_id`; headers §3 + `session_id`, `segment`.
The `tt.replay` and `tt.profiles` topics are created with `max.message.bytes=10485760` (compose; same in prod/Helm); gw uses a separate producer for these two topics
(record ≤ 8 MiB). gw validation: `session_id` ≤ 128 characters `[A-Za-z0-9._-]` (part of the S3 key),
`segment` 0..999999.

**Writer:** writes the blob to S3, then the index row to ClickHouse (`deploy/clickhouse/005_replay_profiles_llm.sql`).
- S3: the `TT_S3_*` envs from §12; bucket `TT_REPLAY_S3_BUCKET` (default `tt-replays`, created if missing). Key
  `<org>/<project>/<yyyy-mm-dd>/<session_id>/<segment:06d>.json.gz` (date = `start_ts` UTC). If `TT_S3_ENDPOINT` is missing (no default)
  or the bucket cannot be opened within 15 s, the writer does not consume the replay topic (log + `tt_writer_replay_disabled` gauge = 1). For a writer running
  on the host, `TT_S3_ENDPOINT=http://localhost:9002`. Objects have `Content-Encoding: gzip`, `Content-Type: application/json`. In prod, bucket lifecycle 30 days.
- `replay_segments(org_id, project_id, session_id String, segment UInt32, service, env, version, platform, format
  LowCardinality(String), start_ts, end_ts DateTime64(3), events UInt32, bytes UInt32 (compressed), blob_key String,
  has_full_snapshot UInt8, clicks, errors, rage_clicks UInt16, view_name String, user_id, anonymous_id String, received_at
  DateTime64(3))` — `MergeTree`, `PARTITION BY toDate(start_ts)`, `ORDER BY (org_id, project_id, session_id, segment)`,
  TTL 30 days. If the same (session, segment) arrives twice, queries dedupe with `argMax(…, received_at)`.
- MV `replay_sessions` (`AggregatingMergeTree`, `ORDER BY (org_id, project_id, service, session_id)`): `min(start_ts)`,
  `max(end_ts)`, `count()` segments, `sum(clicks|errors|rage_clicks)`, `any(platform|format|user_id|anonymous_id)`,
  `argMin(view_name, start_ts)` first view. The list query reads from here.

**API** (`openapi.yaml`, tenant filter required):
- `GET /api/v1/replays?app=&range=&q=&hasErrors=&minDurationS=&limit=` → `{ replays: ReplaySummary[] }`
  `ReplaySummary = { sessionId, app, platform, startedAt (ms), durationMs, segments, user: {id?, anonymousId}, country?,
  device?, browser?, os?, appVersion?, firstView, views, errors, clicks, rageClicks }` (user/device fields from `rum_events`).
  `q` searches user id / anonymous id / view name. Order: newest first.
- `GET /api/v1/replays/{sessionId}` → `{ summary: ReplaySummary, segments: [{ segment, startTs, endTs, events, bytes,
  hasFullSnapshot }], timeline: TimelineItem[] }`. `TimelineItem = { ts, kind: 'view'|'action'|'error'|'resource'|'vital',
  label, detail?, viewName?, traceId?, fingerprint?, status?, durationMs? }` — from `rum_events` (all events of the session, ≤ 2000).
- `GET /api/v1/replays/{sessionId}/segments/{segment}` → segment JSON (the gzip body in S3 as is, with `Content-Encoding: gzip`).
  404: no segment or TTL expired.
- Additions to existing endpoints: `GET /apps/{app}/sessions` rows have `hasReplay: boolean`; `GET /apps/{app}/errors/{fingerprint}` and
  `GET /errors/{fingerprint}` responses have a top-level `replay?: { sessionId, offsetMs }` (the newest occurrence covered by a replay; for a backend error via the RUM resource event that carries the trace id).

**Hybrid apps:** React Native → native SDK replay (`TrooperRum.startReplay(options)`, `stopReplay()`, `isReplayRecording()`
— all `Promise<boolean>` —, `notifyReplayError()` triggers the on-error buffer on a JS error; native API `TrooperReplay.notifyError()` /
`TrooperReplay.stop()`; on iOS `TROOPER_RN_NATIVE_SDK=1` also pulls the `TaskTrooper/Replay` subspec, `TROOPER_RN_REPLAY=0` turns it off; without a native SDK
no-op + warning), `resource.platform` = `android|ios`, `sdk_name` is the native SDK's. Flutter → `tasktrooper_flutter` Dart-side wireframe
(`sdk_name` = `trooper_flutter`, `platform` = `android|ios`); if a native SDK is present, native replay is **not started** (to avoid double recording).
**Web tabs:** a tab in buffer mode flushes its buffer and switches to recording if the decision becomes `session` on a localStorage `storage` event.

**UI:** `/replays/:sessionId` player (`?t=<offsetMs>`), **Replays** tab in client app detail; "Watch replay" in Sessions rows and the error
drawer. Web player `@rrweb/replay`; the wireframe player is our own SVG.

## 24. Continuous profiling (ADR-022)

**Ingest:** `POST /v1/profiles` (ingest-gw :4318; `x-trooper-key` / Bearer / internal credential; a request with an `Origin` header gets 403, no CORS,
`?key=` is **not accepted**). gw validates the profile by parsing it (400 if broken); `end − start` ≤ 1 hour, `unit` must match the type. The body is a single profile, ≤ 4 MB (decompressed ≤ 32 MB); `Content-Encoding: gzip` is accepted for both formats. Metadata via query parameters:

| Param | Required | Meaning |
|---|---|---|
| `service` | ✓ | `service.name` |
| `env`, `version`, `host`, `runtime` (`go|jvm|node|dotnet|python`) | | resource |
| `start`, `end` | ✓ | unix ms, the window the profile covers |
| `format` | | `pprof` (default; `Content-Type: application/octet-stream`, gzipped or not — detected by the magic `1f8b`) or `folded` (`text/plain`) |
| `type`, `unit` | ✓ for folded | the type names below; for pprof derived from sample types |

- **Types** (`type` / `unit`): `cpu` (nanoseconds), `wall` (nanoseconds), `alloc_space` (bytes), `alloc_objects` (count),
  `inuse_space` (bytes), `inuse_objects` (count), `goroutines` (count), `lock` (nanoseconds — mutex/monitor wait),
  `block` (nanoseconds). pprof sample type mapping: `cpu/nanoseconds`→cpu, `samples/count` skipped (if cpu exists),
  `alloc_space/bytes`, `alloc_objects/count`, `inuse_space/bytes`, `inuse_objects/count`, `goroutine/count`→goroutines,
  `delay/nanoseconds` (mutex)→lock (for a block profile with `?type=block`), `contentions/count` skipped.
- **folded:** one `frame1;frame2;…;frameN <value>` per line (root first, leaf last, integer value, in `unit`).
  A `# trooper.route=<route> trooper.method=<METHOD>` line applies an endpoint label to the following lines (until the next `#` line;
  a bare `# ` clears the label). Frame names cannot contain `;` or a newline.
- **pprof labels:** sample labels `trooper.route` + `trooper.method` → endpoint. Other labels are dropped.
- **Frame name:** Go `pkg.(*T).Method`, JVM `com.acme.Foo.bar`, Node `fn (path/to/file.js)` (no line/column; anonymous → `(anonymous)`).
  File/line are not stored. If stack depth > 128, the root side is cut with a `[truncated]` frame.

**Kafka:** `tt.profiles` — value is the raw body (may be gzipped); headers §3 + `service`, `env`, `version`, `host`, `runtime`,
`start`, `end`, `format`, `type`, `unit`.

**OTLP profiles:** gw `POST /v1development/profiles` (OTLP/HTTP; protobuf or JSON; gzip) and gRPC
`opentelemetry.proto.collector.profiles.v1development.ProfilesService/Export` (:4317). Auth is the same as the OTLP endpoints. gw writes a single record
to `tt.profiles` per request (`format=otlp`, value = `ExportProfilesServiceRequest` protobuf; service/time headers may be empty).
For each `ResourceProfiles` → `ScopeProfiles` → `Profile` the writer takes: `service.name` / `deployment.environment.name` / `service.version` /
`host.name` / `process.runtime.name` from resource attributes, `time_unix_nano` + `duration_nano` as the window, sample types via the §24 pprof
mapping (if there is no nanosecond type, `samples`/`count` × period → `cpu`), stack = location → function name (inline frames are expanded),
sample attribute `trooper.route`/`trooper.method` → endpoint. Proto: separate modules `go.opentelemetry.io/proto/otlp/profiles/v1development`
and `.../collector/profiles/v1development` **v0.4.1** (same as Collector 0.164 pdata; `ProfilesDictionary` at request level) —
on a breaking change gw pins the version. Writer rules: attributes may come inline or via `key_strindex`/`string_value_strindex`
(both are resolved); index 0 of the tables may be real data (the Collector writes it that way) → read as a normal index. If `service.name` is missing,
`unknown_service:<process.executable.name>` (the eBPF profiler does not write a service). Sample value = sum of `values`, otherwise the timestamp count.
If `cpu/nanoseconds` is present at the same time, the `samples/count` profile is skipped. Metric: `tt_ingest_writer_otlp_profiles_total{outcome}`.

**Writer** (`deploy/clickhouse/005_replay_profiles_llm.sql`):
- `profile_stacks(org_id, project_id, ts DateTime (profile start), service, env, version, host, runtime, type, unit, route,
  method LowCardinality(String), stack_hash UInt64 (cityHash64(stack)), stack Array(LowCardinality(String)), value Int64)` —
  `PARTITION BY toDate(ts)`, `ORDER BY (org_id, project_id, service, type, ts, stack_hash)`, TTL 14 days. One row per profile + type + route +
  stack (identical stacks are summed in the writer). ≤ 10,000 stacks per profile per type; beyond that, the smallest by value
  are folded into a single-frame `[other]` stack. Zero values are not written.
- `profiles(org_id, project_id, ts DateTime64(3) (start), end_ts DateTime64(3), service, env, version, host, runtime, type, unit,
  total Int64, stacks UInt32)` — one row per profile + type, `ORDER BY (org_id, project_id, service, type, ts)`, TTL 14 days.
  CPU usage (cores) = `total / ((end_ts - ts) ns)`; the time series comes from here.

**API:**
- `GET /api/v1/profiles/types?service=&range=` → `{ types: [{ type, unit, runtime, hosts: string[], versions: string[],
  routes: number (whether there are route-labeled stacks / how many routes) }] }` (only those with data).
- `GET /api/v1/profiles/flamegraph?service=&type=&range=&env=&version=&host=&route=&method=` →
  `{ type, unit, total, profiles, from, to, root: FlameNode }`, `FlameNode = { name, value (total), self, children: FlameNode[] }`.
  Root name `"all"`. Nodes below 0.05% of the total value are pruned (their values do not stay in the parent's `self`; they are summed into a single
  child named `"[pruned]"`); ≤ 5000 nodes. Children sorted by value, descending.
- `GET /api/v1/profiles/diff?service=&type=&env=&host=&route=&method=` + base and target: (`baseVersion` & `version`) **or**
  (`baseFrom`, `baseTo`, `from`, `to` unix ms) → `{ type, unit, total, baseTotal, root: DiffNode }`, `DiffNode = FlameNode +
  { baseValue, baseSelf }` (the two trees merged; 0 on the missing side). The UI draws the share difference (`value/total − baseValue/baseTotal`).
- `GET /api/v1/profiles/top?service=&type=&range=&env=&version=&host=&route=&method=&limit=50` → `{ unit, total, functions:
  [{ name, self, total }] }` (`total` = sum of stacks that contain the function, recursion counted once).
- `GET /api/v1/profiles/series?service=&type=&range=&env=&version=` → Series (§8): average cores for `cpu`/`wall`; for `alloc_*`/`lock`/`block` a per-minute rate
  normalized by the profile window (so windowed SDKs do not read low); for instant types (`inuse_*`, `goroutines`) the average value per
  minute; one series per host (≤ 8, the rest "Other").
- The endpoint page takes route/method from the service endpoint; with no profile the UI shows the "Enable profiling" instructions.

**.NET:** `TaskTrooper.Telemetry.Profiling` package (`services.AddTaskTrooperProfiling()` or `TROOPER_PROFILING=true` + package
reference), default types `cpu,alloc_space,lock`, runtime `dotnet`, folded format + route sections. EventPipe samples at a fixed ~1 kHz,
so it is windowed (default 10 s / 60 s; `Duration = Period` means continuous); average overhead ~2% (10–13% within the window).
The route label is tracked across `await`s with `AsyncLocal` (only sampled requests). `cpu` = samples taken while managed code runs. **Python:** `tasktrooper`
`init(profiling=True)`, default `cpu`, optional `wall`, `inuse_space` (tracemalloc, overhead warning), runtime `python`, route label
via the entry span's thread (in asyncio per thread, not per task — noted). Extra Python envs: `TROOPER_PROFILING_HZ`,
`TROOPER_DISABLE_INSTRUMENTATIONS`, `TROOPER_DEBUG`; CPU from the thread CPU clock on Linux, an idle heuristic elsewhere; on Python < 3.11
the frame name is only the function name.

**SDK config:** `TROOPER_PROFILING` (`true|false`, default false), `TROOPER_PROFILING_TYPES` (comma-separated; default
`cpu,alloc_space,inuse_space` + `goroutines` on Go), `TROOPER_PROFILING_PERIOD` (s, default 60), `TROOPER_PROFILING_CPU_DURATION`
(s, default 10). Node: default `cpu,inuse_space` (V8 runs a single sampling heap profiler at a time), no route label (V8 samples do not carry async context); `cpu` = main thread excluding `(idle)`, `wall` optional. Overhead budget < 2% CPU (sdk.md Principle 2). If sending fails, the profile is dropped (no queue), backoff §2.

## 25. AI / LLM monitoring (ADR-023)

**Wire:** OTLP span (no new endpoint). OTel GenAI attributes read by the writer (span):

| Attribute | Meaning |
|---|---|
| `gen_ai.operation.name` | `chat`, `text_completion`, `embeddings`, `generate_content`, `execute_tool`, `invoke_agent`, `create_agent` — **its presence makes the span an LLM span** |
| `gen_ai.provider.name` (old `gen_ai.system`) | `openai`, `anthropic`, `gcp.gemini`, `gcp.vertex_ai`, `aws.bedrock`, `azure.ai.openai`, `mistral_ai`, `cohere`, `groq`, `deepseek`, `x_ai`, `ollama`, `openrouter`, `together_ai`, `fireworks_ai`, `perplexity`, `cerebras` … |
| `gen_ai.request.model`, `gen_ai.response.model` | model (response wins if present) |
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | tokens (int) |
| `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_creation.input_tokens` | prompt cache tokens (Anthropic/OpenAI) — **`input_tokens` includes these too** (OTel semconv; on Anthropic the SDK writes `input_tokens + cache_read + cache_creation`, OpenAI `prompt_tokens` already includes them). Cost: `(input − cache_read − cache_write) × input price + cache_read × cacheRead + cache_write × cacheWrite (otherwise input price) + output × output price` |
| `gen_ai.request.max_tokens`, `gen_ai.request.temperature`, `gen_ai.response.finish_reasons` (string[]), `gen_ai.response.id`, `gen_ai.conversation.id`, `gen_ai.agent.name`, `gen_ai.tool.name` | meta (stays in attrs) |
| `trooper.llm.ttft_ms` (double) | time to first token in a stream (measured by the SDK) |
| `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions` (JSON string) | **only with `captureContent`** — the writer removes these from attrs and writes them to `llm_content` |

Vercel AI SDK provider spans (`ai.*.doGenerate|doStream|doEmbed`) do not carry `gen_ai.operation.name`: the writer completes it from `ai.operationId` / `gen_ai.system` (`anthropic.messages` → `anthropic`, TTFT `ai.response.msToFirstChunk`); `@tasktrooper/node` also does this in the SDK.
Provider SDKs' own GenAI spans (e.g. Anthropic Python SDK 2.x, .NET): `gen_ai.usage.cache_write.input_tokens` →
cache_creation, `gen_ai.response.time_to_first_chunk` (seconds) → `trooper.llm.ttft_ms`. With such SDKs `instrument_anthropic` does not produce a second
span (to avoid double counting).
Old OpenLLMetry names are also accepted: `gen_ai.prompt.N.*` / `gen_ai.completion.N.*` → content; `llm.usage.*` → tokens.
If the OTel log event `gen_ai.client.inference.operation.details` arrives, content is taken from it.

**Writer:**
- On an LLM span `dep_kind = 'ai'`, `dep = "<provider> · <model>"` (client/internal does not matter; `execute_tool`/`invoke_agent`/
  `create_agent` do not produce a dep). The HTTP client span below it in the same request (e.g. `HTTP · api.openai.com`) is **not counted as a dep**
  (an HTTP client span whose parent is an LLM span is skipped) — so duration is not double counted.
- Written to `spans` normally (except content attrs). `llm_content(org_id, project_id, ts DateTime64(6), trace_id, span_id,
  service, system String, input String, output String)` — truncated at 32 KB per field (`…[truncated]`), TTL 7 days,
  `ORDER BY (org_id, project_id, trace_id, span_id)`.
- MV `llm_1m` (from `spans`, only model calls — excluding `execute_tool` / `invoke_agent` / `create_agent`; `/ai/calls` and the trace `llm` field follow the same rule; `AggregatingMergeTree`): `org_id, project_id, service, env, provider, model, operation, route
  (entry route), method, minute | calls, errors, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
  (SimpleAggregateFunction(sum, UInt64)), duration_hist (quantilesTDigestWeighted), ttft_hist` — TTL 30 days; MV `llm_1h` 13 months.
  Multiplied by `sample_weight` (tail sampling).

**API:**
- `GET /api/v1/ai/overview?range=&env=&service=&provider=&model=` → `{ totals: { calls, errors, errorRate, inputTokens,
  outputTokens, cacheReadTokens, costUsd (sum over models with a known price; null if none has a price), costPartial: boolean (there are unpriced calls), p50Ms, p95Ms, ttftP95Ms? },
  series: { calls, tokens (input/output), cost (≤ 8 per model + Other), latency (p95) },
  byModel: [{ provider, model, calls, errors, inputTokens, outputTokens, costUsd, p95Ms, priceKnown }],
  byService: [{ service, calls, inputTokens, outputTokens, costUsd, models: string[] }],
  byEndpoint: [{ service, route, method, calls, avgInputTokens, avgOutputTokens, costUsd, costPerCallUsd, p95Ms }] }`.
- `GET /api/v1/ai/calls?range=&service=&provider=&model=&errorsOnly=&route=&limit=50` → `{ calls: [{ ts, traceId, spanId,
  service, route, provider, model, operation, inputTokens, outputTokens, costUsd, durationMs, ttftMs?, error?, finishReason?,
  hasContent }] }` (recent calls, from `spans`).
- `GET /api/v1/ai/calls/{traceId}/{spanId}` → `{ call, attributes: Record<string,string>, content?: { system?, input, output } }`
  (`input`/`output` JSON string — GenAI message array; the UI shows it by role).
- `GET /api/v1/ai/prices` → `{ updated (date of the default table), prices: [{ provider, model, inputPerMTok, outputPerMTok, cacheReadPerMTok?, cacheWritePerMTok?,
  source: 'default'|'org' }] }`; `PUT /api/v1/ai/prices` (admin+) `{ provider, model, inputPerMTok, outputPerMTok, … }` org
  override; `DELETE /api/v1/ai/prices?provider=&model=` reverts to the default. The price key is **family** + model: the provider is reduced to a family (`gcp.gemini`/`gcp.vertex_ai` → `google`, `azure.ai.openai` → `openai`, `aws.bedrock` → the model's vendor, e.g. `anthropic.claude-…` → `anthropic`). Model match: exact name, otherwise the longest prefix at a non-alphanumeric boundary
  (e.g. `claude-sonnet-4-5-20250929` → `claude-sonnet-4-5`). Default prices in `services/pkg/llmprice/defaults.json`.
  Postgres `llm_prices(org_id, provider, model, input_per_mtok, output_per_mtok, cache_read_per_mtok, cache_write_per_mtok,
  updated_by, updated_at)` (`deploy/postgres/012_llm_prices.sql`).
- `GET /api/v1/services/{id}` response gets `hasAi: boolean`; service scope via `ai/overview?service=`.
- In `GET /api/v1/traces/{id}` spans, an LLM span has `llm?: { provider, model, operation, inputTokens, outputTokens,
  costUsd, ttftMs?, hasContent }`.

**Alert (monitor kind `llm`):** `query: { filter: { service?, provider?, model?, route?, method?, operation? }, metric }`,
`metric` ∈ `cost_usd` (window total, estimated — price rule above, org override included), `cost_usd_per_hour`, `tokens`,
`input_tokens`, `output_tokens`, `calls`, `errors`, `error_rate`, `p95_ms`, `ttft_p95_ms`; source `llm_1m` (`llm_1h` if the window is ≥ 6 hours).
`minRequests` on rate/duration metrics. Models with unknown prices are not included in `cost_*`; if all models in the filter are unpriced,
`no_data`. Price table in `services/pkg/llmprice` (shared by api and alert-engine; defaults are embedded JSON + Postgres `llm_prices`). Templates:
"LLM cost above $X/hour" (`cost_usd_per_hour`, 1 hour), "LLM error rate > 5%" (15 min, minRequests 20), "LLM p95 > 20 s" (15 min),
"Token spike" (`tokens` threshold). UI: on the AI monitoring screen, "Create alert" from a model/service row → `/alerts/new?kind=llm&service=&model=`.

**SDK surface:** see sdk.md § AI / LLM. Shared config: `TROOPER_LLM_CAPTURE_CONTENT` (`true|false`, default false).

## 26. MCP server (`services/mcp`, ADR-025)

MCP server for local coding agents; read-only by default, write is opt-in (ADR-032). (`cmd/tt-mcp`, stdio; official Go MCP SDK). Details: `.ai/mcp.md`.

- **In-process use (ADR-033):** `services/mcp/toolset` builds the same server for the api's dashboard assistant over an in-memory transport;
  its HTTP client then sends the user's session (`apiclient.WithAuth`) through an in-process transport instead of an api key.

- **Wrapping:** Tools wrap `services/api` (`openapi.yaml`); no direct DB connection. HTTP client
  `Authorization: Bearer <TT_API_KEY>`; the tenant comes from the api key (ADR-016). Extra check: if `TT_ORG_ID` /
  `TT_PROJECT_ID` cannot be matched with `GET /me` at startup, the server does not start.
- **Env:** `TT_API_URL`, `TT_API_KEY`, `TT_ORG_ID`, `TT_PROJECT_ID`, `TT_LOG_LEVEL`, `TT_MCP_ALLOW_WRITE` (default `false`). `TT_API_KEY` never appears in tool
  definitions, tool output, error messages or logs.
- **Env file:** at startup `tt-mcp` reads the variables above from the first file it finds, in order: `$TT_MCP_ENV_FILE` → `.tasktrooper/mcp.env` (cwd → git root) →
  `~/.config/tasktrooper/mcp.env`; the environment wins, mode 600 is expected.
  The installer (`/install-agent.sh`) writes this file; binaries are published to a GitHub Release on the `mcp/vX.Y.Z` tag (`.ai/mcp.md` § Install).
- **v1 tool surface** (12, all GET). `range` enum `15m|30m|1h|6h|24h|7d` (default `1h`):

  | Tool | Wrapped endpoint (operationId) |
  |---|---|
  | `tt_list_services` | `GET /api/v1/services` (`getServices`) |
  | `tt_get_service_kpis` | `GET /api/v1/services/{service}` (`getServicesService`) |
  | `tt_list_endpoints` | `GET /api/v1/services/{service}/endpoints` (`getServicesServiceEndpoints`) |
  | `tt_endpoint_rca` | `GET /api/v1/services/{service}/endpoints/{endpoint}/rca` (`getServicesServiceEndpointsEndpointRca`) |
  | `tt_search_traces` | `GET /api/v1/traces` (`getTraces`) |
  | `tt_get_trace` | `GET /api/v1/traces/{traceId}` (`getTracesTraceId`) |
  | `tt_search_logs` | `GET /api/v1/logs` (`getLogs`) |
  | `tt_list_monitors` | `GET /api/v1/monitors` (`getMonitors`) |
  | `tt_list_incidents` | `GET /api/v1/incidents` (`getIncidents`) |
  | `tt_get_incident` | `GET /api/v1/incidents/{id}` (`getIncidentsId`) + `GET /api/v1/incidents/{id}/events` (`getIncidentsIdEvents`) |
  | `tt_list_loadtest_runs` | `GET /api/v1/loadtests/runs` (`getLoadtestsRuns`) |
  | `tt_list_deploys` | `GET /api/v1/deploys` (`getDeploys`) |

  v2 (ADR-032) read additions: `tt_list_errors`, `tt_get_error`, `tt_list_kpis`, `tt_list_alert_suggestions`, `tt_list_inventory`,
  `tt_list_monitor_templates`, `tt_list_notify_channels`, `tt_get_app_summary`, `tt_get_ai_overview`, `tt_list_hosts`,
  `tt_list_databases`, `tt_list_consumer_groups`, `tt_preview_monitor` (POST, does not save), `tt_list_loadtest_scenarios`,
  `tt_get_loadtest_run` → 27 read tools. Write (only with `TT_MCP_ALLOW_WRITE=true`, 11): `tt_create_monitor`,
  `tt_update_monitor`, `tt_delete_monitor`, `tt_accept_suggestion`, `tt_create_kpi`, `tt_update_kpi`, `tt_delete_kpi`,
  `tt_send_slack_test`, `tt_create_loadtest_scenario`, `tt_run_loadtest`, `tt_stop_loadtest`. Wrapped endpoints: `.ai/mcp.md` § Tool surface.
- **Write contract:** no request is sent without `confirm` (`{dryRun, wouldSend, next}`; exception `tt_stop_loadtest`).
  Created monitors / KPIs / scenarios have `source = "agent"` (Postgres check constraints in `014_agent_source.sql`, api
  validation, web badge). Delete only for `source = agent`; `source = code` is not updated. The `error` message of API 400/409/501/502
  (≤ 300 characters) is passed into the tool error; a 5xx body is not.

- **Cross-cutting rules:** every tool takes org/project from the key (no extra tenant parameter); input enum/hex/limit
  validation happens before calling the API; tool output ≤ 64 KB JSON (if exceeded, the array is cut + `truncated: true`,
  `total: <original count>`, `hint: <how to narrow>`); errors are short and action-oriented (`check TT_API_KEY`,
  `call tt_list_endpoints for valid endpoint ids`, …); secrets, stacks and raw upstream bodies are never passed through.
- **codeintel:** `services/codeintel` is not a query API (webhook + analysis); inventory/suggestion data lives in api endpoints.
  MCP does not connect to codeintel; the v2 inventory tools also wrap only `GET /inventory` / `GET /suggestions`.
- **Flag:** `POST /api/v1/ai/incident-summary` is new on main (0764ad4) with a mock provider by default — not wrapped in v1.
  `Incident.rootCause` has no schema in openapi (`rootCause: {}`); MCP passes it through as opaque JSON.

## 27. Self-observability (ADR-027)

**Project:** no seed (ADR-028). `make self` → `api bootstrap --org TaskTrooper --project tasktrooper --owner <email>` creates the self
project idempotently and writes the ingest key to `deploy/self/.run/dsn`; `TT_SELF_PROJECT_ID` is this project's id.

**Go services — env (all optional; no export without `TROOPER_DSN`):**

| Env | Default | Note |
|---|---|---|
| `TROOPER_DSN` | — | ingest DSN of the self project |
| `TROOPER_SERVICE` | fixed per binary (`tt-api` …, ADR-027) | override |
| `TROOPER_ENV` | value of `TT_ENV`, otherwise `development` | |
| `TROOPER_VERSION` | the `-ldflags` version from the build / empty | deploy marker |
| `TROOPER_SAMPLE_RATE` | `1` | lowered for ingest-gw under high traffic |
| `TROOPER_PROFILING` | `false` | §24 |
| `TT_SELF_PROJECT_ID` | `0` (off) | only ingest-gw / ingest-writer: no spans/logs are produced for this project's requests/records |

Instrumentation: incoming HTTP (with route templates), pgx, go-redis, franz-go (produce/consume), ClickHouse queries
(`db.system=clickhouse` client span, SQL goes through `SanitizeSQL`), slog → OTel logs (the existing `slog` handler is kept, log
level unchanged), codeintel LLM calls (§25, content off). Health/metrics endpoints (`/healthz`, `/readyz`, `/metrics`) produce no
spans. No tenant id in metric labels (cardinality); if tenant info is needed, it goes only on spans as span attributes `tt.org_id` /
`tt.project_id`.

**Periodic work = job entry (§4, `trooper.StartJob`; each module's `internal/selftel.StartJob`):** so that background services also
show up on the Services screen with throughput / errors / duration, root spans are `consumer` kind job entries:

| Service | Job (`trooper.process`) | When |
|---|---|---|
| `tt-alert-engine` | `alert.cycle` | each tick that holds a shard (a standby replica produces no span; a leader election error is a failed cycle); `alert.evaluate` / `alert.kpis` children |
| `tt-alert-engine` | `alert.ai_quota_check` | each check (5 min; every replica runs it, `ai_quota_alerts` dedupes) |
| `tt-notifier` | `notify.deliver` | per delivery attempt (an empty outbox poll produces no span) |
| `tt-trooper-agent` | `agent.scrape` | per target scrape |
| `tt-codeintel` | `codeintel.analyze` | per analysis run |
| `tt-loadtest-ctl` | `loadtest.claim`, `loadtest.verify_domains` | only when a run is claimed / while a verification is pending; `loadtest.finish` / `.abort` are children of the claim |
| `tt-loadtest-runner` | `loadtest.run` | per shard (remote parent = claim span, `TRACEPARENT`) |
| `tt-trooper-cloud` | `discover <provider>`, `collect <provider>` | per step |

ingest-writer's `process flush` is a real Kafka consumer (`messaging.system=kafka`, group) — not a job. Services with no
traffic (an idle ingest-gw, a writer with no flushes besides self records) are listed via heartbeat / runtime metrics (§4).

**ingest-gw / ingest-writer (loop protection, `services/ingest/internal/selfobs`):**
- In gw the project is known only after key auth: the server span is opened **after** auth (start time = request arrival time,
  new root — the customer's `traceparent` does not pull the span into another project's trace). For the self project, a fixed,
  **unsampled** parent span context is put into the context instead: the parent-based sampler drops all children (go-redis, pgx, producer),
  and the log handler writes records with this context only locally. Unauthenticated requests (401/403) produce no span (so a wrong
  self key does not create a 401 loop). One `send <topic>` producer span per Kafka record (enqueue → ack); trace context is
  **not injected** into records (the customer record does not change).
- writer: one consumer span `process flush` per flush (`trooper.process=flush`, `messaging.batch.message_count`,
  `tt.self_records`) + one `INSERT <table>` per table batch (`db.system=clickhouse`, `db.operation.batch.size`; no span
  per row). A flush made only of self records produces no span/log; a mixed flush is traced (it happens anyway because of other projects,
  self data does not trigger an extra flush). The warning for an unresolvable self record is local only.
- `TT_SELF_PROJECT_ID` = the project in the DSN; if it is `0` while a DSN is given, the service logs a warning at startup. With telemetry off
  (no `TROOPER_DSN`) there is no extra work/allocation on the hot path (`BenchmarkHTTPTraces` unchanged).

**Dashboard (`apps/web`):** `VITE_TROOPER_DSN` (ingest key DSN, RUM), `VITE_TROOPER_REPLAY_SAMPLE_RATE` (default `0`), app name
`tt-web`. `traceparent` is added to api calls (api CORS allows the `traceparent`, `tracestate`, `baggage` headers);
so the RUM → api → ClickHouse trace is visible end to end. Replay masking is default (all text masked).
Extra (all optional): `VITE_TROOPER_REPLAY_ON_ERROR_SAMPLE_RATE` (default `0`; >0 → other sessions buffer the last 60 s,
sent on the first error), `VITE_TROOPER_SAMPLE_RATE` (`1`), `VITE_TROOPER_ENV` (default Vite `MODE`), `VITE_TROOPER_VERSION`
(default `<package version>+<git sha>`), `VITE_TROOPER_TRACE_PROPAGATION=false` (for an api whose CORS predates ADR-027;
otherwise the preflight fails). Without a DSN the SDK chunks are not in the build at all; if both replay rates are 0, the replay chunk is not included.
The view name is the react-router route template; `identify` uses only the internal user id (no email/name). Details: `apps/web/.env.example`.

**Infrastructure:** `deploy/self/agent.yaml` — trooper-agent; Postgres, ClickHouse, Redpanda, Redis receivers from compose + host
metrics → self project. `make self`: applies the seed, starts the agent, prints the env the services need.
The agent runs with `--agent-modes db,host` (binary + log in `deploy/self/.run/`, gitignored; `make self-down` stops it, calling `make self`
again restarts it). db: Postgres `tt-postgres` (name `trooper` = the `db.namespace` of pgx spans), Redis
`tt-redis`, Redpanda `tt-redpanda` (only `^tt\.` topics and `^tt-` groups — demo topics on the same broker belong to the demo
project; node metrics `:9644/public_metrics`). host: the machine + all compose containers via the Docker Engine API.
**ClickHouse** has no engine in the collector: for now only container metrics (`tasktrooper-clickhouse-1`).

**Notes:**
- The notifier's outbound client is `RedactedTransport`, not otelhttp: the path of Slack/Teams/webhook URLs carries secrets →
  the span has only scheme+host, method, status, `peer.service`; `traceparent` is still added.
- In Go SDK client spans, `url.full` is written without query string and fragment (PII rule; also applies to all customer services).
- loadtest-ctl passes `TRACEPARENT`/`TRACESTATE` + `TROOPER_*` env to the runner; in k8s `TROOPER_DSN` (like `TT_DSN`) must be in a secret.
- Clients that talk to customer infrastructure (trooper-agent scrapers, load test target traffic, cloud metrics OTLP sending) are not traced.
