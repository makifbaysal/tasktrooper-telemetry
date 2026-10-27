# trooper-cloud — cloud integrations (ADR-012)

`services/integrations` (`cmd/trooper-cloud`) pulls infrastructure data that only exists in cloud provider APIs
(AWS, Google Cloud, Azure, Neon, Vercel) into TaskTrooper:

- **Discovery** (every 10 min) upserts `databases` / `broker_clusters` rows (contracts §10) and counts resources.
- **Metrics** (every 60 s) are sent to ingest-gw as OTLP/HTTP gauges with the **same names as trooper-agent**
  (contracts §5), so the writer, api and alerting treat cloud resources like any other DB / broker.
- **Vercel deployments** become `deploy_markers`.
- **Serverless functions** (AWS Lambda, Azure Function Apps) become `serverless_functions` rows; their metrics are
  `trooper.faas.*` gauges (ClickHouse `faas_1m`, contracts §21). Azure apps with Application Insights also get one row
  per function with per-function metrics (contracts §22).

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
TT_INTERNAL_TOKEN=dev-internal GOWORK=off go run ./cmd/trooper-cloud     # ingest-gw needs the same TT_INTERNAL_TOKEN
TT_CLOUD_INGEST_KEYS=1:tt_dev_ingest_key GOWORK=off go run ./cmd/trooper-cloud   # legacy: per-project ingest keys
GOWORK=off go run ./cmd/trooper-cloud --once 12   # discovery + one metrics step for integration 12, then exit
docker build -f services/integrations/Dockerfile -t trooper-cloud services/   # from the repo root
```

| Env | Default | |
|---|---|---|
| `TT_POSTGRES_URL` | `postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable` | integrations, secrets, discovered rows |
| `TT_DSN` | `http://tt_dev_ingest_key@localhost:4318/1` | ingest-gw URL; its key is used for its own project |
| `TT_INTERNAL_TOKEN` | — | internal service credential shared with ingest-gw; when set, metrics go to any integration project with `x-trooper-internal` + `x-trooper-project` (see *Auth* below) |
| `TT_CLOUD_INGEST_KEYS` | — | `projectId:ingestKey,...` — fallback key per project (used when `TT_INTERNAL_TOKEN` is unset or ingest-gw answers 401 to it), and for Vercel `createDrain` |
| `TT_SECRETS_KEY` | dev key + warning | base64 32-byte AES key for `secret:<id>` refs (`services/pkg/secrets`) |
| `TT_PUBLIC_INGEST_URL` | — | public ingest-gw URL, only for `vercel.config.createDrain` |
| `TT_CLOUD_RELOAD_INTERVAL` / `TT_CLOUD_DISCOVERY_INTERVAL` / `TT_CLOUD_METRICS_INTERVAL` | `30s` / `10m` / `60s` | |
| `TT_LOG_LEVEL` | `info` | |
| `TT_CLOUD_AWS_ENDPOINT`, `TT_CLOUD_GCP_ENDPOINT`, `TT_CLOUD_NEON_URL`, `TT_CLOUD_VERCEL_URL` | — | **testing only**: point providers at fakes (AWS: every service + static creds; GCP: `<base>/sql`, `/redis`, `/pubsub`, `/mon`, no auth) |
| `TT_AZURE_LOGIN_URL` / `TT_AZURE_ARM_URL` | `https://login.microsoftonline.com` / `https://management.azure.com` | Entra ID token endpoint base / ARM base (tests: `internal/fakes` Azure fake; the token scope stays `https://management.azure.com/.default`) |

