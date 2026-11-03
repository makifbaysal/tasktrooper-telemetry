# Data model

> Applied schema: `deploy/clickhouse/001_init.sql` and `deploy/postgres/001_init.sql` (single source of truth). This file
> explains the design rationale. Rollups (`red_1m/1h`, `edges_1m` (= deps_1m), `callers_1m`, `consumer_1m`, `errors_1h`,
> `rum_views_1m`, `rum_vitals_1m`, `rum_resources_1m`, `trace_index`, `deploys`) are materialized views over raw `spans`/`rum_events`;
> `edges_1m` needs no join because the SDK writes `trooper.entry.route` on every span.

## ClickHouse (telemetry)

All tables are in the `MergeTree` family, not `ReplacingMergeTree`; ORDER BY starts with the tenant. TTL is per table, overridden per plan.

### `spans`
```sql
CREATE TABLE spans (
  org_id UInt32, project_id UInt32,
  ts DateTime64(6), duration_ns UInt64,
  trace_id FixedString(16), span_id FixedString(8), parent_span_id FixedString(8),
  service LowCardinality(String), env LowCardinality(String), version LowCardinality(String),
  kind Enum8('server'=1,'client'=2,'producer'=3,'consumer'=4,'internal'=5),
  name String, route LowCardinality(String), http_method LowCardinality(String), status_code UInt16,
  is_error UInt8, error_fingerprint UInt64,
  attrs Map(LowCardinality(String), String)
) ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, service, route, ts)
TTL toDateTime(ts) + INTERVAL 15 DAY;
```

### `red_1m` (rollup, produced by ingest-writer)
`org_id, project_id, service, env, route, method, minute, count, errors, duration_hist (AggregateFunction(quantilesTDigest...)), status_2xx/3xx/4xx/5xx`
→ `red_1h` via MV. Retention: 1m → 30 days, 1h → 13 months.

### `consumer_1m`
`service, topic, group, minute, processed, failed, retried, dlq, duration_hist, max_lag`
- The unit in the UI is `(service, process=group)`; `topic` is a separate dimension. One topic → N processes (each a separate group, separate lag).
- Background job entries (contracts §4) also land here: `messaging_system = 'job'`, `topic`/`group` empty, process = job name.
  api does not show these as queue/topic/group (process source `cron`, method `JOB`).
- The service list finds services without traffic from `spans` + `metrics` (`GROUP BY service`, at most the last 24 hours of the window,
  services already visible in the rollup are excluded via the primary key with `NOT IN`; `trooper.*` metrics excluded, heartbeat included).

### `logs`
`ts, org_id, project_id, service, env, severity, body, trace_id, span_id, attrs Map, pattern_id UInt64`
- `tokenbf_v1` skip index on `body` (full-text search).
- `pattern_id`: log template extracted at ingest with the Drain algorithm → for log pattern alerts and grouping.

### `rum_events`
`ts, org_id, project_id, service, env, type, session_id, view_id, view_name, anonymous_id, user_id, country, device_type, browser, os, app_version, duration_ms, vital_name, vital_value, error_fingerprint, attrs Map`
→ MV: `rum_views_1m` (per-view views, p75 LCP/INP/CLS, errors), `rum_sessions` (AggregatingMergeTree, session summaries).

### `errors`
Fingerprint-based issue table: `first_seen, last_seen, count, sample_event_id, status (open/resolved/ignored)` — status in Postgres, counts in ClickHouse.

### `db_1m` (DB/cache metrics; written by the agent, cloud integration or direct collector)
`database_id, node, minute, cpu, memory, disk, connections, max_connections, ops, p95_ms, slow, hit_ratio, evictions, repl_lag_ms`
- Top queries: `db_queries_1m(database_id, query_hash, minute, calls, total_ms, p95_ms, rows)`; text is normalised (parameters `$1`), contains no PII. The calling service/endpoint is matched via the `db.statement` hash in spans.
- Service ↔ DB matching: dependency name (`postgres · orders-db`) = `databases.name`.

