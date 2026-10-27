# ingest

Two binaries (Go module `github.com/tasktrooper/telemetry/services/ingest`), following `.ai/contracts.md` §2–6, §9.

| Binary | Does |
|---|---|
| `cmd/ingest-gw` | OTLP/HTTP `:4318` (`/v1/traces`, `/v1/metrics`, `/v1/logs`: protobuf or JSON, optional gzip), RUM `POST /v1/rum` (JSON or protobuf), Vercel drains `POST /v1/drains/vercel`, OTLP/gRPC `:4317`, `GET /healthz`, `GET /metrics`. Authenticates ingest keys, rate-limits per project, validates the payload and publishes it as protobuf to Redpanda. Stateless. |
| `cmd/ingest-writer` | Consumer group `tt-ingest-writer` over the 4 topics. Flattens payloads into ClickHouse rows (`spans`, `logs`, `metrics`, `rum_events`, `db_1m`, `db_queries_1m`, `broker_*_1m`, `hosts_1m`, `containers_1m`, `processes_1m`, `k8s_*_1m`, `faas_1m`, `gpus_1m`) and batch-inserts them; Kafka offsets are committed only after the insert succeeded (at-least-once). |

## Run

```bash
make infra && make migrate        # from the repo root (compose infra + schemas)
make run-writer                   # terminal 1
make run-gw                       # terminal 2

curl -s localhost:4318/v1/logs -H 'x-trooper-key: tt_dev_ingest_key' -H 'content-type: application/json' \
  -d '{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"demo"}}]},"scopeLogs":[{"logRecords":[{"severityNumber":9,"body":{"stringValue":"hello"}}]}]}]}'
```

Docker: `docker compose -f deploy/docker-compose.yml --profile app up -d --build ingest-gw ingest-writer`
(`Dockerfile`, build context `services/`, build arg `CMD=ingest-gw|ingest-writer`).

## Tests

```bash
go test ./...                               # unit tests (table-driven)
go test -run '^$' -bench . -benchmem ./internal/...   # benchmarks (or: make bench-ingest)
go test -tags e2e -count=1 -v ./e2e/        # end-to-end against running infra (or: make e2e-ingest)
```

The e2e test starts gw + writer in-process (gw on random ports), sends spans with the OpenTelemetry Go SDK
(OTLP/HTTP, gzip), a RUM JSON beacon (`?key=`, `text/plain`), OTLP/JSON logs and metrics, then polls
ClickHouse until rows exist in `spans`, `red_1m`, `edges_1m`, `errors_1h`, `rum_events`, `rum_views_1m`, `logs`, `db_1m`, `metrics`.
It also sends an SQS consumer span (`messaging_system`, `consumer_1m`), cloud-style broker topic / queue-age metrics
(`broker_topics_1m`, `broker_lag_1m.oldest_age_s`), resolves RUM country through a generated GeoIP db, creates a temporary
Vercel integration (project 1, secret via `env:` ref) and posts a signed log + speed-insights drain.
`TestInfraFamilies` (`e2e/infra_test.go`) sends host / container / process / k8s / faas metrics, a Kubernetes Warning event and
an OBI span through the gw with the internal credential (project 9601, created if missing) and checks every typed table,
`logs` and `spans.resource_attrs`; its rows are deleted afterwards. Data uses the service prefix
`ingest2-e2e-<rand>` and is deleted at the end. **Stop any other `ingest-writer` first** — it shares the consumer group
and would consume the test records without the test's GeoIP db.

## Configuration

Common (`services/pkg/config`): `TT_POSTGRES_URL`, `TT_CLICKHOUSE_URL`, `TT_KAFKA_BROKERS`, `TT_REDIS_URL`, `TT_LOG_LEVEL`.

