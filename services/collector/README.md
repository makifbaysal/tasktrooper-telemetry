# trooper-agent (services/collector)

Database and message-broker collector. It reads statistics from PostgreSQL, MySQL, Redis, MongoDB, Kafka and
RabbitMQ with read-only monitoring users and ships them to TaskTrooper as **OTLP metrics** (contracts §5)
via the OpenTelemetry Go SDK (`otlpmetrichttp`) to ingest-gw. It never consumes or publishes messages and
never runs anything but catalog/statistics queries.

```
targets (static YAML and/or GET /api/v1/internal/collector-config every 60 s)
   └─ one loop per target, every `interval` (15 s), per-target `timeout`
        └─ scraper (pgx / go-sql-driver / go-redis / mongo-driver v2 / franz-go kadm / RabbitMQ management API /
                    Prometheus text endpoints / gopsutil)
             └─ emitter: gauges (latest snapshot per target) + delta sums (query calls/ms/rows)
                  └─ OTLP/HTTP protobuf, gzip → {TT_DSN host}/v1/metrics, header x-trooper-key
```

A failing target never stops the agent: the error is logged (each distinct error once, repeats every
5 min), its gauges are dropped and `trooper.collector.up{trooper.collector.target, trooper.collector.kind}` = 0.
Panics inside a scraper are recovered the same way.

## Run

```bash
cd services/collector
make build test                      # GOWORK=off by default (module uses replace ../pkg)

# static config, local ingest-gw
TT_DSN=http://tt_dev_ingest_key@localhost:4318/1 \
ORDERS_DB_PASSWORD=… go run ./cmd/trooper-agent --config examples/agent.yaml

# targets from the api (+ optional static ones; static wins on id clashes)
TT_DSN=… TT_API_URL=http://localhost:8080 TT_API_KEY=tt_dev_api_key go run ./cmd/trooper-agent

# "test connection": scrape every target twice (5 s apart), print samples as JSON lines, exit 1 if any failed
go run ./cmd/trooper-agent --config examples/agent.yaml --once            # TT_DSN optional here
```

| Flag | Env | Default | |
|---|---|---|---|
| `--config` | `TT_COLLECTOR_CONFIG` | | static YAML target list |
| `--api-url`, `--api-key` | `TT_API_URL`, `TT_API_KEY` | | pull targets from the api every `--poll` |
| `--poll` | `TT_COLLECTOR_POLL` | `60s` | remote config interval; on failure the current targets are kept |
| `--dsn` | `TT_DSN` | | `<scheme>://<ingest key>@<host>[:port]/<project>` → endpoint `<scheme>://<host>/v1/metrics` + `x-trooper-key` |
| `--modes` | `TT_COLLECTOR_MODES` | all | only targets whose `collector` is in the list (`agent,direct,cloud`) |
| `--host-metrics` | `TT_COLLECTOR_HOST_METRICS` | `false` | report this machine's CPU/memory/disk for `collector: agent` databases |
| `--host-disk-path` | `TT_COLLECTOR_HOST_DISK_PATH` | `/` | filesystem behind `trooper.db.disk` |
| `--once`, `--once-gap` | | `5s` | one-shot mode (see above) |
| `--log-level` | `TT_LOG_LEVEL` | `info` | JSON logs (`services/pkg/logx`) |

Resource: `service.name=trooper-agent`, `service.version`. Export interval = config `interval`.

## Config reference

Static YAML and the api JSON use the same camelCase fields (Postgres `databases` / `broker_clusters`
columns + collector-only knobs). Full example: [`examples/agent.yaml`](examples/agent.yaml). Unknown YAML
fields are rejected (typo safety); unknown JSON fields are ignored (forward compatibility).

**Top level:** `interval` (default `15s`; `"15s"` or seconds as a number), `timeout` (`10s`), `databases[]`, `brokerClusters[]`.

**`databases[]`**

