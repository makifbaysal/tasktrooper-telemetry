# Architecture decisions (ADR)

Format: context → decision → consequence. New decisions go at the bottom, with increasing numbers.

## ADR-001 — OTLP as the wire protocol
**Context:** A protocol of our own would require writing SDKs from scratch; customers may already be using OTel.
**Decision:** Backend SDKs are a thin layer on top of the OTel SDK; ingest accepts OTLP. A lightweight custom JSON beacon for RUM.
**Consequence:** Fast language coverage; existing OTel users can come without an SDK. The browser bundle stays small (< 15 KB gz target).

## ADR-002 — ClickHouse as the telemetry store
**Context:** High write rate, columnar aggregation, log full-text, long retention.
**Decision:** Metrics, spans, logs and RUM in a single ClickHouse cluster. Not a separate Prometheus/Loki/Tempo stack.
**Consequence:** Correlation (trace ↔ log ↔ RUM) is easy with a single query language. Operational load comes down to one system.

## ADR-003 — Redpanda as the ingest buffer
**Context:** A ClickHouse slowdown must not cause ingest loss; stream alerts need an event stream.
**Decision:** Redpanda (Kafka API compatible, no JVM).
**Consequence:** Replay, backpressure, stream consumers (alerting, pattern extraction).

## ADR-004 — Backend Go, frontend React + Vite
**Context:** The ingest path needs high throughput; the team knows Go and React well.
**Decision:** All services in Go. Dashboard is a React 19 + Vite SPA (no need for SSR, it sits behind auth).
**Consequence:** Single-binary deploy, low memory. The dashboard can be hosted statically.

## ADR-005 — Charts are our own SVG components
**Context:** Observability charts come in a limited number of forms (time series, bar, histogram, heatmap, sparkline); we want full design system alignment and a small bundle.
**Decision:** Lightweight SVG components under `apps/web/src/components/charts`. If very dense data (>10k points) is needed, moving to uPlot will be considered.
**Consequence:** Full alignment with tokens, dark mode, and the hover layer is in our hands.

## ADR-006 — k6 as the load test engine
**Context:** A mature engine instead of writing our own load generator.
**Decision:** The k6 Go library embedded in the runner; declarative YAML → compiled to a k6 script.
**Consequence:** Script mode gives advanced users full power; metrics flow to us via OTLP.

## ADR-007 — LLM for interpretation, not for extraction
**Context:** Hallucination risk in code analysis.
**Decision:** The inventory is extracted deterministically with tree-sitter extractors; the LLM only produces criticality, explanations and suggestions.
**Consequence:** The inventory is reliable; the LLM can be turned off.

## ADR-008 — KPI = SLO, alerts via burn rate
**Context:** Users want to define experience goals such as "the home page should open fast"; raw threshold alerts are noisy.
**Decision:** A KPI is an SLO: target (page/endpoint/journey/app start), good-event definition, objective, window (7d/28d).
Alerts are multi-window burn rate (fast 1h+5m, slow 6h+30m) + an optional "below target" rule.
**Consequence:** Short spikes don't page; if the budget is running out it warns early. Frontend and backend KPIs share the same model.

## ADR-009 — Root cause: delta attribution on the trace graph
**Context:** An automatic answer to "is the error mine or the service I call?"; an ML model isn't explainable and there is no training data.
**Decision:** A call tree is built from `edges_1m`; each child's contribution = `calls/request × (now − baseline)`; for latency, only the
critical path is counted for parallel HTTP calls; the unexplained remainder is "own code". It descends to a leaf / own code by following
the largest share; confidence = product of the shares along the path. Evidence (deploy, migration, query fingerprint, resource) is attached to the root node.
**Consequence:** Deterministic, explainable, and "how much does it explain" can be shown at every step. It cannot distinguish correlated
drops on a shared dependency — if a sibling is ≥15% it is listed as an alternative.

## ADR-010 — Telemetry store: ClickHouse, not Cassandra (re-evaluation)
**Context:** Cassandra / ScyllaDB was proposed for high write load (target 200k spans/s + 500k logs/s in a single region).
**Evaluation:**
- Cassandra/Scylla: write throughput and linear scaling are very good, multi-DC replication is mature. But queries only go through
  the partition key; no `GROUP BY`, percentile, ad-hoc filter or full-text. Every screen of the product is aggregation (p95 per
  service/endpoint/time, top-N query, lag per group, log search). Every view would have to be precomputed into a separate table at write time; new screen =
  new table + backfill. Tombstone/compaction load with time-series + TTL (TWCS required), and disk ~5–10× since there is no columnar compression.
- ClickHouse: columnar, 10–20× compression, millions of rows/s batch insert per node, vectorized `quantilesTDigest`,
  rollup via materialized views, log search with `tokenbf_v1`, per-table TTL + S3 tiering. Horizontal scale with sharding (`Distributed`) +
  `ReplicatedMergeTree` + ClickHouse Keeper. Sentry (Snuba), PostHog, SigNoz, Uptrace and Cloudflare use it for the same job.
**Decision:** ADR-002 stands: telemetry in ClickHouse. For high load: Redpanda buffer (ingest independent of ClickHouse), large
batches in the writer (≥50k rows / 1 s), `async_insert` off (we do the batching), shard key `cityHash64(org_id, project_id, service)`,
hot/cold tiering (7 days NVMe → S3). Metadata in Postgres, rate limit/cache in Redis, blobs (sourcemap, crash dump, replay) in S3.
**Consequence:** Cassandra is not added. If a very high QPS key-value need (e.g. trace-by-id cache, session state) comes up later,
ScyllaDB will be considered; for now trace-by-id is solved in ClickHouse with the `trace_index` table.

