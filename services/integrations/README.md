# trooper-cloud — cloud integrations (ADR-012)

`services/integrations` (`cmd/trooper-cloud`) pulls infrastructure data that only exists in cloud provider APIs
(AWS, Google Cloud, Neon, Vercel) into TaskTrooper:

- **Discovery** (every 10 min) upserts `databases` / `broker_clusters` rows (contracts §10) and counts resources.
- **Metrics** (every 60 s) are sent to ingest-gw as OTLP/HTTP gauges with the **same names as trooper-agent**
  (contracts §5), so the writer, api and alerting treat cloud resources like any other DB / broker.
- **Vercel deployments** become `deploy_markers`.

```
Postgres integrations ──(30 s reload)──► runner ── per integration goroutine (isolated, panic-safe)
                                            │  discovery 10 min ─► databases / broker_clusters (collector='cloud')
                                            │  metrics   60 s   ─► OTLP gauges ─► ingest-gw /v1/metrics (project ingest key)
                                            └─ status / error / last_sync_at / resources ─► integrations
```

## Run

```bash
make infra migrate                      # repo root
make run-writer & make run-gw &         # ingest pipeline
cd services/integrations
TT_CLOUD_INGEST_KEYS=1:tt_dev_ingest_key GOWORK=off go run ./cmd/trooper-cloud
GOWORK=off go run ./cmd/trooper-cloud --once 12   # discovery + one metrics step for integration 12, then exit
docker build -f services/integrations/Dockerfile -t trooper-cloud services/   # from the repo root
```

| Env | Default | |
|---|---|---|
| `TT_POSTGRES_URL` | `postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable` | integrations, secrets, discovered rows |
| `TT_DSN` | `http://tt_dev_ingest_key@localhost:4318/1` | ingest-gw URL; its key is used for its own project |
| `TT_CLOUD_INGEST_KEYS` | — | `projectId:ingestKey,...` — key used to send each project's metrics (see *Auth* below) |
| `TT_SECRETS_KEY` | dev key + warning | base64 32-byte AES key for `secret:<id>` refs (`services/pkg/secrets`) |
| `TT_PUBLIC_INGEST_URL` | — | public ingest-gw URL, only for `vercel.config.createDrain` |
| `TT_CLOUD_RELOAD_INTERVAL` / `TT_CLOUD_DISCOVERY_INTERVAL` / `TT_CLOUD_METRICS_INTERVAL` | `30s` / `10m` / `60s` | |
| `TT_LOG_LEVEL` | `info` | |
| `TT_CLOUD_AWS_ENDPOINT`, `TT_CLOUD_GCP_ENDPOINT`, `TT_CLOUD_NEON_URL`, `TT_CLOUD_VERCEL_URL` | — | **testing only**: point providers at fakes (AWS: every service + static creds; GCP: `<base>/sql`, `/redis`, `/pubsub`, `/mon`, no auth) |

**Auth to ingest-gw.** Metrics must land in the integration's project, and ingest keys are stored hashed, so
trooper-cloud cannot look them up. For now each project needs an ingest key in `TT_CLOUD_INGEST_KEYS`
(the project in `TT_DSN` is covered automatically). A project without a key gets `status=error`
("no ingest key for project"). **Follow-up:** an internal service credential on ingest-gw that accepts a
`x-trooper-project` header from trusted services.

## Common behaviour

- `credentials_ref`: `secret:<id>` (Postgres `secrets`, AES-256-GCM, AAD = project id), `env:NAME`, `file:/path`.
  Plaintext is never logged or written to `integrations.error`.
- Slug = `<provider>-<name>` (lower-case, non `[a-z0-9]` → `-`). Rows whose `integration_id` is NULL (added by hand)
  or belongs to another integration are **never updated**; their slug is skipped and no metrics are sent for it.
- Resources not found any more get `meta.missing = true` (rows are never deleted); the flag clears when they reappear.
- **Sync now** (`POST /api/v1/integrations/{id}/sync`): the api sets `config.syncRequestedAt` (epoch ms or ISO-8601). On the next 30 s reload, if it is newer than `last_sync_at`, the running worker immediately runs discovery + metrics + deploy sync (once per request; the key is ignored for change detection, so no restart).
- Vercel: `credentials_ref` → the access token (plain sealed secret), `config.drainSecretRef` → the drain signing secret.
- One failing integration never affects others; a partial failure (one region / service) keeps the rest flowing and
  sets `status=error` with the joined message.