### `broker_lag_1m` (written by the broker collector)
`cluster_id, topic, group, partition, minute, end_offset, committed_offset, lag, consume_rate, produce_rate` — RabbitMQ: `partition = 0`, `lag = messages_ready`, plus `unacked`.
- Kafka: topic end offsets and group committed offsets are read via AdminClient every 15 s; no messages are consumed. Group ↔ service matching via the `group.id` reported by the SDK (RabbitMQ: consumer tag).
- `consumer_1m.max_lag` comes from the SDK; used when there is no broker, and the broker value takes precedence when a broker is connected.

### Host, container, process, Kubernetes and serverless (ADR-019/020, `deploy/clickhouse/003_infra_k8s_faas.sql`)
All are `AggregatingMergeTree`, `PARTITION BY toYYYYMM(minute)`, row = entity dimensions + minute. Gauges are
`SimpleAggregateFunction(max, Float64)`; an unreported value is `-Inf` (contracts §5, readers use `isFinite`). Empty `cluster` = outside K8s.
Writer: ingest-writer (`trooper.host|container|process|k8s|faas.*` OTLP gauges; metric → column mapping in contracts §20/§21).

| Table | Dimensions (ORDER BY) | Values | TTL |
|---|---|---|---|
| `hosts_1m` | `cluster, host, host_id, os, arch, node` (`host, minute`) | `cpu, cpu_cores, memory, memory_bytes, disk, disk_read, disk_write, net_in, net_out, load1, uptime_s` | 30 days |
| `containers_1m` | `cluster, host, namespace, pod, pod_uid, container, container_id, image, workload_kind, workload, service` (`cluster, namespace, pod, container, host, minute`) | `cpu, cpu_limit, memory_bytes, memory_limit, restarts, net_in, net_out, throttled` | 30 days |
| `processes_1m` | `host, container_id, pid UInt32, exe, command` (`host, pid, exe, minute`) | `cpu, memory_bytes, threads, fds` | 7 days |
| `k8s_nodes_1m` | `cluster, node` | `ready, allocatable_cpu, allocatable_memory, pods` | 30 days |
| `k8s_pods_1m` | `cluster, namespace, pod, pod_uid, node, workload_kind, workload, service` (`cluster, namespace, pod, pod_uid, minute`) | `phase`, `reason` (`SimpleAggregateFunction(anyLast, String)`), `ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit` | 30 days |
| `k8s_workloads_1m` | `cluster, namespace, kind, name` | `desired, ready, available, updated` | 30 days |
| `faas_1m` | `function_id, provider, region` (`function_id, minute`) | `invocations, errors, throttles, duration_avg, duration_max, duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units` | 30 days |
| `gpus_1m` (`004_gpu.sql`, contracts §22) | `cluster, host, gpu_index UInt8, gpu_uuid, model` (`host, gpu_index, gpu_uuid, minute`) | `utilization` (0..1), `memory` (0..1), `memory_bytes`, `temperature_c`, `power_w` | 30 days |

- `gpus_1m`: trooper-agent host mode `nvidia-smi` gauges (`trooper.host.gpu.*`, attr `gpu.index|uuid|model`); `hosts_1m`
  is unchanged. api adds the GPUs from the last minute (`gpus`) to the host row and a per-GPU utilization series (`series.gpu`,
  id `gpu<index>`) to host detail. A GPU that went silent 3 min before its host is not listed.
- Azure function rows (`serverless_functions.meta.parent` = app slug, App Insights) write their own `faas_1m` rows
  (`function_id` = function slug); the app row is the app-level total. Function rows are not counted in summary totals.
- `containers_1m` also has `host` in the sort key (in addition to the contract): so that same-named containers outside K8s (empty pod/namespace)
  on different hosts do not merge.
- Query pattern: first `max(...)` with `GROUP BY <key>, minute` (partial rows of the same minute), then filter with `isFinite` and
  aggregate over the range. `restarts` is a cumulative counter: increase in the range = `max − min`. `faas_1m` range total = `sum` of the minutes.