## ADR-011 — SDK strategy: OpenTelemetry distribution + RUM JSON
**Context:** Go (gin, fiber), Java (Spring, Quarkus), Kotlin, .NET backends; Android, iOS, Flutter, React Native mobile; web JS/TS.
**Decision:**
- **Backend SDKs = an OTel distro.** We don't write our own exporter; OTLP/HTTP protobuf to `ingest-gw`. The distro's
  job: one-line init, DSN, resource (service/env/version), `trooper.*` attributes (route template, entry route, consumer
  group/topic, caller), PII defaults, sampling. Framework auto-instrumentation comes from OTel's own libraries;
  we only write code for what's missing (e.g. route template, consumer group, Fiber/Gin route).
  - Go: `trooper-go` + `troopergin`, `trooperfiber`, `trooperecho`, `trooperchi`, `trooperhttp`, `trooperkafka` (franz-go, sarama),
    `trooperamqp`, `troopersql`, `trooperredis`, `trooperslog`.
  - JVM (Java, Kotlin): an OTel Java agent **extension** (`trooper-javaagent.jar` — agent + extension in one jar) → Spring Boot, Quarkus
    (JVM mode), Ktor, Kafka, RabbitMQ, JDBC all from the agent. For those who can't use the agent: `trooper-spring-boot-starter` and
    `trooper-quarkus` (configures Quarkus's `quarkus-opentelemetry` extension, including native image), and the `trooper-ktor` plugin.
  - .NET: `TaskTrooper.Telemetry` NuGet — `AddTaskTrooper()` (ASP.NET Core, HttpClient, EF Core/SqlClient, Confluent.Kafka, RabbitMQ).
- **Client SDKs (web, RN, Flutter, Android, iOS) = RUM JSON** (`POST /v1/rum`, schema `proto/rum/v1/rum.proto`). OTel client
  SDKs are heavy and don't cover RUM concepts (session, view, vital, crash, app start). Network requests are linked to the backend trace via the `traceparent`
  header.
  - Web: `@tasktrooper/browser` works without a framework (SPA routes via the History API, window error, fetch/XHR, Web Vitals). Thin packages for where
    the framework swallows errors: `@tasktrooper/react` (ErrorBoundary, React 19 root error callbacks),
    `@tasktrooper/vue` (`app.config.errorHandler`), `@tasktrooper/angular` (`ErrorHandler` provider). Svelte, Solid, Next.js and Nuxt need no extra package.
  - `@tasktrooper/react-native` (JS; react-navigation screen tracking, ErrorUtils, fetch/XHR, AppState, offline queue).
  - `tasktrooper_flutter` (Dart), `TaskTrooper` Android (Kotlin, AAR), `TaskTrooper` iOS (Swift Package).
**Consequence:** Backend language coverage is cheap and a customer using OTel can come without an SDK. Client side is a single schema → a single writer path.

## ADR-012 — Cloud integrations: AWS, Google Cloud, Vercel, Neon
**Context:** Part of the customer's DB/queue/deploy data exists only in the cloud provider's API (RDS/ElastiCache CPU-disk,
MSK broker metrics, SQS/Pub/Sub queues, Cloud SQL, Neon compute, Vercel deploy + log + Web Vitals). It can't be fetched via SQL.
**Decision:**
- New service `services/integrations` (`cmd/trooper-cloud`): for each connection in the Postgres `integrations` table it does
  **discovery** (upserts resources as `databases` / `broker_clusters` rows, `collector = 'cloud'`, `integration_id`) and
  **metric pulling** (60 s). It sends metrics to ingest-gw **with the same OTLP names** as the collector (contracts §5) —
  writer and api show "Cloud integration" sourced data without changes.
  - AWS: STS AssumeRole (with external id, recommended) or access key. CloudWatch `GetMetricData`: RDS/Aurora, ElastiCache,
    MSK (broker node + `SumOffsetLag`), SQS. Discovery: `rds:DescribeDBInstances`, `elasticache:DescribeReplicationGroups`,
    `kafka:ListClustersV2`, `sqs:ListQueues`.
  - Google Cloud: service account JSON (or workload identity). Cloud Monitoring `timeSeries.list`: Cloud SQL, Memorystore Redis,
    Pub/Sub (`num_undelivered_messages`, `oldest_unacked_message_age`). Discovery: Cloud SQL Admin, Memorystore, Pub/Sub API.
  - Neon: API key. Projects/branches/compute endpoints → `databases` (engine postgres); compute state, CU, active time,
    storage → `trooper.db.*`; for `pg_stat_statements` the existing collector's direct mode (with the Neon role) is used.
  - Vercel: access token (+ team id). Projects and **deploys** via the REST API → `deploy_markers` (service mapping: Vercel project
    name = service name, can be changed). **Drains**: Vercel log / trace / speed-insights / web-analytics drains
    arrive at `POST /v1/drains/vercel` on ingest-gw (`x-vercel-signature` HMAC-SHA1 verification) and are converted to logs, spans, RUM vitals /
    views.
- SQS and Pub/Sub are a **queue** model like RabbitMQ: `broker_clusters.kind ∈ {kafka, rabbitmq, sqs, pubsub}`; lag = visible /
  undelivered messages, plus the oldest message age (`oldest_age_s`).
- Credentials: secrets entered from the UI are encrypted with AES-256-GCM in the Postgres `secrets` table (`TT_SECRETS_KEY`, 32 bytes
  base64); `credentials_ref = 'secret:<id>'`. Other references are `env:` / `file:` (same as the collector). Secrets are never returned in API
  responses.
**Consequence:** Cloud data doesn't need a separate pipeline; it's just a new "source". For Lambda / Cloud Run / Vercel Functions,
RED metrics come from spans (OTel/Vercel trace drain); service RED is not derived from cloud metrics.

## ADR-013 — Dashboard data mode: synchronous API + remote cache
**Context:** Screens call `src/data/*` functions synchronously (`services`, `endpointSeries(...)`). Converting all of them to async means
rewriting 27 screens.
**Decision:** Function signatures stay the same. When `VITE_DATA=api` (default `mock`):
- List data (`services`, `monitors`, `incidents`, `kpis`, `databases`, `clusters`, `topics`, `consumerGroups`, scenarios,
  integrations …) is fetched from api via `hydrate()` when the app opens and the **contents** of the existing exported arrays are replaced
  (the reference stays the same). Refreshed every 60 s and after mutations.
- Detail/series functions use `remote(path, fallback)`: returns from cache if present, otherwise starts the request and returns an empty/zero
  fallback; when the response arrives the data version increments and screens using `useApp()`/`useData()` re-render.
- Mutations (`addDatabase`, `addLagMonitor`, ack/resolve …) POST to api, then invalidate the related cache.
**Consequence:** Screen code doesn't change; ~~mock mode is kept for development/demo~~ (removed by ADR-028). During loading, charts briefly show zero
(accepted; on critical screens a thin loading bar via `remoteStatus()`).

## ADR-014 — Code intelligence service (`services/codeintel`)
**Context:** Phase 4: read a repo and produce an inventory, alert suggestions and load test drafts. The tables (`repositories`,
`inventory_items`, `alert_suggestions`) exist in 001, but there is no service.
**Decision:**
- Separate Go service `codeintel` (:8096). Uses the same Postgres as api; only reads ClickHouse (observation matching).
  api triggers (`repositories.analyze_requested_at`), codeintel polls every 15 s (trooper-cloud `syncRequestedAt` pattern).
- Repo access: GitHub App (app id + private key secret, installation token) **or** token (`secret:`), and for dev/demo a
  `local` provider (`file://` path). Shallow clone; source is deleted when analysis finishes.
- Parsing: instead of tree-sitter, **deterministic extractors written in Go** (`go/ast` for Go; a lightweight
  tokenizer + framework patterns for TS/JS and Python). Rationale: single binary without CGO, and the extractors are pattern-based anyway. Scope: the code-intelligence.md
  table.
- LLM: only for **interpretation** (criticality, summary, failure mode, monitor suggestion, loadtest_hint). Endpoint, key and model come
  from the platform's single OpenAI-compatible endpoint (`TT_ASSISTANT_API_URL` / `_API_KEY` / `_MODEL`, ADR-034; originally Claude with
  `ANTHROPIC_API_KEY` + `TT_CODEINTEL_MODEL`); if key or model is missing the LLM step is off → static rules. Results are cached by file hash.
- Alert-as-code: during a scan `.trooper/monitors/*.yaml` is read → upserted into the `monitors` table with `source='code'`
  (`code_ref` = repo:path). UI and api keep these monitors read-only. A file's monitor is deleted when the file is removed from the repo.
- Webhook: `POST /v1/webhooks/github` (HMAC `X-Hub-Signature-256`) → incremental analysis on default branch push.
**Consequence:** An extra language/framework means a new extractor file. Tree-sitter accuracy can be added later if needed.
**Addendum (GitLab / Bitbucket, contracts §18a):** A per-repo token + per-repo webhook instead of an OAuth application. The self-managed
GitLab address is not in a separate integration row but in `repositories.base_url` (one field per repo; different
GitLabs in the same project are possible, queries stay simple). The webhook secret is generated by api, not the user (no weak secret risk), and shown
once. Since the Bitbucket push payload doesn't list files, every push enqueues `{*}`; the actual narrowing happens in the git diff incremental.

## ADR-015 — Load test controller and runner (`services/loadtest`)
**Context:** Phase 5: api has scenario/run CRUD, but no runner; runs stay `queued`.
**Decision:**
- Module `services/loadtest`: `loadtest-ctl` (:8097) + `loadtest-runner`. ctl picks up `loadtest_runs.state='queued'` rows
  with `FOR UPDATE SKIP LOCKED`; checks domain verification and plan limits; starts the runner
  (`TT_LOADTEST_EXECUTOR=local` child process, `k8s` → Job manifest, one Job per region).