- HTTP: timeouts, retries on 429/5xx with `Retry-After` + exponential backoff (REST providers); AWS SDK standard
  retryer (5 attempts); context cancellation everywhere.
- Every sample carries the provider's **datapoint timestamp** (last complete minute), so late reads land in the right
  minute; collector tables use max/min so resends are idempotent.
- Resource attributes: `service.name=trooper-cloud`, `service.version`, `trooper.integration.id`,
  `trooper.integration.name`, `cloud.provider`, `cloud.region`.
- Queue-style lag (SQS, Pub/Sub) and aggregated Kafka lag (MSK `SumOffsetLag` per group+topic) use partition `"0"`
  (`broker_lag_1m.partition` is `UInt32`, so `-1` is not representable).
- `messaging.system`: `kafka` (MSK), `aws_sqs`, `gcp_pubsub`.

## AWS

`config`: `{ "roleArn", "externalId", "regions": ["eu-central-1"], "services": ["rds","elasticache","msk","sqs"],
"sqsConsumers": {"<queue>": "<group>"}, "instanceMemoryGb": {"db.z1d.large": 16}, "maxQueues": 1000, "env": "production" }`

Credentials: **AssumeRole** with `roleArn` + `externalId` (recommended; base credentials from the default chain —
instance profile / IRSA / env) or an **access key** `credentials_ref` → `{"accessKeyId","secretAccessKey"[,"sessionToken"]}`
(if `roleArn` is also set the key is used to assume the role).

Trust policy (role in the customer account):

```json
{ "Version": "2012-10-17", "Statement": [{ "Effect": "Allow",
  "Principal": { "AWS": "arn:aws:iam::<TASKTROOPER_ACCOUNT_ID>:root" }, "Action": "sts:AssumeRole",
  "Condition": { "StringEquals": { "sts:ExternalId": "<externalId>" } } }] }
```

Permissions policy (read-only):

```json
{ "Version": "2012-10-17", "Statement": [{ "Effect": "Allow", "Resource": "*", "Action": [
  "rds:DescribeDBInstances", "rds:DescribeDBClusters",
  "elasticache:DescribeReplicationGroups",
  "kafka:ListClustersV2", "kafka:ListNodes", "kafka:GetBootstrapBrokers",
  "sqs:ListQueues", "sqs:GetQueueAttributes",
  "cloudwatch:GetMetricData", "cloudwatch:ListMetrics" ] }] }
```

Discovery: RDS/Aurora instances (postgres, mysql, mariadb; Oracle/SQL Server skipped) → one `databases` row per
instance (`aws-<instance id>`, role from read-replica source / Aurora writer flag); ElastiCache replication groups
(redis/valkey; memcached skipped) → one row per group, nodes = member clusters; MSK clusters (`ListClustersV2`,
brokers via `ListNodes`, endpoint = bootstrap brokers, consumer group/topic pairs via `ListMetrics SumOffsetLag`) →
`broker_clusters` kind `kafka`; SQS → one `broker_clusters` row **per region** (`aws-sqs-<region>`, kind `sqs`,
`meta.queues` with retention / visibility / redrive).

Metrics: `GetMetricData`, ≤ 500 queries per call, period 60 s over the last 10 min, newest datapoint per series.