- K8s Warning events are not a separate table: `logs` (`service='k8s'`, `attrs['k8s.event.*']`, `attrs['k8s.cluster.name']` —
  the writer copies `k8s.cluster.name|namespace.name|pod.name|node.name` from the resource onto the log row).
- eBPF (OBI) spans come through the normal `spans` path; source `resource_attrs['telemetry.sdk.name'] IN
  ('opentelemetry-ebpf-instrumentation', 'beyla')`.

### Replay, profiles and LLM (ADR-021/022/023, `deploy/clickhouse/005_replay_profiles_llm.sql`)

| Table | Row | Note | TTL |
|---|---|---|---|
| `replay_segments` | session + segment | blob in S3 (`blob_key`), index + counters here | 30 days |
| `replay_sessions` (MV, Aggregating) | session | list screen: duration, segments, clicks/errors, first view | 30 days |
| `profile_stacks` | profile + type + route + stack | `stack Array(LowCardinality(String))` root first; flame graph `GROUP BY stack` | 14 days |
| `profiles` | profile + type | total value, window → CPU core series | 14 days |
| `llm_1m` / `llm_1h` (MV, from `spans`) | provider, model, operation, service, entry route, minute | calls, errors, tokens, duration/TTFT digest; cost in api | 30 days / 13 months |
| `llm_content` | LLM span | prompt/completion (`captureContent`), field ≤ 32 KB | 7 days |

Details and columns: contracts §23–25. `user_id` is in the replay index (session search), not in rollups.

## PostgreSQL (metadata)

```
orgs(id, name, plan, created_at)
users(id, email, name) · memberships(org_id, user_id, role)
projects(id, org_id, name, slug)
api_keys(id, project_id, kind ingest|api, hash, allowed_origins[], last_used_at)
services(id, project_id, name, type, language, first_seen, last_seen)
endpoints(id, service_id, method, route, source enum(observed|code|both), code_ref jsonb)
monitors(id, project_id, name, kind, query jsonb, condition jsonb, severity, source enum(manual|code|suggestion|suggested|template|agent),
         status enum(active|muted|draft), suggestion_id, created_by)
incidents(id, monitor_id, kpi_id null, state enum(firing|acknowledged|resolved), started_at, resolved_at, peak_value,
          root_cause jsonb)            -- RcaResult snapshot: path, root node, confidence, summary
kpis(id, project_id, name, description, owner, target jsonb, sli enum(latency|availability|success_rate),
     good_when jsonb, objective real, window enum(7d|28d), source enum(code|template|manual|agent), created_by)
kpi_alert_rules(id, kpi_id, kind enum(burn_rate|below_target), long_window, short_window, threshold real,
                severity, channels text[])
notification_channels(id, project_id, type, config jsonb)
repositories(id, project_id, provider github|gitlab|bitbucket|local, full_name, default_branch, last_analyzed_sha, status,
             base_url null /*gitlab self-managed only*/, credentials_ref, webhook_secret_ref, webhook_received_at)
             -- all columns: contracts §13, §18, §18a
inventory_items(id, repository_id, kind endpoint|consumer|cron|view|dependency, key, source, file, line, meta jsonb)
-- consumer/cron: key = process name, source = topic/queue or schedule
alert_suggestions(id, inventory_item_id, monitor_draft jsonb, rationale text, confidence real, state proposed|accepted|rejected)
repo_service_mappings(repository_id, project_id, path, detected_name, evidence, kind, items, service null,
                      status matched|unmatched|confirmed|ignored, decided_by, updated_at)   -- folder → service, ADR-036
databases(id, project_id, name, engine postgres|mysql|redis|mongodb|clickhouse, host, collector agent|cloud|direct,
          credentials_ref, cloud_resource_id, created_at)      -- credentials in the secret store, only the ref here
llm_prices(org_id, provider, model, input_per_mtok, output_per_mtok, cache_read_per_mtok, cache_write_per_mtok)  -- org override, defaults in api
broker_clusters(id, project_id, name, kind kafka|rabbitmq, endpoint, vhost, auth jsonb, credentials_ref, created_at)
loadtest_scenarios(id, project_id, name, spec jsonb, source manual|generated|agent)
loadtest_runs(id, scenario_id, state, started_at, finished_at, summary jsonb, thresholds_passed bool)
assistant_threads(id, org_id, project_id, user_id, title, created_at, updated_at)        -- dashboard assistant, private per user (ADR-033)
assistant_messages(id, thread_id, org_id, project_id, role user|assistant, content jsonb, transcript jsonb, created_at)
assistant_attachments(id uuid, org_id, project_id, user_id, media_type, size, data bytea, created_at)  -- images ≤ 5 MB
```

