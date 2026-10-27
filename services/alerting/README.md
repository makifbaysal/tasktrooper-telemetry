# services/alerting

Go module `github.com/tasktrooper/telemetry/services/alerting` with two commands:

| Command | Port | What it does |
|---|---|---|
| `cmd/alert-engine` | `TT_ALERT_ADDR` (`:8090`) — `/healthz`, `/metrics` | Evaluates monitors and KPI burn-rate rules against ClickHouse rollups, runs the incident state machine, root-cause analysis, writes `telemetry.sli_1m`, enqueues the `notifications` outbox |
| `cmd/notifier` | `TT_NOTIFIER_ADDR` (`:8091`) — `/healthz`, `/metrics` | Delivers the outbox to Slack, Microsoft Teams, PagerDuty, Opsgenie, generic webhooks and e-mail with retries |

Design docs: `.ai/alerting.md` (kinds, state machine, burn rate, RCA), `.ai/contracts.md`, `.ai/data-model.md`.

## Run

```bash
docker compose -f deploy/docker-compose.yml up -d        # postgres, clickhouse (schemas applied)
cd services/alerting
GOWORK=off go run ./cmd/alert-engine
GOWORK=off go run ./cmd/notifier
# images (build context services/)
docker build -f alerting/Dockerfile --build-arg CMD=alert-engine -t tt-alert-engine services/
docker build -f alerting/Dockerfile --build-arg CMD=notifier     -t tt-notifier     services/
```

`GOWORK=off` is only needed while other modules listed in `services/go.work` are missing; `go.mod` replaces
`services/pkg` with `../pkg`, so the module builds both inside and outside the workspace.

### Configuration

Common: `TT_POSTGRES_URL`, `TT_CLICKHOUSE_URL`, `TT_LOG_LEVEL` (see `services/pkg/config`).

| Variable | Default | |
|---|---|---|
| `TT_ALERT_INTERVAL` | `30s` | evaluation period |
| `TT_ALERT_REFRESH` | `5m` | full monitor/KPI reload even when the fingerprint did not change |
| `TT_ALERT_EVAL_TIMEOUT` | `10s` | per monitor query budget |
| `TT_ALERT_WORKERS` | `8` | concurrent monitor evaluations |
| `TT_ALERT_SLI_BACKFILL` | `6h` | how far back `sli_1m` is filled for a KPI without history |
| `TT_ALERT_SHARDS` / `TT_ALERT_MAX_SHARDS` | `1` / all | sharding (below) |
| `TT_ALERT_LOCK_KEY` | `0x7474616c657274` | advisory-lock key of shard 0 |
| `TT_ALERT_PROJECTS` | all | comma-separated project ids (dedicated deployments) |
| `TT_ALERT_STREAM` / `TT_ALERT_STREAM_GROUP` | `false` / `tt-alert-stream` | stream rules (below); brokers from `TT_KAFKA_BROKERS` |
| `TT_PUBLIC_APP_URL` (fallback `TT_PUBLIC_URL`) | `http://localhost:5173` | dashboard URL used in notification links (`/incidents?id=`); the notifier uses it for Slack "Open incident" when a payload has no link |
| `TT_SLACK_API_URL` | `https://slack.com/api` | notifier: Slack Web API base (tests) |
| `TT_NOTIFIER_POLL` / `_BATCH` / `_CONCURRENCY` / `_MAX_ATTEMPTS` | `2s` / `50` / `8` / `10` | outbox poller |
| `TT_NOTIFIER_SMTP_ADDR` / `_USER` / `_PASSWORD` / `_FROM` | — | e-mail relay (`host:587`, PLAIN auth when user set) |
| `TT_AI_QUOTA_CHECK_INTERVAL` | `5m` | alert-engine: AI quota threshold check (80 % / 100 % → kind `ai_quota` to `orgs.ai_notify`) |
| `TT_AI_PLAN_CREDITS` | `free=100,team=5000,enterprise=-1` | alert-engine: plan credits for the effective org limit (same value as codeintel / api) |

## Leader election and sharding

Each shard `k ∈ [0, N)` is owned by the session holding `pg_try_advisory_lock(TT_ALERT_LOCK_KEY + k)` on a dedicated
pooled connection. Every tick the engine pings that session; if it died, the locks are gone and the cache is dropped.
Standbys retry every tick, so failover takes ≤ one interval. With `N = 1` (default) this is plain leader election.
With `N > 1` an instance owns monitors/KPIs whose `id % N` is a held shard, and grabs at most `TT_ALERT_MAX_SHARDS`
shards, so `N = 4, MAX = 2` splits the work over two replicas (a third replica stands by). Changing ownership
resets the in-memory state (recovery hysteresis, KPI rule state, `sli_1m` watermarks), which is re-derived from
Postgres/ClickHouse. A consistent-hash ring or a lease table can replace the modulo later without touching evaluation.