| CloudWatch (stat) | trooper metric | conversion |
|---|---|---|
| RDS `CPUUtilization` (Avg) | `trooper.db.cpu` | ÷100 |
| RDS `FreeableMemory` (Avg) | `trooper.db.memory` | 1 − free ÷ class memory (table below) |
| RDS `FreeStorageSpace` (Avg) + `AllocatedStorage` | `trooper.db.disk` | 1 − free ÷ allocated (not Aurora) |
| RDS `DatabaseConnections` | `trooper.db.connections` | |
| RDS `ReadIOPS` + `WriteIOPS` | `trooper.db.ops` | sum, /s |
| RDS `ReplicaLag` / `AuroraReplicaLag` | `trooper.db.replication_lag` | s×1000 / ms |
| ElastiCache `EngineCPUUtilization` | `trooper.db.cpu` | ÷100 |
| ElastiCache `DatabaseMemoryUsagePercentage` | `trooper.db.memory` | ÷100 |
| ElastiCache `CurrConnections` | `trooper.db.connections` | |
| ElastiCache `Evictions` (Sum) | `trooper.db.evictions` | per minute |
| ElastiCache `CacheHitRate` | `trooper.db.hit_ratio` | ÷100 |
| ElastiCache `GetTypeCmds` + `SetTypeCmds` (Sum) | `trooper.db.ops` | ÷60 |
| MSK `CpuUser` + `CpuSystem` (per broker) | `trooper.broker.node.cpu` | ÷100 |
| MSK `MemoryUsed` / (`MemoryUsed` + `MemoryFree`) | `trooper.broker.node.memory` | |
| MSK `KafkaDataLogsDiskUsed` | `trooper.broker.node.disk` | ÷100 |
| MSK `BytesInPerSec` | `trooper.broker.node.net_in` | ÷10⁶ → MB/s |
| MSK `SumOffsetLag` (Max, group + topic) | `trooper.broker.lag` | partition `0` |
| SQS `ApproximateNumberOfMessagesVisible` (Max) | `trooper.broker.lag` | group = queue (or `sqsConsumers`) |
| SQS `ApproximateNumberOfMessagesNotVisible` (Max) | `trooper.broker.unacked` | |
| SQS `ApproximateAgeOfOldestMessage` (Max) | `trooper.broker.oldest_age_s` | |
| SQS `NumberOfMessagesSent` (Sum) | `trooper.broker.produce_rate`, `trooper.broker.topic.in_rate` | ÷60 |
| SQS `NumberOfMessagesDeleted` (Sum) | `trooper.broker.consume_rate` | ÷60 |
| SQS attribute `MessageRetentionPeriod` | `trooper.broker.topic.retention_ms` | ×1000 |

RDS memory table (`trooper.db.memory`; CloudWatch has no total memory): `db.t*` micro 1 / small 2 / medium 4 /
large 8 GiB …; `db.m*` 8 GiB per `large`; `db.r*` 16 GiB per `large`; `db.x*` 32 GiB per `large`; `xlarge` = 2×,
`Nxlarge` = 2N×. Other classes (Serverless v2, `z1d`, `metal`, custom) report no memory unless set in
`config.instanceMemoryGb`.

## Google Cloud

`config`: `{ "projectIds": ["shop-prod"], "services": ["cloudsql","memorystore","pubsub"], "consumerMap": {"<subscription>": "<group>"}, "env": "production" }`