**Auth to ingest-gw.** Metrics must land in the integration's project. With `TT_INTERNAL_TOKEN` set (same value as
ingest-gw's), every request carries `x-trooper-internal: <token>` + `x-trooper-project: <integration project id>`
(contracts §16); ingest-gw resolves the org from Postgres, so no per-project key is needed and the
`status=error` "no ingest key for project" path never triggers. If ingest-gw answers 401 (token unset/different there)
and the project has a key in `TT_CLOUD_INGEST_KEYS` (or `TT_DSN`'s project), the batch is retried with that key.
Without the token, each project needs an ingest key (keys are stored hashed, trooper-cloud cannot look them up);
a project without one gets `status=error` ("no ingest key for project"). Vercel `createDrain` always needs a real
ingest key because the drain URL is public.

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

`config`: `{ "roleArn", "externalId", "regions": ["eu-central-1"], "services": ["rds","elasticache","msk","sqs","lambda"],
"sqsConsumers": {"<queue>": "<group>"}, "instanceMemoryGb": {"db.z1d.large": 16}, "maxQueues": 1000, "maxFunctions": 1000, "env": "production" }`

`services` empty or missing = every service (incl. `lambda`). An explicit list is honoured as is: existing integrations
with `["rds","sqs"]` do not get Lambda until `lambda` is added (the UI pre-selects it for new integrations).

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
  "lambda:ListFunctions", "lambda:ListTags", "lambda:ListEventSourceMappings",
  "lambda:GetFunctionConcurrency", "lambda:GetPolicy", "lambda:ListFunctionUrlConfigs",
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

### AWS Lambda (`lambda`)

Discovery (every 10 min, per region): `ListFunctions` (paginated, `$LATEST`, ≤ `maxFunctions` per region) → one
`serverless_functions` row per function (contracts §21, `deploy/postgres/011_serverless.sql`):

| Column | Source |
|---|---|
| `slug` | `aws-<region>-<name>` (lower-case, non `[a-z0-9]` → `-`) |
| `name`, `region`, `runtime`, `memory_mb`, `timeout_s`, `arch` | `FunctionName`, region, `Runtime` (`container` for image functions), `MemorySize`, `Timeout`, `Architectures[0]` |
| `service` | tag `tasktrooper.service`, else the function name — must equal the spans' `service.name` |
| `triggers` | event source mappings (one `ListEventSourceMappings` per region; disabled ones skipped): `sqs:<queue>`, `kinesis:<stream>`, `dynamodb:<table>`, `kafka:<cluster>` (MSK) / `kafka` (self-managed), `mq:<broker>`, `docdb:<cluster>`; resource policy (`GetPolicy`): API Gateway → `api`, EventBridge rule → `schedule:<rule>` (rule name contains schedule/cron/rate) or `eventbridge:<rule>`, `scheduler` → `schedule`, S3 → `s3:<bucket>`, SNS → `sns:<topic>`, ALB → `alb`; Function URL (`ListFunctionUrlConfigs`) → `http` |
| `url` | Function URL, if any |
| `meta` | `arn`, `region`, `tags`, `handler`, `packageType`, `state`, `lastModified`, `reservedConcurrency` (`GetFunctionConcurrency`), `otel: true` when an OTel/ADOT layer or `AWS_LAMBDA_EXEC_WRAPPER` is present (env var **names** only — values are never read into meta) |

`ListTags` failures are reported in `integrations.error`; `GetFunctionConcurrency`, `GetPolicy` and
`ListFunctionUrlConfigs` are optional — on `AccessDenied` they are skipped for the rest of the run (no `api` / `http`
triggers then). Functions not found any more get `missing = true` (rows are never deleted), cleared when they come back.
Rows with `integration_id` NULL or owned by another integration are never touched (no metrics are sent for them).

Metrics (every 60 s): `GetMetricData` on `AWS/Lambda` with dimension `FunctionName`, batched with the other services
(≤ 500 queries per call), period 60 s over the last 10 min. Unlike RDS/SQS, **every datapoint of the window** is sent
(CloudWatch Lambda sums arrive late and minutes without invocations have no point; `faas_1m` keeps the max per minute,
so resends are idempotent). Gauges carry `trooper.faas.id` = slug and `faas.name`; resource `cloud.provider=aws`,
`cloud.region`.

| CloudWatch `AWS/Lambda` (stat) | trooper metric | `faas_1m` column |
|---|---|---|
| `Invocations` (Sum) | `trooper.faas.invocations` | `invocations` |
| `Errors` (Sum) | `trooper.faas.errors` | `errors` |
| `Throttles` (Sum) | `trooper.faas.throttles` | `throttles` |
| `Duration` (Average / Maximum / p95 / p99), ms | `trooper.faas.duration.avg` / `.max` / `.p95` / `.p99` | `duration_avg` / `duration_max` / `duration_p95` / `duration_p99` |
| `ConcurrentExecutions` (Maximum) | `trooper.faas.concurrency` | `concurrency` |
| `IteratorAge` (Maximum), ms — only for functions with a stream trigger (Kinesis, DynamoDB, Kafka, DocumentDB) | `trooper.faas.iterator_age_ms` | `iterator_age_ms` |

8 queries per function (+1 with a stream trigger): CloudWatch bills `GetMetricData` per metric requested, so 100
functions ≈ 800 metrics/min. Cold starts are not in CloudWatch: they come from spans (`faas.coldstart=true`) — use
`@trooper/node` `wrapLambda` or the OTel/ADOT Lambda layer (`.ai/sdk.md` § Serverless).

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

## Azure (Function Apps)

`provider = 'azure'`. `config`: `{ "tenantId": "<directory id>", "clientId": "<app id>", "subscriptionIds": ["<sub>"],
"resourceGroups": ["rg-a"], "env": "production", "maxApps": 500, "readAppSettings": false }` — `subscriptionIds` empty = every enabled
subscription the principal can list; `resourceGroups` optional filter. Credentials: the service principal's **client
secret** behind `credentials_ref` (plain sealed secret; JSON `{"clientSecret": "..."}` is accepted too).

Setup: register an app (Entra ID → App registrations), create a client secret, and assign it **Reader** and
**Monitoring Reader** on each subscription (or resource group):

```bash
az ad sp create-for-rbac --name tasktrooper-reader --role Reader --scopes /subscriptions/<sub>
az role assignment create --assignee <appId> --role "Monitoring Reader" --scope /subscriptions/<sub>
# tenantId = "tenant", clientId = "appId", secret = "password" of the output
```

Token: `POST {TT_AZURE_LOGIN_URL}/{tenantId}/oauth2/v2.0/token` (client credentials, scope
`https://management.azure.com/.default`), cached until 2 min before expiry. AAD errors are reported with their code and
first description line (never the secret).

Discovery (every 10 min): `GET /subscriptions` (if needed) → `GET /subscriptions/{sub}[/resourceGroups/{rg}]/providers/Microsoft.Web/sites`
(api-version 2023-12-01, `nextLink` followed on the same host only) → sites whose `kind` contains `functionapp`
(Logic Apps `workflowapp` skipped) → `GET {site}/functions`. One `serverless_functions` row **per Function App** (Azure
Monitor metrics are app-level):

| Column | Source |
|---|---|
| `slug`, `name`, `app` | `azure-<app>`, app name, app name |
| `region` | `location` normalized (`West Europe` → `westeurope`) |
| `runtime` | `functionAppConfig.runtime` (Flex), else `siteConfig.linuxFxVersion`, else `GET {site}/config/web` (`python 3.11`, `node 20`, `dotnet-isolated 8.0`, `container`) |
| `memory_mb` | Flex `instanceMemoryMB` (else NULL); `timeout_s`, `arch` not available |
| `triggers` | trigger bindings of the enabled functions: `http`, `schedule`, `queue:<q>`, `servicebus:<queue or topic>`, `eventhub:<hub>`, `blob`, `cosmosdb:<container>`, `eventgrid`, `kafka:<topic>`, `rabbitmq:<q>` (app-setting references `%X%` drop the name) |
| `service` | app setting `TASKTROOPER_SERVICE` / `OTEL_SERVICE_NAME` (only with `readAppSettings`), else tag `tasktrooper.service`, else the app name |
| `url` | `https://<defaultHostName>` |
| `meta` | `id`, `subscriptionId`, `resourceGroup`, `kind`, `state`, `os`, `plan` (`Consumption` = SKU Dynamic, `Flex` = FlexConsumption, `Premium` = ElasticPremium, else `Dedicated`), `tags`, `functions: [{name, trigger, disabled?}]`, `appInsights: "connected" \| "unavailable"`, `appInsightsComponent` (component name, when connected) |

Metrics (every 60 s, one request per app): `GET {site}/providers/microsoft.insights/metrics` (api-version
2023-10-01, `interval=PT1M`, last 10 min, every datapoint sent):

| Azure Monitor metric (aggregation) | trooper metric | conversion |
|---|---|---|
| `FunctionExecutionCount` (Total); Flex: `OnDemandFunctionExecutionCount` + `AlwaysReadyFunctionExecutionCount` | `trooper.faas.invocations` | Flex series summed per minute |
| `FunctionExecutionUnits` (Total); Flex: `OnDemand…` + `AlwaysReadyFunctionExecutionUnits` | `trooper.faas.execution_units` | MB·ms |
| `Http5xx` (Total) | `trooper.faas.errors` | |
| `HttpResponseTime` (Average / Maximum) | `trooper.faas.duration.avg` / `.max` | s × 1000 → ms |
| `MemoryWorkingSet` (Average) | `trooper.faas.memory_bytes` | bytes |

No throttles, p95/p99, concurrency or iterator age on Azure. A metric the app rejects (HTTP 400, e.g. on Flex or Linux
Consumption) is retried alone and then skipped for that app until restart. ARM read quota: ~12 000 requests/h per
subscription and principal → roughly 150 apps at one metrics call per minute (two with App Insights, see below).

### Per-function metrics (Application Insights)

Azure Monitor only has app-level metrics. When a Function App has an Application Insights component, every function
also gets its own row and metrics (contracts §22). **No extra role:** Reader covers both the component listing and the
query (`microsoft.insights/components/query/read`).

- **Discovery:** `GET /subscriptions/{sub}[/resourceGroups/{rg}]/providers/Microsoft.Insights/components`
  (api-version 2020-02-02). A component belongs to the app whose resource id is in its `hidden-link:<siteId>` tag
  (compared case-insensitively — the portal writes `resourcegroups` lower-case); fallback: a component with the app's
  name in the same subscription (portal default). A 403/404 on the listing is not an error. With a `resourceGroups`
  filter only those groups are searched, so keep the component in a listed group.
- **Query:** ARM proxy `POST {componentId}/query?api-version=2018-04-20`, body `{"query": KQL, "timespan": "PT15M"}`,
  same ARM token. Answer: `{"tables": [{"name": "PrimaryResult", "columns": [{"name", "type"}], "rows": [[...]]}]}`
  (columns are matched by name). KQL:

  ```kusto
  requests | where timestamp > ago(15m)
  | summarize invocations=count(), errors=countif(success == false), avg=avg(duration),
      p95=percentile(duration,95), p99=percentile(duration,99), max=max(duration)
    by bin(timestamp, 1m), operation_Name, cloud_RoleName
  ```

  At most **one query per component per metrics tick** (≥ 50 s apart; a call still in flight or inside its interval is
  skipped). Discovery's query is handed over to the next metrics step instead of querying twice. The 15-minute lookback
  re-sends late App Insights data every minute (idempotent: `faas_1m` keeps the max per minute); the current, incomplete
  minute is dropped. `operation_Name` is matched to the function name case-insensitively; other operations are ignored.
- **Rows:** one per function listed by `{site}/functions` (also disabled ones and those without data, so the UI lists
  every function): `slug = azure-<app>-<function>`, `name` = function, `app` = app name, `region` / `runtime` /
  `memory_mb` from the app, `triggers` = `[<trigger of the function>]`, `url` = `https://<host>/api/<function>` for
  HTTP triggers, `meta = {parent: <app slug>, source: "appinsights", trigger, disabled?, language?, plan, appId,
  subscriptionId, resourceGroup}`. `service` = app setting (with `readAppSettings`) > most frequent `cloud_RoleName`
  of the function (else of the app; remembered while the function is quiet; ignored if it only equals the app name —
  the Functions default — and the app has a `tasktrooper.service` tag) > tag > app name.
- **Metrics** (`trooper.faas.id` = function slug, `faas.name` = function; several role names of one minute are merged):

  | KQL column | trooper metric |
  |---|---|
  | `invocations` | `trooper.faas.invocations` |
  | `errors` (`success == false`) | `trooper.faas.errors` |
  | `avg` / `max` / `p95` / `p99` (`duration`, ms) | `trooper.faas.duration.avg` / `.max` / `.p95` / `.p99` |

  Cold starts are not derived from App Insights (they come from OTel spans, `faas.coldstart`).
- **Without App Insights** (no component, or the query answers 403/404) only the app row is written with
  `meta.appInsights = "unavailable"`; function rows written earlier get `missing = true`. A transient query error
  (429/5xx) keeps the function rows and is reported in the integration status.

### `readAppSettings` (optional)

`"readAppSettings": true` → `POST {site}/config/appsettings/list?api-version=2023-12-01` at discovery; only
`TASKTROOPER_SERVICE` (else `OTEL_SERVICE_NAME`) is read and overrides `service` of the app row and its function rows —
no other setting is kept or logged. Listing settings is a write-level action (`Microsoft.Web/sites/config/list/action`,
e.g. **Website Contributor** on the apps); without it the call answers 403 and is skipped silently (one info log per app).

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
GOWORK=off go test -tags integration ./internal/store/       # live Postgres (projects 9301 + 9701 for serverless_functions, cleaned up)
GOWORK=off go test -tags e2e -count=1 -v ./e2e/              # fake AWS+Vercel → real ingest-gw/writer → ClickHouse (project 9301)
```

- AWS fakes speak the real wire protocols (`internal/fakes`): awsquery XML (RDS, ElastiCache), awsJson (SQS), restJson
  (MSK, Lambda) and rpc-v2-CBOR (CloudWatch `GetMetricData` / `ListMetrics`); GCP / Neon / Vercel fakes serve recorded-shape JSON.
- `fakes.NewAzure` is an Entra ID token endpoint + ARM (sites, functions, Azure Monitor metrics; one Flex app rejects
  `MemoryWorkingSet` to exercise the per-metric fallback; App Insights components with `hidden-link` tags and the
  `/query` proxy answering `fakes.AIRows` — two role names, an incomplete minute, an unknown operation; `QueryStatus`
  forces 403/429; `appsettings/list` answers one app and 403s the other). Run trooper-cloud against it with `TT_AZURE_LOGIN_URL` /
  `TT_AZURE_ARM_URL`.
- The e2e test asserts `db_1m`, `broker_lag_1m` (incl. `oldest_age_s`), `broker_nodes_1m` rows and Postgres
  `deploy_markers` / `databases` / `broker_clusters` / `integrations.status`, then deletes project 9301 everywhere. Its AWS integration lists
  `services` explicitly (no Lambda), so it does not depend on `faas_1m`.
