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
| `TT_PUBLIC_URL` | `http://localhost:5173` | dashboard URL used in notification links (`/incidents?id=`) |
| `TT_NOTIFIER_POLL` / `_BATCH` / `_CONCURRENCY` / `_MAX_ATTEMPTS` | `2s` / `50` / `8` / `10` | outbox poller |
| `TT_NOTIFIER_SMTP_ADDR` / `_USER` / `_PASSWORD` / `_FROM` | — | e-mail relay (`host:587`, PLAIN auth when user set) |

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

### Stream rules (`TT_ALERT_STREAM=true`)

`log_match`/`log_count` and `error_new` monitors with `condition.stream = true` are also fed from Redpanda
(`internal/stream`, franz-go, group `tt-alert-stream`, topics `tt.otlp.logs` / `tt.otlp.traces`, starting at the end:
no replay). The group is joined only while the instance holds a shard, so standbys do not steal partitions; with
`TT_ALERT_SHARDS > 1` records for monitors owned by another replica are ignored there (the periodic evaluation still
covers them). Records are routed by the `project_id` header and decoded only when the project has stream rules.

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
JSON shape) goes to `incidents.root_cause`, and its summary into the notification payload.

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
| `slack` | `webhookUrl` | Block Kit: header, fields (severity, value vs threshold, target, started), likely cause, "Open incident" button |
| `pagerduty` | `routingKey`, `url?` | Events v2 `trigger` / `resolve`, `dedup_key = tt-incident-<id>` |
| `opsgenie` | `apiKey`, `url?` | create alert (alias = dedup key) / close by alias |
| `webhook` | `url`, `headers?`, `secret?` (HMAC-SHA256 → `X-Trooper-Signature`) | `{version: 1, event, incident: payload}` |
| `email` | `to` (string or list), `from?` | plain-text RFC 5322 via `TT_NOTIFIER_SMTP_*` |
| `teams` | `webhookUrl` (Incoming Webhook or Power Automate Workflows URL) | `{type: "message", summary, attachments: [{contentType: "application/vnd.microsoft.card.adaptive", content: AdaptiveCard 1.4}]}`: coloured title (attention/warning/accent/good), FactSet (severity, value, target, started/resolved, reminder), likely cause, "Open incident" `Action.OpenUrl` |

Channel prefixes: `slack:`, `teams:` (or `msteams:`), `pagerduty:`, `opsgenie:`, `webhook:`, `email:`.

Notification kinds: `firing`, `resolved`, `reminder`. Reminders read "Still firing (reminder n): …" (Slack prefixes
:repeat:, Teams adds a Reminder fact); PagerDuty re-sends `trigger` and Opsgenie re-creates with the same dedup key/alias
(both append to the open alert instead of paging anew); webhooks get `event: "reminder"` and `incident.reminder = n`.

Outbox `dedupe_key` = `incident:<id>:<firing|resolved|reminder-<n>>:<channel>`.

## Metrics

`tt_alert_leader`, `tt_alert_shards_held`, `tt_alert_monitors`, `tt_alert_kpis`, `tt_alert_evaluations_total{kind,outcome}`,
`tt_alert_transitions_total{to}`, `tt_alert_cycle_seconds`, `tt_alert_last_cycle_timestamp_seconds`, `tt_alert_rca_total{outcome}`,
`tt_alert_sli_minutes_written_total`, `tt_alert_reminders_total`, `tt_alert_stream_records_total{topic}`,
`tt_alert_stream_matches_total{kind}`, `tt_notifier_deliveries_total{outcome,type}`.

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
- `stream`: log matching, error-span rule, sliding window, processor (OTLP protobuf records → apply / scheduled checks).
- `integration`: org/project 9201 — synthetic spans, logs, broker lag and RUM events; 14 monitors of every kind + a KPI;
  one tick asserts states, incidents, RCA (`pay-db` 50× slower; error path into `itest-pay`), outbox rows, `sli_1m`;
  the notifier delivers to an httptest webhook (unknown channel → terminal error); recovery resolves the incident.
  Leader election/failover/shard split. `TestRound2`: pending → quiet resolve, mute and maintenance suppression,
  reminders (acknowledged incident excluded, idempotent), anomaly up/down on 4 weeks of seeded history, composite
  and/or, job_missed (consumer + route), oldest_age on SQS (fires) vs Kafka (no_data), persisted KPI rule state +
  `kpi_rule_id`, a stream log rule firing without a tick, Teams adaptive cards (firing + reminder) delivered by the
  notifier. Everything for 9201 is deleted afterwards (`TT_ITEST_KEEP=1` keeps it).

## Not yet

`group_by` dedupe, graph snapshots in notifications, reminders for KPI rules (`kpi_alert_rules` has no condition),
`log_pattern_new` as a stream rule (needs the writer's Drain ids), cron-expression schedules for `job_missed`.