| Field | Source | |
|---|---|---|
| `id`, `slug`, `name` | row | `trooper.db.id` = `slug` ‖ `id` ‖ `name`; `trooper.db.name` = `name` (the dependency name `postgres · orders-db`) |
| `engine` | row | `postgres` \| `mysql` \| `redis` \| `mongodb` |
| `host`, `port` | row | port defaults per engine |
| `collector` | row | `agent` \| `direct` \| `cloud` (filter with `--modes`) |
| `credentialsRef` | row | `env:NAME` or `file:/path`. The secret is a **full DSN/URL** (used as is) or a **bare password** (DSN built from host/port/user/database) |
| `cloudResourceId`, `env` | row | passed through, not used yet |
| `dsn` | agent | inline DSN (`$VARS` expanded); wins over `credentialsRef` |
| `user` | agent | default `trooper_monitor` (Redis: none / ACL user) |
| `database` | agent | Postgres/MySQL database to connect to (Postgres default `postgres`) |
| `tls` | agent | Postgres `sslmode` (default `prefer`), MySQL `tls=` value, Redis `"true"` → `rediss://` |
| `node` | agent | `trooper.db.node` (default host) |
| `hostMetrics` | agent | override `--host-metrics` for this database |
| `interval`, `timeout`, `topQueries` (50), `slowQueryMs` (500) | agent | |
| `topQueriesSource` | agent | MongoDB: `auto` (default: `$queryStats`, else the profiler) \| `querystats` \| `profiler` \| `none` |

**`brokerClusters[]`**

| Field | Source | |
|---|---|---|
| `id`, `slug`, `name` | row | `trooper.broker.cluster` = `slug` ‖ `id` ‖ `name` |
| `kind` | row | `kafka` \| `rabbitmq` |
| `endpoint` | row | Kafka bootstrap servers (comma separated) / RabbitMQ management URL (`http(s)://host:15672`) |
| `vhost` | row | RabbitMQ, default `/` |
| `auth` | row | `{ mechanism: none\|plain\|scram-sha-256\|scram-sha-512\|iam\|oauthbearer, tls: bool }` + agent-only `caFile`, `certFile`, `keyFile` (mTLS), `insecureSkipVerify`, and the IAM / OAuth fields below |
| `credentialsRef` | row | secret is `user:password` or a bare password (user from `user`, default `trooper`); IAM: `accessKeyId:secret[:sessionToken]` or empty; OAuth: `clientId:clientSecret` (with `tokenUrl`) or a static token |
| `auth.awsRegion`, `auth.awsProfile`, `auth.roleArn` | agent | **AWS MSK IAM** (`mechanism: iam`, alias `aws-msk-iam`; TLS forced): credentials from `credentialsRef` or the default AWS chain (env, shared config/profile, web identity/IRSA, ECS, EC2 IMDS), optionally assuming `roleArn` (STS, cached). The signing region comes from the broker host name |
| `auth.tokenUrl`, `auth.scopes`, `auth.extensions` | agent | **SASL OAUTHBEARER**: OAuth 2 client-credentials grant (token cached until 30 s before expiry), or a static token when `tokenUrl` is empty |
| `nodes[]` | agent | `{ name, brokerId, metricsUrl, diskMount }` — Prometheus endpoints for `trooper.broker.node.*` (see below) |
| `nodeMetrics` | agent | RabbitMQ: `auto` (default) probes the `rabbitmq_prometheus` plugin on `<management host>:15692/metrics` when no `nodes` are listed; `off` |
| `topicMetadataEvery` | agent | Kafka retention (DescribeConfigs) refresh, default `5m` |
| `collector` | row | as for databases |
| `user`, `topicFilter`, `groupFilter` (RE2), `interval`, `timeout` | agent | |

### `GET {TT_API_URL}/api/v1/internal/collector-config` (expected from the api)

`Authorization: Bearer <api key>`; 200 with the project's targets (the key's project). Ids may be numbers.

```json
{
  "interval": 15,
  "databases": [
    { "id": 12, "slug": "orders-db", "name": "orders-db", "engine": "postgres", "host": "10.0.0.5", "port": 5432,
      "collector": "direct", "credentialsRef": "env:ORDERS_DB_DSN", "cloudResourceId": null, "env": "production" }
  ],
  "brokerClusters": [
    { "id": 3, "slug": "kafka-main", "name": "kafka-main", "kind": "kafka", "endpoint": "b-1:9096,b-2:9096", "vhost": null,
      "auth": { "mechanism": "scram-sha-512", "tls": true }, "credentialsRef": "file:/secrets/kafka-main", "collector": "agent" }
  ]
}
```