| Variable | Default | |
|---|---|---|
| `TT_INGEST_HTTP_ADDR` | `:4318` | gw HTTP listen address |
| `TT_INGEST_GRPC_ADDR` | `:4317` | gw gRPC listen address |
| `TT_INGEST_MAX_BODY_BYTES` | `5242880` | max body, enforced on wire **and** decompressed size (413) |
| `TT_INGEST_MAX_RECORD_BYTES` | `921600` | larger requests are split into several Kafka records (Redpanda's default limit is 1 MiB) |
| `TT_INGEST_RATE_LIMIT_RPS` | `1000` | requests/s per project when the key has no `rate_limit_rps`; burst 2×; `0` disables |
| `TT_INGEST_PRODUCE_BUFFER_RECORDS` | `20000` | records waiting for broker ack; above → 503 + `Retry-After` |
| `TT_INGEST_PRODUCE_BUFFER_BYTES` | `268435456` | same, in bytes |
| `TT_INGEST_PRODUCE_TIMEOUT` | `30s` | per-record delivery timeout |
| `TT_INGEST_WRITER_GROUP` | `tt-ingest-writer` | consumer group |
| `TT_INGEST_WRITER_TOPICS` | the 4 ingest topics | |
| `TT_INGEST_WRITER_BATCH_ROWS` | `50000` | flush when any table batch has this many rows… |
| `TT_INGEST_WRITER_FLUSH_INTERVAL` | `1s` | …or when the oldest buffered record is this old |
| `TT_INGEST_WRITER_MAX_POLL` | `5000` | records per poll |
| `TT_INGEST_WRITER_RETRY_MAX` | `30s` | cap of the ClickHouse retry backoff (retries never give up; offsets stay uncommitted) |
| `TT_INGEST_WRITER_HEALTH_ADDR` | `:4320` | writer `/healthz` (pings ClickHouse); empty disables |
| `TT_INGEST_METRICS_ADDR` | — | gw Prometheus listener; unset → `/metrics` on the HTTP port |
| `TT_INGEST_WRITER_METRICS_ADDR` | `:9464` | writer Prometheus `/metrics`; empty disables (bind failure only logs) |
| `TT_TAIL_SAMPLE_RATE` | `1` | writer tail sampling: share of "boring" traces kept (`1` = keep all) |
| `TT_TAIL_SLOW_MS` | `1000` | traces with an entry span ≥ this are always kept (`0` disables the rule) |
| `TT_GEOIP_DB` | — | path to a MaxMind GeoLite2-Country / DB-IP Country Lite `.mmdb` (not bundled) |
| `TT_SECRETS_KEY` | dev key | AES key for `secret:<id>` drain secrets (contracts §10) |
| `TT_INTERNAL_TOKEN` | — | internal service credential (see *Behaviour notes*); unset disables it |

## Behaviour notes

- **Auth:** `x-trooper-key`, `Authorization: Bearer`, or `?key=` (only `/v1/rum`). sha256 lookup in `api_keys`
  (`kind='ingest'`, not revoked) with 60 s TTL cache, 10 s negative cache, singleflight, stale entry served if Postgres is
  down. `last_used_at` is written asynchronously, at most every 5 min per key. Browser `Origin` is checked against
  `allowed_origins` (exact origin, host, or `*.example.com`); CORS on `/v1/rum`, `/v1/traces`, `/v1/logs`.
- **Internal service credential** (contracts §16; trooper-cloud, loadtest-runner): `x-trooper-internal: <TT_INTERNAL_TOKEN>`
  and `x-trooper-project: <id>` on OTLP/HTTP, OTLP/gRPC metadata and `/v1/rum`. When the internal header is present the
  ingest key is ignored; the token is compared in constant time (sha256 digests), the project must exist in Postgres
  (org resolved via `projects.org_id`, 60 s cache, 10 s negative cache, stale on Postgres errors). Unset token → feature
  off (401). Requests with an `Origin` header (HTTP, or `origin` gRPC metadata) are rejected with 403, and the CORS
  preflight never allows the header. Rate limit as for keys (`TT_INGEST_RATE_LIMIT_RPS`).
- **Rate limit:** GCRA in Redis (Lua, shared across replicas); if Redis fails the gw switches to an in-memory token bucket
  for 5 s at a time. 429 + `Retry-After` (gRPC: `ResourceExhausted` + `RetryInfo`).
- **Produce:** franz-go, async, zstd, key = `project_id`, headers per contracts §3. Protobuf bodies ≤ record limit are
  forwarded byte-for-byte; JSON (OTLP/JSON hex ids handled) is converted to protobuf; oversized requests are re-packed by
  resource/scope. Responses are empty `Export*ServiceResponse` (`{}` for JSON).
- **Spans:** `is_entry` = server/consumer span with no parent, a remote parent (OTLP `SpanFlags` is-remote bits) or —
  without flags — a parent that is not a server/internal span of the same resource batch. Route: `trooper.route` →
  `http.route` → normalized `url.path`/`http.target` (numbers, UUIDs, hex, ULIDs, opaque tokens → `:id`); consumers:
  `trooper.process` → consumer group. Non-entry spans get `route = entry_route`. `entry_route`/`entry_method` come from
  `trooper.entry.route`/`trooper.entry.method`, else the own route for entry spans, else inherited from the nearest
  ancestor entry span in the same batch. Caller: `trooper.caller` (kind `service`) or user-agent classification
  (`web`, `mobile`, `probe`, `webhook` with the vendor as caller, `unknown`). Dependencies per contracts §4
  (`redis`/`memcached`/`valkey` → `cache`, `gRPC · x` for gRPC). `db.query.text`/`db.statement` is replaced by its
  normalized form (literals → `?`, `IN (...)` collapsed) and hashed. Error fingerprint: xxhash over service, type and the
  top 5 normalized frames (or the masked message; bare 5xx → name + route). Exception type/message are copied into
  `attrs` (read by `errors_1h_mv`). Attribute maps: ≤ 64 keys, values ≤ 1 KiB (stack traces in events ≤ 16 KiB).
- **Logs:** severity from `severity_number` (or text), `route` from `trooper.entry.route`, `pattern_id` = Drain-style
  template hash (tokens with digits / ids / quoted values / `k=v` values masked). Resource `k8s.cluster.name`,
  `k8s.namespace.name`, `k8s.pod.name`, `k8s.node.name` are copied into `attrs` unless the record sets them (Kubernetes
  Warning events: `service.name=k8s`, `k8s.event.*` attrs). Spans keep the whole resource in `resource_attrs`, so eBPF
  (OBI/Beyla) spans are recognisable by `resource_attrs['telemetry.sdk.name']`.
- **Metrics:** `trooper.db.*`, `trooper.db.query.*`, `trooper.broker.*` (incl. `trooper.broker.topic.*` →
  `broker_topics_1m`, `trooper.broker.oldest_age_s` → `broker_lag_1m.oldest_age_s`) go to the typed tables. Entity
  attributes are read from the data point first, then the resource — so trooper-cloud payloads (resource-level
  `trooper.broker.cluster`, `cloud.*`, `trooper.integration.id`) route exactly like collector ones, merged per entity and
  minute within a flush. Columns not reported are written as **`-Inf` (max columns) / `+Inf` (`hit_ratio`, min)**, not
  NaN: ClickHouse `max`/`min` over NaN is order-dependent, so NaN partial rows would corrupt merges. Readers should treat
  non-finite values as "missing". `trooper.host.*`, `trooper.container.*`, `trooper.process.*`, `trooper.k8s.{node,pod,workload}.*`
  and `trooper.faas.*` (contracts §20, §21) go to `hosts_1m`, `containers_1m`, `processes_1m`, `k8s_nodes_1m`, `k8s_pods_1m`,
  `k8s_workloads_1m`, `faas_1m` (`internal/flatten/infra.go`: one spec per table — dimension columns and the attribute each
  comes from, data point first then resource; row key = all dimensions + minute; gauges merged with `max`, unreported
  `-Inf`; pod `phase`/`reason` taken from the newest data point; `process.command` capped at 128 chars).
  `trooper.host.gpu.*` (contracts §22) goes to `gpus_1m` (dims `cluster, host, gpu_index` (UInt8, from `gpu.index`),
  `gpu_uuid`, `model`), not `hosts_1m`. Unknown names
  under these prefixes fall through to `metrics`. Everything else → `metrics` (gauge/sum/histogram/exp_histogram as explicit
  buckets/summary with quantiles in attrs; `attrs_hash` = xxhash of sorted attrs).
- **RUM:** protojson (proto field names, unknown fields ignored). Clock skew: if the newest event of a batch is > 5 min
  from `received_at`, the whole batch is shifted onto it. Browser/OS/device fall back to the `User-Agent` header.
  View URLs keep query keys only; resource URLs are `scheme://host` + normalized path. Clock skew: only a batch whose
  newest event is > 5 min in the **future** or > 24 h in the past is shifted onto `received_at` (spacing kept);
  offline-queued / backfilled events up to 24 h old keep their own timestamps.
- **GeoIP:** with `TT_GEOIP_DB` the writer resolves `rum_events.country` from the `client_ip` header (private/reserved
  ranges → empty); the IP itself is never stored. An event attribute `geo.country` (set by drains) wins. No database is
  bundled: download GeoLite2-Country (MaxMind account, free) or DB-IP "IP to Country Lite" (CC BY 4.0) and mount it.
  Tests build a tiny db with `internal/geo/geotest` (mmdbwriter). Spans get no country (their client IP is a server).
- **Messaging:** `spans.messaging_system` = normalized `messaging.system` (`kafka`, `rabbitmq`, `aws_sqs`, `gcp_pubsub`,
  `aws_sns`, `servicebus`, `eventhubs`, `nats`; others lower-case with `.`/`-` → `_`); feeds `consumer_1m.messaging_system`.
- **Tail sampling** (writer, `TT_TAIL_SAMPLE_RATE` < 1): per trace within one flush — any error span or an entry span
  ≥ `TT_TAIL_SLOW_MS` keeps the whole trace unchanged; other traces are kept iff `xxhash(trace_id) < rate` and their spans
  get `sample_weight *= 1/rate`, so every `sum(sample_weight)` rollup stays unbiased (tests check RED in expectation).
  The hash decision is identical across batches / replicas; the error/slow rule is best effort per flush (a trace whose
  error arrives in another flush may lose its other spans with probability 1 − rate; counts stay unbiased). Rows of a
  failed flush are not re-sampled.

## Vercel drains

`POST /v1/drains/vercel?key=<ingest key>&integration=<integrations.id>` (contracts §10). The integration must be
`provider='vercel'`, not disabled, and belong to the key's project. Config (`integrations.config`):
`drainSecretRef` (`secret:<id>` / `env:` / `file:`), `verifyToken`, `projectMap` (Vercel project **name or id** → service),
optional `speedInsightsUnit: "s"` if vitals arrive in seconds (default ms, web-vitals units). Resolved config is cached 60 s.

- Verification: `GET`/`HEAD`, or an unsigned `POST` with an empty body or `x-vercel-verify` header → 200 with
  `x-vercel-verify: <verifyToken>` (the header is also set on every response).
- Data: `x-vercel-signature` = hex HMAC-SHA1(secret, body), constant-time; checked against the decompressed body and,
  for gzip, the wire bytes. No secret configured → 401; mismatch → 403. Body: JSON array, NDJSON or one object; trace
  drains may also be OTLP protobuf (`Content-Type: application/x-protobuf`). Kind is detected per entry.
- Logs → `tt.otlp.logs`: service = `projectMap[projectId] ?? projectMap[projectName] ?? projectName ?? projectId`;
  resource `deployment.environment.name`, `service.version` = deployment id, `cloud.provider=vercel`,
  `trooper.integration.id`; attrs `vercel.source|deployment_id|request_id|type|host|region|cache|path_type|waf_action…`,
  `http.route` = `trooper.entry.route` (Next.js `[id]` → `:id`, `[...slug]` → `*slug`), `http.request.method`,
  `url.path` (query values stripped), `http.response.status_code`; severity from `level` (`statusCode -1` → error);
  trace/span ids kept. Client IP, user agent, referer and JA3/JA4 are dropped.
- Traces (OTLP JSON incl. lower-case `kind`/status names, or protobuf) → `tt.otlp.traces` with the service mapped via
  `vercel.projectId` and Vercel resource attrs added.
- Speed insights → RUM `VITAL` (LCP/INP/CLS/FCP/TTFB/FID, rating by web-vitals thresholds), view_name = route (or path)
  normalized, device/OS/browser from the entry, `geo.country` from the entry. Web analytics → `VIEW_START` (pageview) or
  `CUSTOM` (event, `eventData` flattened to props). `anonymous_id` = salted sha256 of the device id; drains carry no
  session, so `session_id` = device × UTC day (or a hashed `sessionId` when present).
- `client_ip` / `user_agent` headers are not forwarded for drains (they would describe Vercel's servers).

## Prometheus metrics

gw: `tt_ingest_gw_requests_total{endpoint,status}`, `_request_bytes_total`, `_request_duration_seconds`,
`_auth_failures_total{reason=invalid_key|origin|integration|signature|internal|internal_origin}`, `_rate_limited_total`, `_produced_records_total{topic}`,
`_produce_errors_total{topic,reason=failed|buffer_full}`, `_produce_latency_seconds`, `_drain_events_total{provider,kind}`
(gRPC endpoints are labelled `grpc:<topic>`). writer: `tt_ingest_writer_records_consumed_total`, `_decode_errors_total`,
`_rows_inserted_total{table}`, `_clickhouse_errors_total{table}`, `_insert_duration_seconds{table}`,
`_batch_duration_seconds`, `_consumer_lag{topic,partition}`, `_tail_sampling_spans_total{decision}` + Go/process metrics.