- Load engine: instead of embedding k6, **our own Go engine** (VU and arrival-rate modes, stages, checks, thresholds,
  think time, template variables). Rationale: embedding k6 as a library is not a supported API and its dependency tree is heavy;
  our declarative scenario format is limited anyway. Raw k6 script mode: the runner invokes the `k6` binary if it is on PATH.
- Correlation: the runner puts `traceparent` + `baggage: tt.svc=loadtest:<run_id>` on every request → the target service's SDKs write this
  as `trooper.caller`; the UI separates test traffic by caller. Extra header `x-trooper-loadtest-run: <run_id>`.
- Runner metrics go via OTLP to our own ingest (`service.name = loadtest`, attr `trooper.loadtest.run`), live summary every 5 s
  into the `loadtest_runs.live` (jsonb) column. When finished: `summary`, `thresholds_passed`, `state ∈ passed|failed|aborted|error`.
- Security: the target host must be verified in `loadtest_domains` (DNS TXT `trooper-verify=<token>` or
  `https://<host>/.well-known/trooper-verify` content = token); `localhost`/`*.localhost`/`127.0.0.1` are exempt in dev
  (`TT_LOADTEST_ALLOW_LOCAL=true`). Limits: `TT_LOADTEST_MAX_VUS`, `TT_LOADTEST_MAX_DURATION`, monthly VU-minutes.
  Abort: api `POST /loadtests/runs/{id}/stop` → `abort_requested_at`; ctl stops the runner. Optional automatic abort
  (`spec.gate.abortErrorRate`).
**Consequence:** The full k6 ecosystem is not available; the declarative scope is deliberately limited.

## ADR-016 — User identity: OIDC + session, API keys are kept
**Context:** The dashboard currently runs with a single dev api key; there are no users, orgs or roles.
**Decision:**
- api runs the OIDC Authorization Code + PKCE flow (`TT_OIDC_ISSUER`, `TT_OIDC_CLIENT_ID`, `TT_OIDC_CLIENT_SECRET`,
  `TT_OIDC_REDIRECT_URL`). On successful login: `users` upsert, httpOnly `tt_session` cookie (Postgres `sessions`, 7 days, sliding).
- Identity order on a request: `Authorization: Bearer <api key>` (automation, CLI, SDK upload) → `tt_session` cookie. For requests coming
  with a cookie, the project comes from the `X-Trooper-Project` header (otherwise the user's first project).
- Roles at org level: `owner | admin | member | viewer`. viewer is GET only (plus read-only POSTs that change nothing, e.g. `POST /monitors/preview`; list in `server.readOnlyPosts`); admin+ manages members/keys/integrations.
- If OIDC is not configured (`TT_OIDC_ISSUER` empty), **dev login**: `POST /auth/dev-login {email}` only when `TT_AUTH_DEV=true`;
  in that case the UI shows email login. ~~The existing `VITE_API_KEY` flow keeps working for dev~~ (removed by ADR-028).
- `audit_log` table: who, when, which mutation.
**Consequence:** SAML/SCIM is follow-up work. API keys stay project-scoped. Password login for operator-provisioned accounts (shared demo viewer) was added
later: ADR-035.

## ADR-017 — Slack App and GitHub App integrations
**Context:** Today alerts go to Slack only via incoming webhook (the channel is fixed to the webhook). Connecting a repo requires entering a token/installation id
by hand.
**Decision:**
- **Slack App** (OAuth v2, bot scopes `chat:write`, `chat:write.public`, `channels:read`, `groups:read`, `channels:join`,
  `users:read.email`): one
  installation per org, bot token encrypted in `secrets`. Monitor/KPI rule notification target `slack:<channel_id>` (display name `#channel`
  stored separately); the notifier writes to that channel with `chat.postMessage`, stores the message `ts`, and resolve/reminder/ack go to the
  **thread** of the same message (the first message changes its status color via `chat.update`). Old `slack:` webhook channels keep
  working.
- Message content: title, status, value/threshold, service/endpoint, chart link, **likely root cause + confidence percentage** (alert-engine's
  RCA result: `rootCause.summary`, `rootCause.confidence` → "Likely cause: … (78% confidence)"; if confidence < 0.3 a "low
  confidence" label, if there is no RCA the line is not shown), deploy (last 1 hour), Ack / Open buttons (Slack interactivity →
  api `POST /slack/interactions`, `X-Slack-Signature` signature verification).
- **GitHub App**: the installation flow is in api (`/integrations/github/install` → GitHub → `/integrations/github/callback?installation_id=`),
  the installation's repo list is selected in the UI, selected repos are added to `repositories` and analysis is queued. The App webhook
  (single URL) comes to codeintel: `push` (default branch) → incremental analysis, `installation`/`installation_repositories` →
  repo add/remove sync, `pull_request` → later. Shared GitHub App client `services/pkg/githubapp`.
**Consequence:** Channel selection is done from a list in the UI; no need to copy a webhook URL.

## ADR-018 — AI usage quota: credits, org + member limits
**Context:** LLM calls (today codeintel enrichment; later RCA, suggestions) are made with a single Anthropic key, and
the only limit is items per analysis (`TT_CODEINTEL_LLM_MAX_ITEMS`). We need a per-plan "you have this much this month" and a per-person
ceiling.
**Decision:**
- Unit **credit**: 1 credit = 0.01 USD of model cost. Token → credit conversion via a model price table (config; model ids are not
  hard-coded). Different models are summed in a single measure.
- Period is the UTC calendar month; resets on the 1st (not a counter, a SUM from the `ai_usage` ledger).
- Limit layers: **plan default** (`free` 100, `team` 5,000, `enterprise` unlimited) → **org override**
  (`orgs.ai_monthly_credits`) → **member ceiling** (`memberships.ai_monthly_credits`, only jobs started by that person).
  Automatic jobs (analysis via webhook) only count against the org limit.
- Check **before** every LLM call (with estimated cost), record **after** the call (actual tokens). Serialized per org with a Postgres
  advisory lock; the limit is "soft": it can be exceeded by at most one call.
- When the limit is reached the job doesn't stop: the LLM step is skipped, it continues with static rules, and `quota_exceeded` shows up in the result.
- At the 80% and 100% thresholds, a single message to the notification targets chosen by the org (Slack channel etc.) (deduped per month+threshold).
- Shared logic in `services/pkg/aiquota` (dependency-free; Postgres access through an interface provided by the caller).
**Consequence:** No billing/payment; the plan is read from `orgs.plan`. The usage screen is Settings → Usage.

## ADR-019 — Host, Kubernetes and container monitoring (trooper-agent modes + optional eBPF)
**Context:** On the infrastructure side only DBs and brokers are monitored. There is no host, K8s cluster, pod, container or process visibility.
New Relic does this with Pixie (eBPF).
**Decision:**
- We don't write a new agent; `trooper-agent` (services/collector) gains **modes**: `db` (today's), `host` (host + container +
  process; DaemonSet in K8s), `cluster` (object state + Warning events from the K8s API; single-replica Deployment).
  They can be combined, e.g. `TT_AGENT_MODES=host,cluster`.
- Host metrics via gopsutil; container metrics via kubelet `/stats/summary` (+ `/pods` metadata) in K8s, and the Docker Engine
  API (read-only socket) outside K8s. Processes: top 20 by CPU+memory per host; command arguments are **not collected** (PII).
- Cluster mode uses read-only RBAC (get/list/watch); metrics are a snapshot every 30 s, Warning events as OTLP logs
  (`service.name=k8s`).