Credentials: service-account JSON behind `credentials_ref` (empty = Application Default Credentials / workload
identity; `projectIds` defaults to the key's project). Required roles: `roles/monitoring.viewer`,
`roles/cloudsql.viewer`, `roles/redis.viewer`, `roles/pubsub.viewer`. Scope `cloud-platform`.

Discovery (REST): Cloud SQL Admin `instances.list` (Postgres/MySQL; host = PRIMARY, else PRIVATE IP),
Memorystore `locations/-/instances`, Pub/Sub `subscriptions.list` → one `broker_clusters` row per GCP project
(`gcp-pubsub-<project>`, kind `pubsub`, `meta.subscriptions`); topic = the subscription's topic, group = the
subscription. With several `projectIds`, database names are prefixed with the project.

Metrics: Cloud Monitoring `timeSeries.list`, alignment 60 s over the last 5 min, newest point per resource.

| Monitoring metric (aligner) | trooper metric | conversion |
|---|---|---|
| `cloudsql…/database/cpu/utilization` | `trooper.db.cpu` | |
| `cloudsql…/database/memory/utilization` | `trooper.db.memory` | |
| `cloudsql…/database/disk/utilization` | `trooper.db.disk` | |
| `cloudsql…/database/postgresql/num_backends` (sum per instance) / `database/network/connections` (MySQL) | `trooper.db.connections` | |
| `cloudsql…/database/postgresql/transaction_count` / `database/mysql/queries` (RATE) | `trooper.db.ops` | /s |
| `cloudsql…/database/replication/replica_lag` | `trooper.db.replication_lag` | s×1000 |
| `redis…/stats/cpu_utilization` (sum of user+sys) | `trooper.db.cpu` | CPU-s/s, capped at 1 |
| `redis…/stats/memory/usage_ratio` | `trooper.db.memory` | |
| `redis…/clients/connected` | `trooper.db.connections` | |
| `redis…/stats/evicted_keys` (DELTA 60 s) | `trooper.db.evictions` | per minute |
| `redis…/stats/cache_hit_ratio` | `trooper.db.hit_ratio` | |
| `pubsub…/subscription/num_undelivered_messages` | `trooper.broker.lag` | partition `0` |
| `pubsub…/subscription/oldest_unacked_message_age` | `trooper.broker.oldest_age_s` | |
| `pubsub…/subscription/ack_message_count` (RATE) | `trooper.broker.consume_rate` | /s |
| `pubsub…/topic/send_message_operation_count` (RATE) | `trooper.broker.produce_rate`, `trooper.broker.topic.in_rate` | /s |

## Neon

`config`: `{ "orgId": "org-x", "projectIds": [], "storageLimitGb": 10, "env": "production" }` (empty `projectIds` = all
projects of the key/org). Credentials: Neon API key (`Authorization: Bearer`), API `https://console.neon.tech/api/v2`.

Discovery: projects → branches → compute endpoints; one `databases` row per endpoint, name
`<project>-<branch>` (+ `-ro-<endpoint>` for read replicas), engine postgres, port 5432, `source` `Neon · <region>`,
`instance_class` `0.25–2 CU`, `disk_gb` = branch logical size, `meta {project, projectId, branch, branchId, state, computeId, type}`.

Metrics (every 60 s, re-reads project / branches / endpoints):

| Neon API | trooper metric |
|---|---|
| branch `logical_size` ÷ project `settings.quota.logical_size_bytes` (else `storageLimitGb`) | `trooper.db.disk` |
| endpoint `current_state` | `databases.meta.state` (+ custom `trooper.cloud.neon.compute_active` 0/1) |
| endpoint `autoscaling_limit_max_cu` | custom `trooper.cloud.neon.compute_cu_max` |
| branch `logical_size` | custom `trooper.cloud.neon.logical_size_bytes` |
| project `compute_time_seconds`, `active_time_seconds`, `cpu_used_sec`, `written_data_bytes`, `data_transfer_bytes`, `synthetic_storage_size` | custom `trooper.cloud.neon.*` (period-to-date counters, `metrics` table) |

**Gaps:** Neon's public API has no CPU / memory / connection / replication-lag time series, so `trooper.db.cpu`,
`memory`, `connections`, `ops` are not reported; if no storage limit is known there is no `db_1m` row at all (only the
custom metrics). Query statistics come from trooper-agent's direct mode (`pg_stat_statements` with a Neon role).

## Vercel

`config`: `{ "teamId": "team_x", "projectMap": {"<vercel project>": "<service>"}, "createDrain": false,
"drainSecretRef": "secret:12", "backfillHours": 24 }`. Credentials: access token (scope: the team).

- Discovery: `GET /v10/projects` (counted in `resources`; no DB/broker rows).
- Every 60 s: `GET /v6/deployments?teamId&since=` → `deploy_markers` (source `vercel`, `external_id` = deployment uid,
  service = `projectMap[project] ?? project`, env = `target` or `preview`, version = git commit sha or uid, url,
  commit message / author from git meta, state `building|ready|error|canceled`). The window overlaps 1 h so
  BUILDING deploys get their final state; first run backfills `backfillHours`.
- **Drains** (logs, traces, speed insights, web analytics) are received by ingest-gw `POST /v1/drains/vercel`
  (contracts §10). Create one in the Vercel dashboard (Team → Settings → Drains → Add drain → Custom endpoint) with URL
  `https://<public ingest>/v1/drains/vercel?key=<ingest key>&integration=<integration id>`, JSON encoding, and the
  signature secret stored behind `config.drainSecretRef`; or let trooper-cloud create it via `POST /v1/drains`
  with `createDrain: true` (needs `TT_PUBLIC_INGEST_URL` and the project in `TT_CLOUD_INGEST_KEYS`). Best effort:
  Vercel's drain API body may change; manual creation is the reference path.

## Tests

```bash
GOWORK=off go test ./...                                     # unit: mappers with fixture API responses (httptest)
GOWORK=off go test -tags integration ./internal/store/       # live Postgres (project 9301, cleaned up)
GOWORK=off go test -tags e2e -count=1 -v ./e2e/              # fake AWS+Vercel → real ingest-gw/writer → ClickHouse (project 9301)
```

- AWS fakes speak the real wire protocols (`internal/fakes`): awsquery XML (RDS, ElastiCache), awsJson (SQS), restJson
  (MSK) and rpc-v2-CBOR (CloudWatch `GetMetricData` / `ListMetrics`); GCP / Neon / Vercel fakes serve recorded-shape JSON.
- The e2e test asserts `db_1m`, `broker_lag_1m` (incl. `oldest_age_s`), `broker_nodes_1m` rows and Postgres
  `deploy_markers` / `databases` / `broker_clusters` / `integrations.status`, then deletes project 9301 everywhere.