## Evaluation loop

1. Elect, then reload monitors (`status = 'active'`, joined with `projects` for `org_id`) and KPIs when the
   fingerprint (count / `max(updated_at)` / `max(id)` of monitors, KPIs and rules) changes, or every `TT_ALERT_REFRESH`.
2. Evaluate each monitor (bounded worker pool, per-monitor timeout, panics recovered). Every ClickHouse query filters
   `org_id` + `project_id`; windows are complete minutes `[floor(now) − window, floor(now))`. Rollups are
   AggregatingMergeTree, so values are always re-aggregated (`sum`, `quantilesTDigestWeightedMerge`, `uniqMerge`).
3. Run the state machine and persist `monitors.state/state_since/last_value/last_eval_at`; on fire/resolve the
   monitor row, incident, `incident_events` row and outbox rows are written in one transaction.
4. Composites (after every other monitor, so they see this cycle's states), then reminders.
5. KPIs: append settled minutes to `sli_1m`, evaluate burn-rate rules, open/resolve KPI incidents, persist rule state.

Mutes come from the monitor row (the api bumps `updated_at` when `muted_until` changes, so the normal reload picks them up);
every tick also reads mutes (`monitors.muted_until > now`, covering direct edits and expiry) and active `maintenance_windows`. Unknown kinds (`custom`,
invalid JSON) and impossible rules (oldest_age on Kafka, messaging-system mismatch) are logged once and set to
`no_data`; a query error leaves the state untouched for that cycle.

### Monitor query shapes (`monitors.query`)

`monitors.target` (`{service, endpoint: "POST /checkout", process, view, env}`) scopes every kind; query fields override it.
`monitors.condition` is `{op: '>'|'>='|'<'|'<=', threshold, forMin, window: "5m", minRequests, renotifyMin, stream}` (`for: "5m"`
and `min_requests` also accepted; `minRequests` suppresses breaches on low traffic; `renotifyMin` → reminders, default 0 = off;
`stream: true` → stream rule for `log_match`/`error_new`). Ids in `monitorIds` may be numbers or strings.

| Kind | Source | `query` | Value |
|---|---|---|---|
| `latency` | `red_1m` | `percentile: p50…p99` (default p95) | ms |
| `error_rate` | `red_1m` | `statusMin: 500` → 5xx only, `400` → 4xx+5xx; default `is_error` | ratio |
| `throughput` (`throughput_change`) | `red_1m` | `direction: rpm \| drop \| spike \| change`, `baselineOffset` (default `168h`) | rpm, or relative change vs. the same window shifted back |
| `apdex` | `red_1m` | `tMs` (default 500); op defaults to `<` | 0..1 (CDF from a 101-level t-digest grid; errors = frustrated) |
| `metric_threshold` | → one of the above by `metric: error_rate \| p95 \| rpm \| apdex` | | |
| `log_count` (`log_match`) | `log_patterns_1m` (`logs` when `contains` is set) | `severity` or `minSeverity`, `patternId`, `contains` (case-insensitive) | count |
| `log_pattern` (`log_pattern_new`) | `log_patterns_1m` | `patternId` → its count; otherwise number of patterns first seen in the window that did not occur during `lookback` (7d) | count |
| `consumer_lag` | `broker_lag_1m`, `broker_groups_1m` | QueueRule: `{clusterId, group, topic?, metric, threshold, forMin, messagingSystem?}` | see below |
| `dlq` (`consumer_failures`) | `consumer_1m` | `metric: dlq \| failed \| retried \| failure_rate \| retry_rate`, `process`, `topic`, `consumerGroup`, `messagingSystem?` | count / ratio |
| `anomaly` | `red_1m` (`red_1h` when the window is whole hours) | `metric: error_rate (default) \| p50 \| p95 \| p99 \| rpm`, `direction: up \| down \| both` (default both), `minDeviation` (metric units, default 0), `weeks` (4); condition `threshold` = z (default 3), `window`, `forMin`, `minRequests` | robust z-score |
| `composite` | other monitors' `state` (Postgres) | `{op: and \| or, monitorIds: [..]}` (default `and`) | children firing |
| `job_missed` | `consumer_1m` (with `process`) or `red_1m` entry spans (with `route`/`target.endpoint`) | `intervalMin`, `graceMin` (default 10 % of interval, ≥ 1), `process?`, `topic?`, `consumerGroup?`, `messagingSystem?`; without `intervalMin` the allowance is `condition.window` (default 1h). Threshold/op are derived (`> intervalMin + graceMin`) | minutes since the last run |
| `error_new` | `errors_1h` | `service?` (or target), `lookback` (30d) | fingerprints first seen in the window with no earlier occurrence in the lookback |
| `rum_vital` | `rum_vitals_1m` | `app` (or target.service), `view`, `vital` (LCP), `percentile` (p75), `deviceType` | ms (CLS unitless) |
| `crash_rate` | `rum_views_1m` | `app`, `view`, `per: session \| view` | crashes / sessions |
| `infra` | `hosts_1m`, `containers_1m`, `k8s_pods_1m`, `k8s_nodes_1m`, `k8s_workloads_1m` | `{entity: host \| container \| pod \| node \| workload, metric, agg: avg (default) \| max, filter: {cluster?, namespace?, host?, workload?, service?}}`; `filter.service` defaults to `target.service`. Metrics: host `cpu \| memory \| disk \| load1`; container/pod `cpu_ratio \| memory_ratio \| restarts \| throttled`; pod/node `not_ready`; workload `unavailable` | worst entity (max; min for `<`/`<=` ops), or the count of not-ready entities |
| `faas` | `faas_1m`, `spans` (cold starts), Postgres `serverless_functions` | `{functionId? , filter: {provider?, region?, service?}, metric: invocations \| errors \| error_rate \| throttles \| duration_p95 \| duration_p99 \| cold_starts \| concurrency \| iterator_age_ms, relativeToTimeout?}` | worst function |
| `llm` | `llm_1m` (windows ≥ 6 h: whole hours from `llm_1h`, edges from `llm_1m`), Postgres `llm_prices` | `{filter: {service?, provider?, model?, route?, method?, operation?}, metric: cost_usd \| cost_usd_per_hour \| tokens \| input_tokens \| output_tokens \| calls \| errors \| error_rate \| p95_ms \| ttft_p95_ms}`; `filter.service` defaults to `target.service` | USD, USD/h, tokens, count, ratio or ms over the window |

**Infra.** Per entity: per-minute `max` over the AggregatingMergeTree rows (non-finite = not reported, skipped), then
`agg` over the window. `cpu_ratio`/`memory_ratio` = Σ usage / Σ limit of the entity's containers that report both
(pods aggregate their containers from `containers_1m`). `restarts` = increase of the cumulative counter in the window
(`max − min`; containers from `containers_1m`, pods from `k8s_pods_1m`). `not_ready` = number of pods/nodes whose newest
sample in the window has `ready = 0` (pods in phase `Succeeded` and entities whose newest sample is > 2 min older than the
window's newest — deleted pods, removed nodes — are ignored); value 0 when entities exist and all are ready.
`unavailable` = newest `desired − available` (≥ 0). Filters a table has no column for go through sub-selects
(`host IN (SELECT host FROM containers_1m …)`, `node IN (SELECT node FROM k8s_pods_1m …)`, workloads of a service via
`k8s_pods_1m`); pod `host` matches `node`. No entity with a value → `no_data`; unknown entity/metric → `no_data` (logged once).
`detail`: `{entity, metric, agg, matched, entities: [{id, cluster, namespace, pod, container, host, node, kind, name, phase, value}…]}`
(worst 5; `id` = `host`, `ns/pod/container` (or `host/container` outside Kubernetes), `ns/pod`, `node`, `ns/Kind/name`).

**FaaS.** Per function over the window: invocations/errors/throttles summed, `error_rate` = errors / invocations,
`duration_p95/p99` invocation-weighted mean of the minute values (`relativeToTimeout: true` → divided by
`serverless_functions.timeout_s × 1000`, template threshold 0.8), concurrency / iterator age max, `cold_starts` =
`sum(sample_weight)` of entry spans with `attrs['faas.coldstart'] = 'true'` of the function's `service`. A service filter
(`filter.service`, else `target.service` when no `functionId`), cold starts and `relativeToTimeout` resolve functions from
`serverless_functions` (`missing = false`; table absent → `no_data`); otherwise `faas_1m` is filtered directly by
`function_id` / `provider` / `region`. With `condition.minRequests`, functions below it do not decide `error_rate` /
`duration_*` breaches. A metric no function reports in the window (e.g. Azure throttles) → `no_data`.
`detail`: `{metric, matched, entities: [{id, provider, region, service?, invocations, value}…]}`.

**LLM** (ADR-023, contracts §25). One query grouped by service × provider × model over the window: sums of calls,
errors, input / output / cache read / cache write tokens, and for `p95_ms` / `ttft_p95_ms` the merged
`quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)` digests (same levels as the rollup state; overall p95 from a second
query over all groups). `tokens` = input + output (input already includes cache tokens). Cost per group comes from
`services/pkg/llmprice` (embedded defaults + the org's `llm_prices` rows, cached 1 min per org; a failed load uses the
defaults): (input − cache read − cache write) × input + cache read × cacheRead + cache write × cacheWrite + output × output.
Groups without a price are excluded from `cost_*` (listed in `detail.unpricedModels`); calls but no priced group → `no_data`;
no calls → $0. `cost_usd_per_hour` = window cost ÷ window hours. `error_rate` / `p95_ms` / `ttft_p95_ms` are `no_data`
without calls (TTFT also without streaming calls); with `condition.minRequests` fewer calls never breach.
`detail`: `{metric, window, source, calls, errors, contributors: [{service, provider, model, value, share, calls}…] (top 5),
topModels, topServices, unpricedModels?, topShare, summary}`; share = part of the value (errors for `error_rate`, calls for
latency). The firing incident's root cause is the summary ("LLM cost $34.20/h above $20/h — claude-sonnet-4-5 in
storefront-bff 81%") with the top contributor as root node and its share as confidence (no call-tree RCA for `llm`); the
notification carries the summary without a confidence suffix.

**Templates** (`internal/eval/templates.go`, `eval.Templates`; kind + query + condition as stored, `source = 'template'`):
`k8s-pod-crashloop` (pod restarts > 3, window 10m), `k8s-node-not-ready` (> 0, for 5m), `host-cpu-high` (avg > 0.9, for 10m),
`host-disk-full` (max > 0.9), `k8s-workload-unavailable` (> 0, for 10m), `faas-error-rate` (> 0.05, for 5m, minRequests 20),
`faas-throttles` (> 0, for 5m), `faas-near-timeout` (p95 / timeout > 0.8), `llm-cost-per-hour` (`cost_usd_per_hour` > 20,
window 1h), `llm-error-rate` (> 0.05, window 15m, minRequests 20), `llm-p95-slow` (`p95_ms` > 20000, window 15m,
minRequests 20), `llm-token-spike` (`tokens` > 5M, window 1h).

**Anomaly.** The baseline is the same minute-of-week in each of the last `weeks` weeks, widened by ±1 h and cut into
window-sized buckets aligned with the current window (5m window → 25 buckets/week, 100 over 4 weeks). z = (current −
median) / (1.4826 · MAD); a flat baseline (MAD 0) falls back to 1.2533 · mean absolute deviation, and a deviation from a
constant baseline is ±1000 (cap). Fewer than 8 usable buckets (rates need ≥ `max(minRequests, 1)` requests) → `no_data`.
Breach: `direction` vs `threshold`, and `|current − median| ≥ minDeviation`. `last_value` is the signed z; the detail
(`incident_events.data.detail`) carries `current`, `median`, `mad`, `samples`.

**Job missed.** The last run is the latest minute with `processed > 0` (consumer_1m) or `count > 0` (red_1m) within
`max(3 × allowance, 24 h)` (≤ 7 d), the running minute included. Never seen in that lookback → value = lookback.

**Consumer lag** mirrors `apps/web/src/lib/lagAlerts.ts` (`ruleSeries`/`evaluate`) on per-minute broker data:
`lag` (sum over partitions), `time_behind` (lag ÷ summed produce rate, rate 0 → 1 like the UI), `partition_lag`
(max single partition), `growing` (slope of a 5-point moving average over 4 points, threshold 0), `not_drained` (hours
since lag was last < 5 % of the lookback peak), `rebalances` (per hour as reported), `oldest_age` (seconds: max
`broker_lag_1m.oldest_age_s` over the group's queues/partitions; minutes where it is not reported (−Inf) are skipped and
forward-filled). `oldest_age` is only defined for queues: the messaging system comes from `broker_clusters.kind`
(`sqs → aws_sqs`, `pubsub → gcp_pubsub`, `rabbitmq`, `kafka`) of `clusterId`; `query.messagingSystem` must match it
when both are known. On Kafka the monitor is `no_data` (use `time_behind`). Gaps are forward-filled; a group
without a sample in the last ~6 minutes is `no_data`. The trailing breach run is passed to the state machine as
`HeldFor`, so a rule that has been breaching for `forMin` fires immediately (also after a restart/failover).

## State machine

`ok → pending → firing → ok` (`internal/statemachine`, pure): a breach enters `pending` and fires once held for
`forMin` (`forMin = 0` fires at once). Entering `pending` opens an incident with `state = 'pending'` (event `pending`,
no notification); firing promotes it to `firing` (same id and `started_at`, event `fired`, notifications); if the
condition goes away first (or the data disappears) it is resolved with `quiet = true` (event `resolved` `{quiet: true}`),
never notified. Still one open incident per monitor (`incidents_one_open`). `firing` resolves only after
`max(forMin/2, 1m)` of clean evaluations (flapping guard, alerting.md); a flap resets that clock. `no_data` never
resolves a firing monitor. On fire: `incidents` row (`ON CONFLICT` on the one-open-per-monitor index, so retries are
idempotent), `fired` event, outbox rows. While firing `peak_value` follows the worst value (min for `<` conditions).

### Mute and maintenance

A monitor with `muted_until > now`, or covered by an active `maintenance_windows` row (`starts_at ≤ now < ends_at`;
`scope` `{}` = whole project, `{services: [..]}` matches the monitor's service (case-insensitive), `{monitorIds: [..]}`),
is still evaluated and its state/incidents recorded, but nothing is enqueued: the `fired`/`resolved` event carries
`{suppressed: true, reason: "muted" | "maintenance:<id>"}` and a `suppressed` event `{reason, notification, channels}`
is added. KPI rules honour maintenance windows (project / service scope). A resolve is only notified to channels that
were sent the firing notification, so an incident that fired while muted resolves silently too.

### Reminders

With `condition.renotifyMin > 0`, a still-firing incident that is not acknowledged gets reminder number
`n = floor((now − fired) / renotifyMin)` once (event `reminder` `{n}`; outbox kind `reminder`, dedupe key
`incident:<id>:reminder-<n>:<channel>`, payload = firing payload + `reminder: n`, current `value`, `peak`). Acknowledged
or pending incidents never remind; while suppressed a `suppressed` event `{notification: "reminder", n}` is logged instead.
KPI rules remind the same way from `kpi_alert_rules.condition.renotifyMin` (`008_alerting_r3.sql`, `{}` default; payload =
KPI firing payload + `reminder`, current burn value, `peak`).

### Stream rules (`TT_ALERT_STREAM=true`)

`log_match`/`log_count` and `error_new` monitors with `condition.stream = true` are also fed from Redpanda
(`internal/stream`, franz-go, group `tt-alert-stream`, topics `tt.otlp.logs` / `tt.otlp.traces`, starting at the end:
no replay). Only the holder of shard 0 (the *stream leader*) joins the group, so standbys and other shards never take
partitions. With `TT_ALERT_SHARDS > 1` it evaluates the stream rules of **every** shard (`LoadStreamMonitors`); a hit on
a monitor of a shard it does not hold is handed to the owner: a row in the unlogged table `alert_stream_hits`
(`kind` `breach` with value/detail, or `eval` = re-check in ClickHouse) plus `NOTIFY tt_stream_hit` in the same
transaction. Every replica runs `RunStreamHandoff` (only with more than one shard): `LISTEN tt_stream_hit` + a 1 s poll,
`DELETE … RETURNING` of the rows of its shards (`FOR UPDATE SKIP LOCKED`), collapsed per monitor (largest breach wins)
and applied escalate-only — typically < 100 ms after the record. Unclaimed rows older than 5 min are purged by the stream
leader. Metric `tt_alert_stream_handoffs_total{dir=sent|received|dropped|error}`.
Records are routed by the `project_id` header and decoded only when the project has stream rules.

- `log_match`: service / env / severity / `contains` are matched in-process (same severity mapping as ingest-writer);
  the count over `window` (1 s buckets) is compared with the condition and a breach goes straight to the state machine
  (≈ 1 s). Below the threshold (the in-memory window only knows what arrived since leadership) or with a `patternId`
  (Drain ids are computed by the writer), a ClickHouse re-evaluation is scheduled at +2 s and +6 s.
- `error_new`: an error span (status ERROR, or HTTP ≥ 500 on a non-internal span) of a matching service schedules the
  `errors_1h` check at +2 s and +6 s, so fingerprints are exactly the writer's.

Stream results only escalate (ok → pending → firing); recovery and resolution stay with the periodic loop, which keeps
evaluating the same monitors. Evaluations of one monitor are serialised.

## KPIs (burn rate)

`kpis.target` `{kind: endpoint|consumer|view|page, service, ref}` + `good_when` `{latencyMs, statusBelow, lcpMs, vital}`:

- availability / success_rate: good = total − bad (bad = 5xx when `statusBelow ≥ 500`, 4xx+5xx when ≤ 400, else errors; failed
  for consumers; JS errors / crashes for views),
- latency: good = non-failed events × CDF(threshold) from the t-digest grid (default 300 ms; views use LCP when `lcpMs`/`vital`).

Settled minutes (older than 2 min) are written once to `sli_1m` (it sums on merge); the watermark is
`max(minute)` read back on (re)acquisition. Rules: `burn_rate` fires when both `long_window` and `short_window`
(defaults 1h and long/12) burn faster than `threshold` (burn = (1 − good/total)/(1 − objective)); `below_target`
fires when the good ratio over `long_window` (15m) is below `threshold` (0.97 or 97). KPI incidents carry `kpi_id`,
`kpi_rule_id` (one open incident per rule: `incidents_one_open_kpi_rule`), a deterministic title per rule and RCA for
endpoint targets. Rule state is persisted every evaluation in `kpi_alert_rules.state/state_since/last_value/last_eval_at`
and reloaded on (re)acquisition (an open incident forces `firing`; a `firing` row without one restarts at `ok`).
Incidents opened before `kpi_rule_id` existed are still found by kpi + title.

## Root cause analysis

`internal/rca` is a port of `apps/web/src/lib/rca.ts` (same delta attribution, parallel critical path, 15 %
alternatives, own-code remainder, summaries — verified against the TS output in `testdata/golden.json`). On fire for
latency/apdex (ms) and error-rate (ratio) monitors the tree is built from ClickHouse: root endpoint (`red_1m`),
outgoing edges (`edges_1m`: db / cache / queue / external), service-to-service HTTP edges expanded into the callee
routes this caller hits (`callers_1m`) and recursed (depth 4, 60 nodes); deploys of the last 2 h become own-code
evidence. Windows: current = last 15 min, baseline = the hour before. The result (children stripped, `RcaResult`
JSON shape, incl. `confidence`) goes to `incidents.root_cause` in the same transaction that enqueues the firing
notification; the payload carries `summary` and `rootCause {summary, confidence}` (KPI endpoint targets too).

**Infra candidates** (`rca/infra.go`, `eval.InfraSignals`): for every incident with a service (monitors of any kind and
KPIs) the engine also reads the last 15 minutes of `k8s_pods_1m` for that `service` (reasons seen, restart increase) and
`hosts_1m` of the hosts running it (`containers_1m` hosts + the pods' nodes, average CPU). Candidates: pod `OOMKilled`
0.8, `CrashLoopBackOff` 0.75, restarts only 0.6 (+0.05 for ≥ 3 restarts, +0.05 when several pods of the service are
affected); host CPU > 0.9 → 0.6 + (cpu − 0.9) × 2.5; clamped to 0.6–0.85, top 5. If there is no call tree, it ends in
the service's own code, explains nothing (empty path) or is less confident, the best candidate becomes `summary` /
`confidence` (e.g. "Pod api-1 restarted (OOMKilled).", which Slack shows as the likely cause) and the tree's answer
moves to `treeSummary` / `treeConfidence`. `root_cause.infra` = `[{kind: pod|host, entity, cluster?, node?, reason:
OOMKilled|CrashLoopBackOff|restarts|cpu, restarts?, value?, title, confidence}]`; the candidates are also appended to
`root.evidence` as `{kind: "resource", title, detail: "<cluster> · NN% confidence"}`. Signal query errors (e.g. tables
missing) are logged and ignored.

Notification payloads also carry `deploy` (newest `deploy_markers` row of the service in the hour before firing:
`version, source, env, at, url?, commitMessage?, author?`) and, for alert-as-code monitors, `source` / `codeRef`
(`monitors.source`, `monitors.code_ref` = `repo:path`; read via `to_jsonb` so the engine works before and after
`005_codeintel.sql`). Code monitors evaluate like any other monitor; channels render `codeRef` as "Defined in code".

## Notifier

Polls `notifications` (`sent_at IS NULL AND next_at <= now() AND attempts < max`) with `FOR UPDATE SKIP LOCKED`,
leases rows by pushing `next_at` 2 min ahead, delivers outside the transaction, then sets `sent_at` (+ a `notified`
incident event) or `attempts/error/next_at`. Retry: 30 s × 2^(n−1), capped at 1 h, ±20 % jitter. 4xx (except 408/429),
bad config and unknown channels are terminal (`attempts = max`, `error` kept). Channel names are free text on monitors:
exact `notification_channels.name` match in the incident's project, else a `type:` prefix (`slack:#payments`) is stripped
and must match the type. Config values may be references: `"secret:<id>"` (any depth — URLs, keys, header values, `to`
lists) is resolved with `services/pkg/secrets.Resolve` against the `secrets` table (AES-256-GCM, `TT_SECRETS_KEY`, AAD =
project id; cached 60 s per project + ref; missing row / bad ref / decryption failure is terminal, a database error is
retried), `"env:NAME"` is read from the environment; plain values work as before:

| type | config | payload |
|---|---|---|
| `slack` | `webhookUrl` | Block Kit: header, fields (severity, value vs threshold, target, started), likely cause (+ confidence), code ref, "Open incident" button |
| Slack App (`slack:<channel id>#<name>`) | — (org bot token) | see *Slack App* below |
| `pagerduty` | `routingKey`, `url?` | Events v2 `trigger` / `resolve`, `dedup_key = tt-incident-<id>` |
| `opsgenie` | `apiKey`, `url?` | create alert (alias = dedup key) / close by alias |
| `webhook` | `url`, `headers?`, `secret?` (HMAC-SHA256 → `X-Trooper-Signature`) | `{version: 1, event, incident: payload}` |
| `email` | `to` (string or list), `from?` | plain-text RFC 5322 via `TT_NOTIFIER_SMTP_*` |
| `teams` | `webhookUrl` (Incoming Webhook or Power Automate Workflows URL) | `{type: "message", summary, attachments: [{contentType: "application/vnd.microsoft.card.adaptive", content: AdaptiveCard 1.4}]}`: coloured title (attention/warning/accent/good), FactSet (severity, value, target, started/resolved, reminder), likely cause, "Open incident" `Action.OpenUrl` |

Targets that need no `notification_channels` row: `slack:<channel id>#<name>` (Slack App, channel id `[CGDZ][A-Z0-9]{6,}`)
and `slack:https://hooks.slack.com/…` (webhook URL; other hosts are not accepted). Channel prefixes: `slack:`, `teams:` (or `msteams:`), `pagerduty:`, `opsgenie:`, `webhook:`, `email:`.

Notification kinds: `firing`, `resolved`, `reminder`, `acknowledged` (Slack App targets only). Reminders read "Still firing (reminder n): …" (Slack prefixes
:repeat:, Teams adds a Reminder fact); PagerDuty re-sends `trigger` and Opsgenie re-creates with the same dedup key/alias
(both append to the open alert instead of paging anew); webhooks get `event: "reminder"` and `incident.reminder = n`.

Outbox `dedupe_key` = `incident:<id>:<firing|resolved|reminder-<n>|acknowledged>:<channel>`. A `429` with `Retry-After`
(Slack) schedules the retry after that delay instead of the backoff.

### Slack App (ADR-017, contracts §17)

Target `slack:<channel id>#<name>` → `chat.postMessage` with the bot token of the org's Slack installation (newest
`integrations(provider='slack', status <> 'disabled')` row of any project of the org; `credentials_ref` `secret:<id>` resolved
with the installing project as AAD, or `env:NAME`). The first message's `ts` is stored in `notification_messages`
(`provider='slack', external_id=ts, external_channel`); a retried firing row does not post twice. `resolved` / `reminder` /
`acknowledged` reply in that thread (`thread_ts`) and `chat.update` the original (best effort) with the new status
colour and buttons; without a stored message they post a new one. The engine (shard-0 holder) enqueues `acknowledged`
for Slack App targets of incidents acknowledged in the last day (`EnqueueAcks`, payload = firing payload + `ackedBy`, `ackedAt`).

Message: header `<emoji> <status>: <title>`, coloured attachment (critical `#E01E5A`, warning `#ECB22E`, info `#36C5F0`,
acknowledged `#7C7C7C`, resolved `#2EB67D`) with fields (status, severity, service · endpoint, value vs threshold,
firing for / duration), **Likely cause: `<summary>` (NN% confidence)** (`· low confidence` below 30 %, omitted without
RCA), context (deploy of the last hour, code ref) and buttons *Open incident* (payload link or `TT_PUBLIC_APP_URL`),
*Ack* (`action_id` `tt_ack:<incidentId>`) and *Mute 1h* (`tt_mute1h:<monitorId>`, monitors only) — handled by the api's
`POST /slack/interactions`; Ack / Mute disappear once acknowledged / resolved.

Errors: `not_in_channel` → `conversations.join` (public channels; needs the `channels:join` scope — with `chat:write.public` public channels
normally never answer `not_in_channel`) and one retry; a channel the bot cannot
join (private) → terminal "invite the app"; `channel_not_found`, `invalid_auth`/`token_revoked`/`missing_scope`…,
`is_archived`, invalid message → terminal with a clear error; HTTP 429 → retry after `Retry-After`; 5xx / other Slack
errors → backoff. App not installed for the org → terminal.

## Metrics

`tt_alert_leader`, `tt_alert_shards_held`, `tt_alert_monitors`, `tt_alert_kpis`, `tt_alert_evaluations_total{kind,outcome}`,
`tt_alert_transitions_total{to}`, `tt_alert_cycle_seconds`, `tt_alert_last_cycle_timestamp_seconds`, `tt_alert_rca_total{outcome}`,
`tt_alert_sli_minutes_written_total`, `tt_alert_reminders_total`, `tt_alert_stream_records_total{topic}`,
`tt_alert_stream_matches_total{kind}`, `tt_alert_stream_handoffs_total{dir}`, `tt_notifier_deliveries_total{outcome,type}` (type `slack_app` for
Slack App targets).

## Tests

```bash
GOWORK=off go test ./...                                            # unit (table-driven)
GOWORK=off go test -tags integration -count=1 -v ./integration      # live Postgres + ClickHouse
```

- `statemachine`: transition sequences incl. hysteresis, flaps, no_data, `HeldFor`.
- `lag`: every metric + `evaluate` against golden output of the verbatim TS formulas (`internal/lag/testdata/golden.mjs`).
- `rca`: `FindRootCause` against `rca.ts` run under node (`internal/rca/testdata/golden.ts`); tree building over a fake source.
- `burn`, `eval` (CDF/apdex/condition parsing, robust z / direction / baseline ranges, job allowance, oldest_age),
  `kpi` (good-event rules), `engine` (KPI rule checks/titles, composite and/or), `store` (mute/maintenance scope).
- `notify`: Slack/Teams/PagerDuty/Opsgenie/e-mail/webhook builders incl. reminders, httptest delivery + status
  classification, retry schedule.
- `notify` Slack App: fake Slack Web API (httptest) — firing message (likely cause + confidence, Ack/Mute action ids, deploy,
  code ref), no double post on retry, resolved/ack/reminder thread replies + `chat.update`, `not_in_channel` → join,
  private channel / `channel_not_found` / `invalid_auth` terminal, 429 `Retry-After`, target parsing.
- `stream`: log matching, error-span rule, sliding window, processor (OTLP protobuf records → apply / scheduled checks).
- `integration`: org/project 9201 — synthetic spans, logs, broker lag and RUM events; 14 monitors of every kind + a KPI;
  one tick asserts states, incidents, RCA (`pay-db` 50× slower; error path into `itest-pay`), outbox rows, `sli_1m`;
  the notifier delivers to an httptest webhook (unknown channel → terminal error); recovery resolves the incident.
  Leader election/failover/shard split. `TestRound2`: pending → quiet resolve, mute and maintenance suppression,
  reminders (acknowledged incident excluded, idempotent), anomaly up/down on 4 weeks of seeded history, composite
  and/or, job_missed (consumer + route), oldest_age on SQS (fires) vs Kafka (no_data), persisted KPI rule state +
  `kpi_rule_id`, a stream log rule firing without a tick, Teams adaptive cards (firing + reminder) delivered by the
  notifier. Everything for 9201 is deleted afterwards (`TT_ITEST_KEEP=1` keeps it).
  `TestRound3` (org/project 9601, two engines × two shards): the stream leader evaluates all stream rules, a hit on shard 1's
  monitor is handed off and fires on engine B in < 1.5 s (`eval` hits claimed too); alert-as-code monitor (`code_ref`) fires with
  `codeRef`, RCA confidence and deploy context in the payload; KPI rule reminder (`condition.renotifyMin`); Slack App via a fake
  Slack API: firing messages, `notification_messages`, ack + reminder thread replies with `chat.update`. Creates
  `notification_messages` / the `slack` provider check only when `009_slack_github.sql` is missing, and reverts that afterwards.
  `TestInfraFaaS` (org/project 9601): synthetic hosts / containers / pods (incl. a deleted not-ready pod) / nodes /
  workloads / functions + cold-start spans; infra and faas monitors (worst entity, `detail.entities`, `no_data` for
  unreported gauges, `relativeToTimeout`, cold starts via `serverless_functions` when `011_serverless.sql` is applied) and
  an error-rate incident whose root cause is the OOMKilled pod (summary in `incidents.root_cause` and the firing payload).
  `TestLLM` (org/project 9602): `llm_1m` rows (llm_1h via its MV) of priced / unpriced models; llm monitors on cost per hour
  (top contributor in the root cause and payload, unpriced models listed), error rate with / without enough calls, p95,
  tokens over 24 h (hourly + edge minutes) vs 1 h, TTFT `no_data`, an unpriced model `no_data`, and an org `llm_prices` row.
- `eval` infra/faas: worst-entity selection, entity ids, filter → SQL (placeholder/arg count), unsupported shapes, faas
  minute aggregation, templates decode into supported monitors; `rca` infra: candidate confidences and merge rules.
- `eval` llm: every metric's value / contributors / shares, unpriced → `no_data`, org override pricing, summary text,
  `llm_1m` vs `llm_1h` + edges source (placeholders vs args, hour bounds); `engine` llm root cause.

## Not yet

`group_by` dedupe, graph snapshots in notifications,
`log_pattern_new` as a stream rule (needs the writer's Drain ids), cron-expression schedules for `job_missed`.

## AI quota notifications

The alert-engine sums this month's `ai_usage` per org every `TT_AI_QUOTA_CHECK_INTERVAL` and enqueues one `ai_quota`
notification (no incident) per `orgs.ai_notify` target when an org reaches 80 % / 100 % of its limit; `ai_quota_alerts`
dedupes per org + month + threshold, so every replica may run it. The notifier renders a single line for every channel
type ("AI usage at 80% of this month's credits (4,012 / 5,000). Resets Nov 1."); Slack App targets get a plain
`chat.postMessage` without thread or buttons. `email:<address>` targets need no `notification_channels` row.
Details: `.ai/alerting.md` § Bildirim, `.ai/contracts.md` §19.