`credentialsRef` is resolved **on the agent** (`env:` / `file:`); secrets never travel through the api.
The agent diff-applies the list every poll: new targets start, changed ones restart, removed ones stop
and their series disappear.

## Monitoring users

Same SQL the Connect-database drawer shows:

```sql
-- PostgreSQL (top queries need the extension + shared_preload_libraries = 'pg_stat_statements')
CREATE ROLE trooper_monitor LOGIN PASSWORD '…';
GRANT pg_monitor TO trooper_monitor;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
-- MySQL
CREATE USER 'trooper_monitor'@'%' IDENTIFIED BY '…';
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'trooper_monitor'@'%';
GRANT SELECT ON performance_schema.* TO 'trooper_monitor'@'%';
```
```js
// MongoDB (top queries: $queryStats needs 7.0.12+/8.0 — clusterMonitor includes queryStatsRead; on
// self-managed servers enable collection with setParameter internalQueryStatsRateLimit=-1 (or a rate).
// Profiler fallback: db.setProfilingLevel(1, {slowms: 100}) + read on <db>.system.profile)
db.getSiblingDB("admin").createUser({ user: "trooper_monitor", pwd: "…", roles: ["clusterMonitor"] })
```
Kafka: `Describe` on cluster, topics and groups, `DescribeConfigs` on topics (retention). MSK IAM policy:
`kafka-cluster:Connect`, `DescribeCluster`, `DescribeTopic`, `DescribeTopicDynamicConfiguration`, `DescribeGroup`.
RabbitMQ: a user with the `monitoring` tag and no vhost rights.

## What each source fills (contracts §5)

Gauges unless noted. "—" = not available from this source (comes from the host scraper in Agent mode or
the cloud integration in Cloud mode; the writer leaves the column NaN).

| Metric | PostgreSQL | MySQL | Redis |
|---|---|---|---|
| `trooper.db.connections` / `max_connections` | client backends in `pg_stat_activity` / `max_connections` | `Threads_connected` / `@@max_connections` | `connected_clients` / `maxclients` |
| `trooper.db.ops` (/s) | Δ(`xact_commit`+`xact_rollback`) | Δ`Questions` | Δ`total_commands_processed` |
| `trooper.db.hit_ratio` | Δ`blks_hit` / Δ(hit+read) | 1 − Δ`Innodb_buffer_pool_reads` / Δ`…read_requests` | Δ`keyspace_hits` / Δ(hits+misses) |
| `trooper.db.slow` (/min) | Δcalls of statements whose interval mean ≥ `slowQueryMs` | Δ`Slow_queries` | new `SLOWLOG` entries |
| `trooper.db.latency.p95` (ms) | calls-weighted p95 of statements' interval mean latency | same, over digests | p95 of PING RTT sampled every 500 ms |
| `trooper.db.replication_lag` (ms) | replica: `now() − pg_last_xact_replay_timestamp()` (0 when caught up); primary: per standby `pg_stat_replication.replay_lag` (node = application_name) | `Seconds_Behind_Source` × 1000 | replica: `master_last_io_seconds_ago`; master: per replica `slaveN lag=` (s since last ACK) |
| `trooper.db.memory` | — | — | `used_memory / maxmemory` (or `/ total_system_memory`) |
| `trooper.db.evictions` (/min) | — | — | Δ`evicted_keys` |
| `trooper.db.cpu` / `memory` / `disk` | `--host-metrics` (gopsutil) | `--host-metrics` | `--host-metrics` (memory: Redis keeps its own `used_memory/maxmemory`) |
| `trooper.db.role` attr | `pg_is_in_recovery()` | replica status present | `role` |
| `trooper.db.query.calls` / `total_ms` / `rows` (**delta sums**) | `pg_stat_statements` Δ per `queryid` | `events_statements_summary_by_digest` Δ per digest | `INFO commandstats` Δ per command (no rows) |
| `trooper.db.query.p95_ms` | mean + 2·stddev (cumulative; pg keeps no percentiles — ≈ p97.7 for a normal distribution, deliberately conservative) | `QUANTILE_95` (8.0+, else `AVG_TIMER_WAIT`); prepared statements: interval mean | — |