## Cardinality rules

- `route` is always a template: `/orders/123` → `/orders/:id`. The SDK takes it from the framework router; if it cannot, ingest-writer normalises heuristically (number, uuid, hex, ulid → `:id`).
- Unique route limit per project: 2,000. If exceeded, new routes are grouped under `__other__` and a warning is shown in the UI.
- Number of keys in the `attrs` map ≤ 64, value ≤ 1 KB.
- `user_id` only in `rum_events` and span attrs; never in rollup tables.

## Outgoing traffic & duration breakdown

Source: `client` / `producer` spans under the server span (DB, cache, HTTP, queue). The category is derived from OTel attributes:
`db.system` → `db` (redis/memcached → `cache`), `http.request.method` + kind=client → `http`, `messaging.system` → `queue`. Remaining duration = `app` (own code).

### `deps_1m` (rollup, produced by ingest-writer)
`org_id, project_id, service, env, route, method, dependency, dep_kind, minute, calls, errors, duration_sum, duration_hist`
- Outgoing traffic = `calls`, fan-out per endpoint = `calls / red_1m.count`.
- Endpoint duration breakdown = `duration_sum / red_1m.count` (per category). `app = avg_duration − Σ dependency durations` (corrected with the critical path for parallel calls, v2).
- `dependency` name normalised: `postgres · <db.name>`, `HTTP · <peer.service | server.address>`, `kafka · <topic>`.

## Incoming traffic source (callers)

For each server span the caller is resolved in this order (first match wins):
1. **Trace context:** parent span in another service → `service` (e.g. orders-api). If it comes from the RUM SDK → `web` / `mobile` (shop-web, shop-mobile).
2. **API key:** name of the project API key carrying the request → `partner` (e.g. "acme-erp").
3. **User-agent classification:** known webhook senders (Stripe, GitHub, Shopify...) → `webhook`; `kube-probe`, ELB health checker → `probe`.
4. None → `unknown` ("Uninstrumented"). The UI shows this as a warning: the SDK should be added to the caller.

### `callers_1m` (rollup)
`org_id, project_id, service, env, route, method, caller, caller_kind, minute, count, errors, duration_hist`
- Service A's outgoing `HTTP · B` calls and service B's incoming traffic with `caller = A` are produced from the same span pair → the two screens are always consistent.
- `caller` cardinality is kept low: a user id / IP is never a caller.

## KPI & root cause

### `sli_1m` (rollup, produced by ingest-writer)
`(org_id, project_id, kpi_id, ts, good UInt64, total UInt64)` — the KPI's good-event definition is evaluated during ingest.
Burn rate = `(1 − good/total) / (1 − objective)`; windows (5m/30m/1h/6h) are aggregated from this table. Retention 35 days.

### `edges_1m` (rollup)
`(org_id, project_id, ts, parent_service, parent_endpoint, child_kind, child_service, child_ref, calls, errors,
duration_sum, duration_p95)` — span parent→child edges. The RCA tree is built from here; calls per request
`calls / parent_calls`. Cardinality: `child_ref` is a route template / db+statement fingerprint, no raw URL.