- **eBPF**: we don't write our own eBPF code. The Helm chart optionally installs the OpenTelemetry eBPF Instrumentation (OBI) DaemonSet
  (`ebpf.enabled`, off by default); OBI produces spans + RED metrics from uninstrumented HTTP/gRPC/SQL services and
  sends them via OTLP to ingest-gw → the existing trace/service map path. The UI shows these services with an "eBPF" badge.
- Data is written to typed ClickHouse tables (`hosts_1m`, `containers_1m`, `processes_1m`, `k8s_pods_1m`, `k8s_nodes_1m`,
  `k8s_workloads_1m`); the service ↔ pod link comes from the pod label (`tasktrooper.io/service` > `app.kubernetes.io/name` > `app`).
- Alerting: new monitor kind `infra` (host/container/pod/node/workload); in a service incident, alert-engine RCA makes OOMKilled/CrashLoop/restart
  in the same service's pods and host CPU saturation root cause candidates.
- Distribution: a single chart `deploy/helm/trooper-agent` (Linux DaemonSet + cluster Deployment + RBAC + optional OBI + optional
  Windows HostProcess DaemonSet + DB/broker collector with `db.enabled`); for non-K8s hosts `docker run` / systemd.
- Cluster mode uses list + watch; non-K8s containers in the order Docker → Podman → cgroup v2, GPU via `nvidia-smi` (§22).
**Consequence:** Pixie's full protocol parsing (MySQL/Kafka payload, flame graph) is out of scope; we get as much as OBI provides.

## ADR-020 — Serverless: AWS Lambda and Azure Functions
**Context:** Serverless functions are not visible; the AWS integration only discovers RDS/ElastiCache/MSK/SQS, and there is no Azure integration.
**Decision:**
- **AWS Lambda** is a new service of the existing AWS integration (`lambda` in `config.services`): ListFunctions + tag + event source
  mapping discovery (10 min), CloudWatch `AWS/Lambda` metrics (60 s): Invocations, Errors, Throttles, Duration
  (avg/max/p95/p99), ConcurrentExecutions, IteratorAge.
- **Azure** is a new provider (`provider='azure'`): service principal (tenant/client id + secret), subscription list. Function Apps
  are discovered from ARM; Azure Monitor metrics are at the **app** level (FunctionExecutionCount, FunctionExecutionUnits,
  Requests, Http5xx, HttpResponseTime, MemoryWorkingSet) → in Azure one "function" row = one Function App; the functions
  inside are in `meta.functions`.
- Trace/cold start: the customer instruments the function with OTel (AWS: OTel/ADOT Lambda layer or `@tasktrooper/node`
  `wrapLambda`; Azure: Functions OpenTelemetry mode) and sends OTLP to ingest-gw. The cold start count is computed from
  `faas.coldstart=true` on spans (not in CloudWatch).
- Cost estimate in api: GB-seconds + request price (constants in config), labelled "estimated" in the UI.
- Metadata in Postgres `serverless_functions`, metrics in ClickHouse `faas_1m`; monitor kind `faas` for alerting.
**Consequence:** Google Cloud Functions/Run and Vercel Functions can later be added to the same table.

## ADR-021 — Session replay: rrweb for web, wireframe for mobile
**Context:** RUM sessions are visible as counts and event lists; what the user experienced on screen is not visible. New Relic offers web
and mobile session replay. It was on the "Out of scope (v1)" list.
**Decision:**
- **Web:** recording with `rrweb` (`@rrweb/record` 2.2.0), DOM snapshot + mutation; separate package `@tasktrooper/replay` (so as not to bloat the browser
  SDK; plugs into `@tasktrooper/browser` as an integration). Playback in the UI with `@rrweb/replay` 2.2.0.
- **Mobile (Android, iOS):** pixels/screenshots are **not collected**. A **wireframe** (rectangle, type, color,
  masked text) is extracted from the view hierarchy frame by frame, only on change and at most 2 frames/s; touches are separate events. The player is ours (SVG).
  This way PII leaking inside images is structurally impossible.
- **React Native:** RN draws with native views → the native SDK's replay (Android `trooper-replay`, iOS `TaskTrooperReplay`)
  works as is; `@tasktrooper/react-native` starts it via the `TrooperRum` native module, and the session comes from JS via `setContext`.
- **Flutter:** since it draws onto a single `FlutterView` surface, native traversal doesn't work; `tasktrooper_flutter` extracts the wireframe on the Dart side
  **from the Element tree** (widget type → kind, `Material`/`ColoredBox`/`DecoratedBox` → color/radius,
  `RenderBox` → global rect), same wire format.
- **Privacy default:** on web all text and inputs are masked (`maskAllText`, `maskAllInputs` true), passwords always,
  `data-trooper-block` elements become placeholders, canvas / cross-origin iframes / network bodies are not recorded. On mobile text is masked,
  images are boxes only, inputs always masked. Unmask explicitly (`data-trooper-unmask` / `trooperUnmask()`).
- **Sampling:** decision per session: `sampleRate` (default 0.1) records; `onErrorSampleRate` (default 1.0) keeps the last 60 s in memory,
  flushes on error and keeps recording. The decision is fixed for the session (web: localStorage, shared across tabs).
- **Transport:** segments (≤ 5 s or ≤ 256 KB) `POST /v1/replay` → `tt.replay` → the writer writes the blob to **S3** (`tt-replays` bucket),
  and the index to ClickHouse `replay_segments`. Retention 30 days. Session metadata (user, device, errors, views)
  comes from `rum_events`; replay doesn't introduce a separate session concept (`session_id` is shared).
- **Correlation:** the player's timeline comes from `rum_events` (view, action, error, resource); via the `trace_id` on resource events
  you can go from the replay to the backend trace (`/traces/:id`). "Watch replay" from an error detail jumps to the relevant second.
**Consequence:** No real image on mobile (deliberate); on web, canvas/WebGL content appears empty. Details: contracts.md §23.
On web the decision/counter is shared across tabs in localStorage; a tab in buffer mode switches to `session` via the `storage` event.

## ADR-022 — Continuous profiling: pprof + folded stack, stack rows in ClickHouse
**Context:** When an endpoint slows down, "where time goes" gives a dependency breakdown, but inside our own code (the `app` category)
it's not visible which function is burning CPU/memory. New Relic offers a thread profiler and flame graphs.
**Decision:**
- SDKs take continuous, low-overhead profiles (default: 10 s of CPU every 60 s, heap/goroutine once every 60 s) and
  send them to `POST /v1/profiles`. Two wire formats: **pprof** (Go's native format) and **folded stack** (`a;b;c 123`, simple for JVM/Node;
  needs no dependencies). The OTLP profiles signal is not stable yet — it will be accepted in gw once stable.
- **Go:** `runtime/pprof`; pprof labels `trooper.route` / `trooper.method` are set on the request goroutine → the flame graph can be filtered
  per endpoint. **JVM:** JFR (`jdk.ExecutionSample`, `jdk.ObjectAllocationSample`, `jdk.JavaMonitorEnter`), no native
  agent. **Node:** `node:inspector` CPU profiler + sampling heap profiler. **.NET:** separate package `TaskTrooper.Telemetry.Profiling`,
  in-process EventPipe session (`DiagnosticsClient` on its own PID) + parsing with TraceEvent: SampleProfiler → `cpu`/`wall`,
  `GCAllocationTick` → `alloc_space`, contention → `lock`; route label via Activity → thread mapping.
  **Python:** the `tasktrooper` package (ADR-024) — a `sys._current_frames()` sampler thread (100 Hz) → `cpu`/`wall`, optional
  `tracemalloc` → `inuse_space`.