MySQL top queries also cover **server-side prepared statements** (binary protocol, `PREPARE`), which never reach
`events_statements_summary_by_digest`: `performance_schema.prepared_statements_instances` deltas per (thread, statement)
are merged into the digest of their text (`STATEMENT_DIGEST`, 8.0+, with `?` → `0` so the parser accepts it — the result
equals the text-protocol digest, so the same statement run both ways is one row). Only statements still allocated at
scrape time are seen (JDBC/connector statement caches keep them; prepare-execute-close per call is missed).

**MongoDB** (`db.system=mongodb`, user with `clusterMonitor`; a single-host `mongodb://` URI connects directly so
metrics are per node):

| Metric | Source |
|---|---|
| `connections` / `max_connections` | `serverStatus.connections.current` / current + available |
| `ops` (/s) | Δ Σ `opcounters` (insert, query, update, delete, getmore, command) |
| `hit_ratio` | 1 − Δ`pages read into cache` / Δ`pages requested from the cache` (WiredTiger) |
| `evictions` (/min) | Δ(`unmodified` + `modified pages evicted`) |
| `memory` | `mem.resident` / `hostInfo.system.memSizeMB` |
| `latency.p95` (ms) | Δ `opLatencies` histograms (`serverStatus({opLatencies:{histograms:true}})`) of reads + writes, p95 interpolated linearly inside the bucket (buckets are ~2–8× wide, so this is an approximation); commands are excluded (awaitable `hello`/getMore long-polls would dominate). No histograms → ops-weighted interval mean |
| `role` / `replication_lag` | `replSetGetStatus`: secondary → primary optime − own optime; primary → per secondary (`trooper.db.node` = member host); standalone → `primary`, no lag; arbiter → `arbiter` |
| `slow` (/min) | profiler only: profiled ops with `millis ≥ slowQueryMs` |
| `query.calls` / `total_ms` / `rows` | `$queryStats`: Δ`execCount` / Δ`totalExecMicros.sum` / Δ`docsReturned.sum` per key, merged per `queryShapeHash`; hash = first 64 bits of `queryShapeHash`; text = `<command> <db>.<coll> <shape JSON>` (literals are already `?string`/`?number`). Fallback: `system.profile` entries since the last scrape in every database with profiling on (only what the profiler kept — at level 1 just ops ≥ slowms), grouped by a normalized shape (literals → `"?"`, session/cursor fields dropped), hash = FNV-1a of that text |
| `query.p95_ms` | `$queryStats`: mean + 2σ from `totalExecMicros.sumOfSquares` (cumulative); profiler: interval p95 of `millis` |

Query attributes: `trooper.db.id`, `trooper.db.query.hash` (decimal uint64: pg `queryid`; MySQL first 64 bits
of `DIGEST`; Redis FNV-1a of the command), `db.query.text` (engine-normalized — `$1` / `?` — whitespace collapsed,
≤ 1 KB). Only the top `topQueries` statements by DB time in the interval are sent. A statement that first
appears between two scrapes counts entirely in that interval.

| Metric | Kafka (kadm) | RabbitMQ (management API) |
|---|---|---|
| `trooper.broker.lag` | end − committed per (group, topic, partition); assigned but never committed: end − log start | `messages_ready` (partition `0`) |
| `trooper.broker.end_offset` / `committed_offset` | ListOffsets(latest) / OffsetFetch | — |
| `trooper.broker.produce_rate` (msg/s) | Δend offset | queue `publish` rate |
| `trooper.broker.consume_rate` (msg/s) | Δcommitted offset | `deliver_get` rate (split across groups by consumer count) |
| `trooper.broker.unacked` | — | `messages_unacknowledged` (split by consumer count) |
| `trooper.broker.group.members` (+ `trooper.broker.group.state`) | DescribeGroups | consumers per group; `Stable` |
| `trooper.broker.group.rebalances` (/h) | sliding 1 h count (see below) | — |
| `trooper.broker.oldest_age_s` | — | now − `head_message_timestamp` (classic queues, when publishers set the `timestamp` property) |
| `trooper.broker.node.memory` | `nodes[].metricsUrl` (below) | `mem_used / mem_limit` (limit = high watermark) |
| `trooper.broker.node.disk` | `nodes[].metricsUrl`, else DescribeLogDirs 1 − Σusable / Σtotal per broker (Kafka 3.3+; node `broker-<id>` or the `nodes[]` name with that `brokerId`) | `disk_free_limit / disk_free` (proximity to the free-disk alarm, 1 = publishers blocked; the API has no volume total) |
| `trooper.broker.node.cpu` / `net_in` (MB/s) | `nodes[].metricsUrl` | Prometheus (plugin or node exporter) |
| `trooper.broker.node.fd` / `.proc` (custom, 0..1) | — | `fd_used / fd_total`, `proc_used / proc_total` (`/api/nodes`) |

Topic / queue metadata (`broker_topics_1m`, attrs `trooper.broker.cluster`, `messaging.system`, `messaging.destination.name`):

| `trooper.broker.topic.*` | Kafka | RabbitMQ (per queue) |
|---|---|---|
| `partitions` / `replication` | metadata (replication = max replica count) | `1` / quorum or stream `members` (classic `1`) |
| `retention_ms` | DescribeConfigs `retention.ms` (`-1` = unlimited), every `topicMetadataEvery` | `x-message-ttl` argument, else policy `message-ttl`, else `-1` |
| `size_bytes` | DescribeLogDirs, per partition the largest replica (one copy, not × RF) | `message_bytes` (ready + unacked bodies) |
| `in_rate` (msg/s) | Σ partitions Δend offset / s | `publish` rate |
| `avg_msg_bytes` | Δsize / Δend offsets over the interval; when there is no usable delta (first scrape, no traffic, segments deleted by retention) size / retained messages (end − start). **No record is fetched**: these are on-disk bytes, i.e. after producer compression and including record-batch overhead | `message_bytes / messages` |
| `bytes_in_rate` (byte/s) | Δsize / s (else in_rate × avg) | in_rate × avg |

**Broker node metrics (`nodes[].metricsUrl`)** — any Prometheus text endpoint; first match wins:

| | node exporter | Kafka JMX exporter | Redpanda `/public_metrics` | `rabbitmq_prometheus` (:15692) |
|---|---|---|---|---|
| cpu | 1 − Δ(idle+iowait)/Δall `node_cpu_seconds_total` | `java_lang_OperatingSystem_CpuLoad` (or `SystemCpuLoad`), else Δ`process_cpu_seconds_total` / processors | Δ`redpanda_cpu_busy_seconds_total` / s / shards | scheduler utilisation from `erlang_vm_msacc_*` (only with microstate accounting enabled) |
| memory | 1 − MemAvailable/MemTotal | 1 − Free/TotalMemorySize | allocated / (allocated + free) | resident / limit |
| disk | 1 − avail/size on `diskMount` (default `/`) | — (log dirs) | 1 − free/total | limit / available |
| net_in (MB/s) | Δ`node_network_receive_bytes_total` (not `lo`) | Δ`kafka_server_BrokerTopicMetrics_BytesIn…` (aggregate series, else Σ topics) | Δ`redpanda_kafka_request_bytes_total{redpanda_request="produce"}` | Δ`erlang_vm_statistics_bytes_received_total` |

Counter-based values need two scrapes. RabbitMQ: without `nodes`, `nodeMetrics: auto` probes the plugin on the management host
(that node only; re-probed every 10 min after a failure); the node name comes from `rabbitmq_identity_info`.

* Topics/queues nobody reads are reported with `messaging.consumer.group.name=""` (end offset + produce rate) so the UI can warn.
* **Kafka rebalances:** DescribeGroups has no generation id, so a generation is the sorted member-id set seen while
  the group is settled (Stable/Empty). A change, or any `PreparingRebalance`/`CompletingRebalance` observation in
  between, counts one rebalance. Rebalances shorter than the scrape interval with identical member ids are missed.