- **OTLP profiles:** gw accepts `POST /v1development/profiles` (OTLP/HTTP protobuf + JSON) and gRPC `ProfilesService/Export`
  (OTel Collector / eBPF profilers connect directly); the writer unpacks them into the same tables via the pprof path.
- The writer unpacks profiles into stack rows: ClickHouse `profile_stacks` (stack = frame array, root first; value) and
  `profiles` (summary per profile). Flame graph, top functions and diff are computed in api with `GROUP BY stack`.
- **Diff:** comparison of two windows/versions by share (value / total) — the "which function grew" question before/after a
  deploy. Retention 14 days.
- Cardinality: frame name = function (no file/line, `fn (file)` in Node), stack depth ≤ 128, ≤ 10k unique
  stacks per profile (smaller ones merge into `[other]`).
**Consequence:** No thread dump / on-demand "thread profiler"; continuous profiling already answers the same question. Details: contracts.md §24.

## ADR-023 — AI / LLM monitoring: OTel GenAI semantic conventions
**Context:** Customers call LLMs in their applications (OpenAI, Anthropic, Gemini, Bedrock, Azure OpenAI…). Today these calls
only show up as an `HTTP · api.openai.com` dependency: no model, tokens, cost or prompt. New Relic offers "AI monitoring".
(Note: ADR-018 is our own LLM quota; this ADR monitors the customer's LLM usage — "AI monitoring" in the UI.)
**Decision:**
- Wire = a normal OTLP span, **OTel GenAI semantic conventions** (`gen_ai.provider.name`, `gen_ai.operation.name`,
  `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens|output_tokens`, …). No new endpoint;
  spans from OTel's own GenAI instrumentations (Spring AI, Microsoft.Extensions.AI, OpenLLMetry, Vercel AI SDK)
  work as is too.
- SDKs provide zero-code wrappers: **Node** `instrumentOpenAI(client)` / `instrumentAnthropic(client)` + Vercel AI SDK
  telemetry; **Go** `llm.Start(ctx, …)` helpers + an OpenAI/Anthropic HTTP `RoundTripper` (reads tokens from the response,
  including streams); **JVM** takes Spring AI / LangChain4j spans from OTel + an Anthropic/OpenAI Java client wrapper;
  **.NET** the `Microsoft.Extensions.AI` ActivitySource is on by default.
- **Prompt / completion content is not collected by default** (PII). Enabled with `captureContent: true`; the writer separates the content from the span
  attributes and writes it to the `llm_content` table (≤ 32 KB per field, retention 7 days); only tokens/meta stay on the span.
- Writer: a GenAI client span becomes dependency category **`ai`** (`dep = "<provider> · <model>"`) → visible in "Time spent" and RCA;
  plus the `llm_1m` rollup (provider, model, operation, service, entry route; calls, errors, tokens, duration digest).
- **Cost** is computed in api: a per-model price table (input/output/cache read per 1M tokens) with in-code defaults +
  org override (Postgres `llm_prices`). Prices change → "est." label in the UI; cost is empty for models with unknown prices.
- UI: **AI monitoring** in the sidebar (calls, tokens, cost, errors, p95 per provider/model/service/endpoint; recent calls +
  prompt drawer), an **AI** tab in service detail (if there are LLM calls), a GenAI span panel in trace detail.
**Consequence:** For Python, the `tasktrooper` package (ADR-024) provides `instrument_openai` / `instrument_anthropic`; OpenLLMetry also works.
LLM alerts are monitor kind `llm` (contracts §25, alerting.md). Details: contracts.md §25.

## ADR-024 — Python SDK (`tasktrooper`): OTel distribution + profiling + LLM
**Context:** Most LLM applications are Python. The Python SDK was "phase 2" on the roadmap; it's needed for AI monitoring and profiling.
**Decision:**
- PyPI package `tasktrooper` (`sdks/python`, Python ≥ 3.9): a thin OTel distribution as in ADR-011 — `opentelemetry-sdk` +
  OTLP/HTTP protobuf exporter, DSN, resource (`telemetry.distro.name=trooper-python`), ParentBased(TraceIdRatio) +
  `tt=r:` tracestate, `trooper.entry.route/method`, `trooper.caller` (baggage `tt.svc`), `trooper.sample_rate`; config names per
  contracts §2 (`TROOPER_DSN`, …). Framework instrumentation from OTel contrib packages (extras: `tasktrooper[django]`,
  `[fastapi]`, `[flask]`, `[celery]`, `[requests]`, `[sqlalchemy]`…); route template from the FastAPI/Flask/Django router.
- `tasktrooper.init(...)` never throws; `tasktrooper-run` launcher + `import tasktrooper.auto`.
- Profiling (§24) and LLM (`instrument_openai`, `instrument_anthropic`, sync + async + stream; §25) in the same package, no extra dependencies.
**Consequence:** Package publishing happens together with the other SDKs (requires accounts).

## ADR-025 — MCP server: `services/mcp`, read-only, stdio, wraps api
**Context:** Local coding agents (OpenCode, Claude Code) should offer natural-language queries over telemetry; no team should have to
write a third-party observability MCP. The API has 150+ endpoints; the agent tool set can't mirror them.
**Decision:**
- New Go module `services/mcp` (`cmd/tt-mcp`), added to `services/go.work`; official Go MCP SDK, **stdio**
  transport (streamable-HTTP is an extension target, not designed in v1).
- Tools wrap `services/api` (`openapi.yaml`); no direct connection to ClickHouse/Postgres (contracts §26).
  Tenant (org/project) comes from the api key; additionally `TT_ORG_ID`/`TT_PROJECT_ID` are verified at startup via `/me`.
- v1 is **read-only**: 12 tools (service KPIs, endpoint, endpoint RCA, trace, log, monitor, incident + timeline,
  load test run, deploy marker). No write tools. Every tool returns input validation, a result cap (64 KB; if truncated,
  `truncated`/`total`/`hint` markers) and errors without secrets/stacks. Details: `.ai/mcp.md`.
**Consequence:** The tool set is deliberately narrow; when a new endpoint is added to api, MCP grows in sync. Write tools
come in v2 with a separate ADR.

## ADR-026 — CI contract: Make targets + go.work module matrix
**Context:** On PRs, every Go module's build/vet + unit tests, web typecheck + build, JS SDK build + vitest
and workflow lint must be verified; the module list can't be kept in two places.
**Decision:**
- `.github/workflows/ci.yml` derives the module matrix from `services/go.work`; `make check` runs the same gates locally
  — CI contract = Makefile targets.
- PR CI is unit-only; integration/e2e (`make e2e-ingest`) is local and deferred, it doesn't enter CI.
- Image builds are not in ci.yml but in `.github/workflows/docker.yml` (release/registry flow). The matrix
  is derived from `services/*/Dockerfile` (context is the repo root — via ADR-027; previously `services/` —, `GOWORK=off`; Dockerfiles with a `CMD` arg get one
  row per binary). PR: build + Trivy (CRITICAL, ignore-unfixed), no push. `main`: push to `ghcr.io/<owner>/<repo>/<image>:{sha,main}`;
  `packages: write` only in this job. No deploy step.
- Images don't set `TT_ENV`; the production environment must provide `TT_ENV=production` + a real `TT_SECRETS_KEY`, otherwise
  the service won't start (fail-closed, contracts §10).
**Consequence:** Adding a module to `go.work` expands CI automatically; no matrix sync headaches.

## ADR-027 — Self-observability: the platform monitors itself with its own SDKs (dogfooding)
**Context:** architecture.md said "all services are instrumented with our own SDK", but the services only exposed Prometheus
`/metrics`. Seeing the platform's own trace/log/profile/LLM data in its own dashboard is needed both for product validation and for
operations.
**Decision:**
- Platform data goes to **a separate project** (`TaskTrooper` / `tasktrooper`, `make self` → `api bootstrap`, ADR-028). It doesn't mix with customer data; in production the operator provides the DSN of their own self project.
- Every Go service calls `trooper.Start` in `main` (`github.com/makifbaysal/tasktrooper-telemetry/sdks/go`; versioned from proxy.golang.org via ADR-029).
  Without a DSN (default) nothing is exported — behavior is the same as today. Instrumentation: HTTP (chi / net/http), pgx,
  go-redis, franz-go, slog → OTel logs, Go runtime metrics, optional profiling (`TROOPER_PROFILING`), codeintel's
  Anthropic calls via the `llm` transport (GenAI span; content capture off). Prometheus `/metrics` stays as is.
- Service names are fixed: `tt-ingest-gw`, `tt-ingest-writer`, `tt-api`, `tt-alert-engine`, `tt-notifier`, `tt-trooper-cloud`,
  `tt-symbolicator`, `tt-codeintel`, `tt-loadtest-ctl`, `tt-loadtest-runner`, `tt-mcp`, `tt-trooper-agent`; the dashboard RUM
  app is `tt-web` (`@tasktrooper/browser` + `@tasktrooper/react` + `@tasktrooper/replay`, off if `VITE_TROOPER_DSN` is not set).
- **Feedback loop forbidden:** ingest-gw and ingest-writer don't produce spans and logs for requests/records belonging to the `TT_SELF_PROJECT_ID` project
  (counters/Prometheus continue). Otherwise self traffic would ingest its own spans and create an infinite loop.
  The SDK exporter's own HTTP calls are not instrumented anyway; when services wrap their outbound HTTP clients they
  exclude requests to the ingest endpoints.
- Platform infrastructure (Postgres, ClickHouse, Redpanda, Redis) is reported to the self project with `trooper-agent`
  (`deploy/self/agent.yaml`, `make self`).
- The Docker build context is the repo root instead of `services/` (Dockerfiles also copy `sdks/go`); the
  "context `services/`" in ADR-026 is updated by this ADR.
**Consequence:** The platform appears in its own dashboard as the "TaskTrooper" project; a regression in the SDK is seen by us first.
The self project's quota/sampling can be tuned separately. Details: contracts.md §27.

## ADR-028 — No dummy / seed data: the platform only works with real data
**Context:** The dashboard opened with mock data by default (`VITE_DATA=mock`), `tools/demo` generated fake service/RUM traffic and
monitor/KPI/load test seeds (the seed was already broken because api rejects `source: "code"`), and a dev seed was applied to Postgres
(`002_seed.sql`: Acme org, Shop project, `tt_dev_*` keys). Fake data hid real bugs and required maintaining two code paths
(mock + api). With ADR-027 the platform can produce its own real data.
**Decision:**
- `tools/demo` and `make demo*` are removed. `apps/web` mock mode is removed: no `VITE_DATA`, all `src/data/*` read only from api;
  when there is no data, screens show an empty state + setup instructions (SDK snippet). ADR-013's synchronous function + remote cache
  structure stays; the mock branches are deleted.
- Postgres dev seeds (`002_seed.sql`, `014_self_project.sql`) are removed. Setup is done via the real flow:
  - **Onboarding:** a user who has logged in (OIDC or `TT_AUTH_DEV=true` dev login) and has no org lands on a "create org + first
    project" step in the UI → `POST /api/v1/orgs {name, project}` (the user becomes owner) → the ingest key is shown once + SDK
    setup snippets.
  - **Bootstrap command:** `api bootstrap --org <name> --project <name> --owner <email> [--ingest-key-out file]` — idempotent; creates the org,
    project, owner membership and an ingest key, and prints the DSN. For the self-observability project (ADR-027) and CI/automation.
    `make self` uses it; `TT_SELF_PROJECT_ID` is the project id this command returns (not a fixed `2`).
- Tests set up their own fixtures (create org/project/key within the test); no test relies on a seed. DSNs in SDK examples/READMEs
  become placeholders (`<ingest-key>`).
- The `VITE_API_KEY` dev shortcut is removed; the dashboard only works with the session cookie.
**Consequence:** On first launch the dashboard is empty; data arrives once an SDK connects (or the platform itself via `make self`). The "mock mode
is kept" item in ADR-013 and `VITE_DATA` in contracts §11 are invalidated by this ADR.

## ADR-029 — SDKs are published on public registries (tag-triggered release)
**Context:** SDKs could only be used from the monorepo via local path/`replace`; customers (and our own applications) couldn't pull versioned packages
from a package manager. Package names were not publishable (`@trooper/*`, `github.com/makifbaysal/tasktrooper-telemetry/sdks/go`,
`io.tasktrooper`/`com.tasktrooper`), and the repo had no license.
**Decision:**
- **License:** SDKs are Apache-2.0 (`sdks/LICENSE`, each package carries a copy). Services + web are outside the scope of this ADR.
- **Names:**
  | Ecosystem | Registry | Name |
  |---|---|---|
  | Go | proxy.golang.org (GitHub) | `github.com/makifbaysal/tasktrooper-telemetry/sdks/go[/integrations/<x>]` |
  | JS | npm | `@tasktrooper/{core,browser,react,vue,angular,react-native,node,replay}` |
  | JVM + Android | Maven Central (Central Portal) | groupId `ai.tasktrooper` (Java/Kotlin package names don't change) |
  | .NET | NuGet | `TaskTrooper.Telemetry*` |
  | Python | PyPI | `tasktrooper` |
  | Flutter | pub.dev | `tasktrooper_flutter`, `tasktrooper_dio` |
  | iOS | CocoaPods + SwiftPM | pod `TaskTrooper`; SwiftPM `Package.swift` at the repo root |
- **Release:** each ecosystem is versioned independently and triggered by a tag; the workflow checks the tag version against the package version, tests, and publishes.
  | Tag | Workflow | Authentication |
  |---|---|---|
  | `sdks/go/vX.Y.Z`, `sdks/go/integrations/<x>/vX.Y.Z` | `release-go.yml` (proxy warm-up) | — |
  | `js-vX.Y.Z` | `release-npm.yml` | npm trusted publishing (OIDC); first publish `NPM_TOKEN` |
  | `jvm-vX.Y.Z`, `android-vX.Y.Z` | `release-jvm.yml`, `release-android.yml` | `MAVEN_CENTRAL_*` user token + `GPG_*` signing |
  | `dotnet-vX.Y.Z` | `release-dotnet.yml` (env `nuget`) | NuGet trusted publishing (OIDC) |
  | `python-vX.Y.Z` | `release-python.yml` (env `pypi`) | PyPI trusted publishing (OIDC) |
  | `tasktrooper_flutter-vX.Y.Z`, `tasktrooper_dio-vX.Y.Z` | `release-flutter.yml` | pub.dev automated publishing (OIDC); first publish by hand |
  | `ios-vX.Y.Z` | `release-ios.yml` | `COCOAPODS_TRUNK_TOKEN`; for SwiftPM the workflow pushes a plain `X.Y.Z` tag |
- Wherever possible, OIDC trusted publishing is used instead of long-lived tokens.
- The platform itself also uses the published SDK (real dogfooding): Go services don't use `replace` for `sdks/go`,
  they pull the versioned module from proxy.golang.org; `apps/web` installs `@tasktrooper/*` from npm. An SDK change reaches the platform once a new version
  is published and the dependency is bumped. SDK examples stay on local paths.
**Consequence:** The "Package publishing" roadmap item is closed. Package names are now a public contract; renaming is a breaking change.
Details: sdk.md § Distribution.

## ADR-030 — Production hosts: subdomains under `telemetry.tasktrooper.ai`
**Context:** The SaaS's public address hadn't been decided; the docs had inconsistent examples such as `ingest.tasktrooper.io`, `ingest.tasktrooper.dev` and `<api-host>`.
Landing, docs and dashboard are served from the same web application (`apps/web`).
**Decision:** A single root `telemetry.tasktrooper.ai`; every externally exposed service on its own subdomain, all on 443 + TLS:

| Host | Service | Internal port |
|---|---|---|
| `telemetry.tasktrooper.ai` | `apps/web` (landing `/`, `/docs`, `/login`, dashboard; SPA, unknown path → `index.html`) | static |
| `api.telemetry.tasktrooper.ai` | `services/api` (`/api/v1/...`, OIDC callback, Slack/GitHub callbacks) | 8080 |
| `ingest.telemetry.tasktrooper.ai` | `ingest-gw` (OTLP/HTTP, `/v1/rum`, `/v1/replay`, `/v1/profiles`; OTLP gRPC over h2 on the same host:443) | 4318, 4317 |
| `hooks.telemetry.tasktrooper.ai` | `codeintel` webhooks (`/v1/webhooks/<github\|gitlab\|bitbucket>`) | 8096 |
| `symbols.telemetry.tasktrooper.ai` | `symbolicator` (`trooper-cli` sourcemap/dSYM/mapping upload) | 8095 |

- Other services (ingest-writer, alerting, the collector's internal endpoints, loadtest-ctl, integrations, ClickHouse, Postgres, Redpanda, Redis)
  are not exposed externally.
- Web and api are the same site (`tasktrooper.ai` eTLD+1) → the `tt_session` cookie (host-only, on the api host, `SameSite=Lax`, `Secure`)
  is sent with `fetch(..., { credentials: 'include' })`; CORS only for `https://telemetry.tasktrooper.ai`.
- DSN format: `https://<ingest-key>@ingest.telemetry.tasktrooper.ai/<project-id>`.
**Consequence:** Env values and DNS records are in contracts.md §2a. Examples and docs use these hosts; no other domain examples are written.

## ADR-031 — Demo deployment: a single Mac, self-hosted GitHub Actions runner, Cloudflare Tunnel
**Context:** The product needs a public demo; production infrastructure (ADR-030 hosts, multi-node setup) doesn't exist
yet. There is a Mac mini (Apple Silicon, 16 GB) with Docker Desktop on hand. Opening ports on the home network and a static IP are not wanted.
**Decision:**
- The whole platform on a single machine, with the existing `deploy/docker-compose.yml` + `deploy/demo/docker-compose.demo.yml` (restart
  policy, public URLs, `TT_AUTH_DEV=false`, `TT_COOKIE_SECURE=true`, a Caddy `web` service for the dashboard). Running in the
  demo: ClickHouse, Postgres, Redpanda, Redis, s3, ingest-gw, ingest-writer, api, alert-engine, notifier, web
  and (via ADR-032) loadtest — small limits: 50 VUs, 15 min, a single concurrent run, 3000 VU-min per month, `TT_LOADTEST_ALLOW_LOCAL=false`.
  No codeintel or trooper-cloud. Images are built from source on the machine (arm64; ghcr images are amd64).
- Exposure: Cloudflare Tunnel (outbound connection, no ports). Hosts are one level below the zone: `telemetry.`,
  `telemetry-api.`, `telemetry-ingest.tasktrooper.ai` — Cloudflare's free certificate doesn't cover two levels
  (`api.telemetry.…`). Only OTLP/HTTP (4318) goes through the Tunnel, no gRPC (4317).
- Login: Google OIDC (`TT_OIDC_ISSUER=https://accounts.google.com`); dev login off.
- Deploy: the `deploy-demo` workflow, manual only (`workflow_dispatch`) and `main` only, `demo` environment,
  runs on the self-hosted runner on the machine (label `trooper-demo`). Secrets are in GitHub: `DEMO_ENV` = the whole of `deploy/.env`;
  the workflow writes the file, runs `deploy/demo/deploy.sh`, and deletes it at the end. `deploy.sh`: infra → `make migrate`
  → dashboard build (outside the checkout, `~/trooper-demo/web`) → services `up -d --build` → health check.
- The repo is public: "Require approval for all external contributors" is mandatory so that fork PRs don't reach the self-hosted runner;
  no other workflow uses the `trooper-demo` label.
- Self-observability (ADR-027) like any customer: the DSN of the project opened in the dashboard is the `demo` environment secret
  `DEMO_TROOPER_DSN`; no special bootstrap. The dashboard RUM sends to the public ingest host; the Go services and the `trooper-agent`
  send to `ingest-gw:4318` on the compose network (deploy.sh rewrites the DSN host, so self-telemetry never crosses the tunnel);
  the agent (`deploy/demo/agent.yaml`, modes `db,host`) sends Postgres/Redis/Redpanda and container metrics.
  `TT_SELF_PROJECT_ID` (loop protection) is derived from the DSN's project id.
- The tunnel runs with `protocol: http2` and `edge-ip-version: "4"`: on QUIC the home network dropped idle UDP flows, and over
  IPv6 the HTTP/2 connections still dropped every ~2 minutes (every hostname 530 until the reconnect). The Mac never sleeps (`pmset`).
- The service containers use `dns: [1.1.1.1, 8.8.8.8]` (so the home router's stale DNS cache doesn't send outbound calls
  elsewhere); `TROOPER_VERSION` = git short sha (deploy marker). Dashboard: long cache for `/assets/*`,
  `no-cache` for every other path; the build is copied with `rsync --delete-after` (no 404s during deploy).
**Consequence:** Setup and steps are in `deploy/demo/README.md`. When the machine is off the demo is down too (no uptime guarantee); backups and
short retention are separate work. On moving to production, the ADR-030 hosts and separate infrastructure apply.

## ADR-032 — MCP write tools: opt-in, confirmed, `source = agent`
**Context:** ADR-025 opened MCP read-only; agents can find problems but can't define alerts, KPIs or load tests for the project,
and the user enters every suggestion by hand in the UI. The data needed for alert design (error groups, KPIs, suggestions, inventory, templates,
notification channels, infra) was also missing from the tool surface. API keys are in the `admin` role (identity.go), i.e. the key itself doesn't
restrict writes.
**Decision:**
- 15 new read tools (27 total): errors, KPIs, suggestions, inventory, monitor templates, notification channels (Slack App channels
  + defined channels; never config/secrets), RUM summary, AI usage, host / database / consumer group, load test scenarios +
  domains, a single run, and `tt_preview_monitor` (POST but doesn't save — to see threshold noise before saving).
- 11 write tools are registered only with `TT_MCP_ALLOW_WRITE=true` (off by default): monitor create / update / delete, suggestion acceptance,
  KPI create / update / delete, Slack test message, load test scenario creation, starting and stopping a run.
- Every write requires `confirm`; without it the request is not sent, and the body that would be sent (`dryRun`) is returned. The agent shows this to the user and
  repeats with `confirm: true` after approval. Exception: `tt_stop_loadtest` (it only reduces load).
- Monitors, KPIs and scenarios created by the agent have `source = 'agent'` (check constraints in `014_agent_source.sql`; UI badge). The agent
  only deletes `agent` records; it doesn't change records managed from the repo (`code`).
- Load test: draft scenarios don't run, the target must be a verified domain (adding domains is up to the user), and if the agent's scenario has no `gate`
  an automatic abort threshold is added.
- Usage flow in `skills/trooper-alerts`: read first, derive the threshold from data, preview, get approval as a batch list; if data is insufficient,
  suggest a load test.
**Consequence:** Confirmation is part of the tool contract, not the MCP client; a careless agent could pass `confirm: true` on the first call —
the real protection is the env flag and reversibility via the `agent` source. A separate write-scoped API key role (read-only key) is open work.
With this decision the demo (ADR-031) also runs the `loadtest` service; limits are in `deploy/demo/docker-compose.demo.yml`.
## ADR-033 — Dashboard assistant: AI chat over the MCP tool surface, approvals in the UI
**Context:** The Overview "Ask telemetry" drawer offered three fixed questions answered by the offline playground mock. Users want a free-text
chat that knows the project: ask about incidents, services and load test results, mention items, attach screenshots, and define monitors /
KPIs or start a load test without leaving the chat. The MCP server (ADR-025, ADR-032) already has the right tool surface with shaped results
and dry-run semantics, but it runs over stdio with an api key.
**Decision:**
- A new screen `/assistant` (chat history per user and project, `assistant_*` tables in Postgres) replaces the Overview drawer; the
  playground's `/ask-telemetry` route is removed. Spec: `.ai/assistant.md`.
- The agent loop runs in `services/api` (`internal/assistant`). Tools come from `services/mcp/toolset`, an exported wrapper over the MCP
  server, connected with an in-memory MCP transport — the assistant and `tt-mcp` share one tool surface. The api module requires
  `services/mcp` with a `replace ../mcp` (the api Dockerfile copies it).
- Tool HTTP calls are served **in process** by the api router (a `RoundTripper`), carrying the user's `tt_session` cookie and project header:
  tools act with the user's role (viewers cannot write), writes are audited as the user, created entities get `source = agent`.
- Write tools are on (`TT_ASSISTANT_ALLOW_WRITE`, default `true`) because the human is in the loop: any tool call with `confirm: true` stops
  the turn and becomes an **approval card**; only `POST …/approvals/{id}` with `approve: true` executes it. A new message rejects pending
  approvals.
- Replies stream as SSE. An assistant-only tool `show_widget` renders entity cards (references, live data from the dashboard data layer),
  charts, tables and stats that link to dashboard paths.
- Provider: any **OpenAI-compatible Chat Completions** API with function calling — Meta Muse Spark by default
  (`TT_ASSISTANT_API_URL`, `TT_ASSISTANT_API_KEY`, `TT_ASSISTANT_MODEL`, model never hard-coded; plain HTTP + SSE, no vendor SDK);
  otherwise an offline mock that calls one read tool and spends nothing. Model requests reserve and record AI credits (ADR-018, feature `assistant`).
**Consequence:** The approval gate lives in the api, not in the model's goodwill — stronger than the MCP `confirm` convention. Images are
stored in Postgres (≤ 5 MB each); move them to object storage if volume grows. One running turn per thread is enforced per api instance
(in-memory lock); several api replicas would need a Postgres advisory lock.

## ADR-034 — One OpenAI-compatible LLM endpoint for the whole platform (Meta Muse Spark by default)
**Context:** Three code paths called LLMs with three configurations: the assistant (OpenAI-compatible, `TT_ASSISTANT_*`), the api's
incident summary (`TT_AI_PROVIDER` meta|baseten, `TT_AI_ALLOW_PAID`, `MODEL_API_KEY` / `BASETEN_API_KEY`, `TT_AI_MODEL`, model ids
hard-coded per provider) and codeintel enrichment (Anthropic Go SDK, `ANTHROPIC_API_KEY` + `TT_CODEINTEL_MODEL`). Operators had to
configure, pay for and monitor several providers, and the incident summary violated "model ids come from config".
**Decision:**
- **Every** LLM call goes through one **OpenAI-compatible Chat Completions** endpoint: `TT_ASSISTANT_API_URL` (default
  `https://api.meta.ai/v1`, Meta Muse Spark; `…/v1` or the full `…/chat/completions` URL), `TT_ASSISTANT_API_KEY`, `TT_ASSISTANT_MODEL`.
  The variable names stay `TT_ASSISTANT_*` (already deployed); api (assistant + incident summary) and codeintel read the same three.
- Shared helper `services/pkg/llmapi`: env parsing, URL normalization, `gen_ai.provider.name` from the host (`api.meta.ai` → `meta`,
  `*.baseten.co` → `baseten`, unknown hosts → `openai`) and a small non-streaming client (Bearer auth, retries on network error / 429 /
  5xx, `usage.prompt_tokens` / `completion_tokens` / `prompt_tokens_details.cached_tokens`). Plain `net/http`, no vendor SDK; the
  `anthropic-sdk-go` dependency is removed from codeintel. The assistant keeps its own streaming client but uses the same normalization.
- Key **and** model set → real model; otherwise every feature falls back as before (assistant / incident summary: offline mock,
  codeintel: static rules). `TT_AI_ALLOW_PAID` is dropped: key + model are the opt-in, like the assistant's. In exchange the incident
  summary now reserves and records AI credits (ADR-018, feature `rca`, `ref = incident-summary`) and answers 429 when the quota is used up.
- codeintel asks for JSON with `response_format: json_schema` and the schema in the prompt, and falls back to the prompt alone when the
  endpoint rejects `response_format` with HTTP 400. Anthropic prompt-cache control is gone; cached prompt tokens reported by the endpoint
  are billed as cache reads.
- Removed env vars: `TT_AI_PROVIDER`, `TT_AI_ALLOW_PAID`, `MODEL_API_KEY`, `BASETEN_API_KEY`, `TT_AI_MODEL`, `ANTHROPIC_API_KEY`,
  `TT_CODEINTEL_MODEL`.
**Consequence:** One key, one bill, one place to switch providers (any OpenAI-compatible gateway works, including a self-hosted one).
All features share one model: a smaller/faster model for enrichment and a stronger one for the chat would need separate variables again.
Endpoints without function calling cannot run the assistant; endpoints without JSON mode still work for codeintel via the prompt schema.

## ADR-035 — Password login for shared demo accounts
**Context:** The public demo (ADR-031) signs in with Google OIDC only. People who want to look inside it need an account in the
org, which means inviting every visitor by email. We want one shared, read-only login we can hand out.
**Decision:**
- `POST /auth/password-login {email, password}`, served only when `TT_AUTH_PASSWORD=true` (default off → 404; `GET /auth/config`
  says `password: true`). Success creates the same `tt_session` cookie and `login` audit entry (`method: password`) as OIDC.
- Hashes: bcrypt (cost 12, `golang.org/x/crypto/bcrypt`) in `users.password_hash` (migration 018). Unknown email, an OIDC user
  without a hash and a wrong password give the same 401 and the same bcrypt work (dummy hash), so accounts cannot be probed.
- No sign-up, reset or self-service change: hashes are set only by the operator CLI `api set-password … --password-stdin`
  (12–72 bytes; creates the user, ensures the membership, default role viewer, never demotes the last owner; a changed password
  deletes the user's sessions). Users with a password can still sign in with OIDC.
- Failed attempts are limited in memory per client IP (10 / 15 min) and per email (30 / 15 min; looser so one client cannot
  lock the shared account) → 429. The IP comes from `TT_CLIENT_IP_HEADER` (demo: `CF-Connecting-IP`). A success does not reset the
  IP counter: knowing the public demo password must not buy more guesses at other accounts. A foreign `Origin` → 403 (login CSRF).
- Demo: `deploy.sh` runs `set-password` for `DEMO_LOGIN_EMAIL` / `DEMO_LOGIN_PASSWORD` as a **viewer** of the self project's org.
- A user with a password cannot `POST /orgs` (403): a shared login must not own orgs, ingest keys or invites.
**Consequence:** Anyone with the shared password sees the demo org read-only; all of them are one user (shared AI credits and
assistant threads, one audit identity). Limits are per api replica (no Redis yet). Not meant for customer accounts: those use OIDC.