* **RabbitMQ group = consumer tag prefix:** trailing per-instance tokens are stripped (uuid, numbers, hex ids,
  tokens with ≥ 2 digits such as k8s pod suffixes): `billing-worker-5d8f7b9c4-x2k9p` → `billing-worker`,
  `mailer_3` → `mailer`, `worker2` → `worker2`. Server-generated tags (`amq.ctag-…`) and all-random tags → queue name.
  SDKs should use `<process>-<instance>` consumer tags.
* Kafka internal topics and topics starting with `_` are skipped.
* **KIP-848 consumer groups** (new protocol, Kafka 4.0+): when a broker advertises `ConsumerGroupDescribe` (API 69,
  checked every 10 min) groups are described with it; classic groups answer it with an error and fall back to
  DescribeGroups (ListGroups' type filter needs 3.8+ and is silently dropped by older brokers, so it is not relied on).
  State is title-cased (`Stable`, `Assigning`, `Reconciling`, `Empty`); a rebalance = a group epoch bump (member
  join/leave/subscription change), so unlike classic groups no rebalance is missed. Share/streams groups are not reported.

## Deferred / not covered

* Kafka share groups (KIP-932) and streams groups; Kerberos (GSSAPI).
* MongoDB sharded clusters via `mongos` (point the agent at each shard's mongod instead); `$queryStats` on
  self-managed servers needs `internalQueryStatsRateLimit` set (0 = off by default).
* Cloud mode (CloudWatch / RDS / MSK metrics) is a separate integration; this agent only does SQL/protocol scraping for `cloud` targets that have credentials.

## Helm

Chart: [`deploy/helm/trooper-agent`](deploy/helm/trooper-agent) (values `dsn`, `apiUrl`, `apiKey`, `targets`,
`integrations.<engine>.*`, `hostMetrics`, `kind: Deployment|DaemonSet`). The UI snippet works as is plus the DSN:

```bash
helm upgrade --install trooper-agent trooper/agent \
  --set dsn=https://<ingest key>@ingest.tasktrooper.io/<project> \
  --set integrations.postgres.enabled=true \
  --set integrations.postgres.address=localhost:5432 \
  --set integrations.postgres.user=trooper_monitor \
  --set integrations.postgres.passwordSecret.name=orders-db-monitor   # Secret key "password"
```

`integrations.mongodb.*` renders a MongoDB target (`topQueriesSource` too). For MSK IAM with IRSA set
`serviceAccount.annotations."eks\.amazonaws\.com/role-arn"`; broker `nodes[]` go into `targets.brokerClusters`.

`kind=DaemonSet` + `hostMetrics=true` mounts `/proc`, `/sys`, `/` read-only so CPU/memory/disk are the node's.

## Docker

```bash
docker build -f services/collector/Dockerfile -t trooper-agent services/   # context = services/ (needs ../pkg)
```

## Tests

```bash
make test                 # unit: rate/delta math, hit ratio, rebalance counting, DSN + config parsing,
                          # credentials, consumer-tag mapping, fake RabbitMQ API, agent → in-process OTLP receiver
make itest-up             # ttcol-postgres (pg_stat_statements), ttcol-mysql, ttcol-redis, ttcol-rabbitmq (+ :15692),
                          # ttcol-mongo (mongo:8 single-node replica set), ttcol-kafka4 (Apache Kafka 4.1, KIP-848)
make itest                # -tags integration: every scraper against the real servers + Redpanda (localhost:19092,
                          # /public_metrics on :9644, ttcol-* topic/group), and `trooper-agent --once` end to end
                          # against an OTLP test server (asserts db, query, lag, topic.*, oldest_age_s, node.* names)
make itest-down
```
Integration addresses can be overridden with `TTCOL_PG_ADMIN`, `TTCOL_MYSQL_ADMIN`, `TTCOL_REDIS`, `TTCOL_RABBIT_MGMT`,
`TTCOL_RABBIT_AMQP`, `TTCOL_RABBIT_PROM`, `TTCOL_KAFKA`, `TTCOL_KAFKA_METRICS`, `TTCOL_KAFKA4`, `TTCOL_MONGO` (see `internal/itest`).
