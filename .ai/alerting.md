# Alerting

## Monitor types

| Type | Example | Evaluation |
|---|---|---|
| `metric_threshold` | `POST /checkout` error rate > 2%, 5 min | Periodic (30–60 s), `red_1m` |
| `latency` | p95 > 800 ms, 10 min | Periodic |
| `throughput_change` | Throughput dropped/rose 50% vs. the same hour last week | Periodic, seasonal baseline |
| `anomaly` | Statistical deviation in error rate | Periodic, robust z-score (MAD) + weekly seasonality |
| `log_match` | Count of `level=error AND body:"payment declined"` > 20/5min | Stream (event from ingest-writer) + periodic verification |
| `log_pattern_new` | A never-before-seen error log pattern appeared | Stream |
| `error_new` | New error fingerprint (new issue) | Stream |
| `consumer_lag` | On broker data, per consumer group (+ optional topic): lag > N, time behind (lag ÷ produce rate) > N s, any partition > N, lag growing for X min, batch consumer not drained in N hours, rebalances/hour > N. The editor replays the last 24 hours ("would have fired N×") and suggests a threshold per group (batch → not drained, falling behind → growing, others → 7-day p99 time-behind × 2). The monitor carries a `queue` field | Periodic (15 s broker scrape) |
| `job_missed` | Cron job did not run within the expected time (heartbeat) | Periodic |
| `rum_vital` | p75 LCP > 2.5 s on the `/checkout` page | Periodic, `rum_views_1m` |
| `composite` | A AND B (e.g. error rate high AND throughput normal) | States of the child monitors |
| `kpi_burn` | The "Homepage loads fast" KPI's error budget is burning 6× fast over 1 hour | Periodic, `sli_1m`; multi-window burn rate (below) |
| `infra` | Pod restarted more than 3 times in 10 min; node NotReady; host CPU > 90%; host / database memory > 90%; node or cluster disk > 85%; database connections > 80% of max; missing replicas in a workload | Periodic, `hosts_1m` / `containers_1m` / `k8s_*_1m` / `db_1m` (ADR-019) |
| `faas` | Error rate > 5% on a Lambda/Azure function, throttles, p95 exceeded 80% of the timeout | Periodic, `faas_1m` + `spans` for cold starts (ADR-020) |
| `llm` | Hourly estimated LLM cost > $20; `gpt-5` error rate > 5%; Anthropic p95 > 20 s; token spike | Periodic, `llm_1m`/`llm_1h` + price table (ADR-023, contracts §25) |

## Rule schema (same as alert-as-code)

```yaml
name: Checkout error rate
kind: metric_threshold
query:
  source: red
  service: orders-api
  route: POST /checkout
  metric: error_rate
condition:
  op: '>'
  threshold: 0.02
  for: 5m
  min_requests: 50          # prevents false positives at low traffic
severity: critical           # info | warning | critical
notify: [slack:#payments-alerts, pagerduty:payments]
labels: { team: payments }
```

Monitors are managed from the UI or from `.trooper/monitors/*.yaml` files in the repo (two-way sync; repo-sourced monitors are read-only in the UI).
Repo-sourced monitors (`monitors.source='code'`, `code_ref` = `repo:path`) are evaluated by the engine like any other;
the notification payload carries `codeRef` and channels show it as "Defined in code".

## Incident state machine

`ok → pending (for duration elapsing) → firing → acknowledged → resolved`
- Flapping protection: resolving requires clean data for half the `for` duration.
- Dedupe key: `monitor_id + group_by values`.
- On entering `pending` a `pending` incident is opened and no notification is sent; on moving to `firing` the same incident is escalated and the notification is sent. If it returns from `pending` without firing, the incident is resolved with `quiet=true` (the UI hides it in the default list). One open incident per monitor.
- **Mute / maintenance window:** `monitors.muted_until` and `maintenance_windows` (monitor/service scoped; KPI rules included). Evaluation and state recording continue, only delivery is not queued; events carry `suppressed:true` + a `muted` / `maintenance:<id>` reason, and a `suppressed` event is also written. Resolve goes only to channels that received the firing notification.
- **Reminder:** `condition.renotifyMin`; the n-th reminder is sent once per incident (dedupe `incident:<id>:reminder-<n>:<channel>`). Not sent to acknowledged/pending incidents. PagerDuty/Opsgenie add it to the open alert with the same key.
  KPI rules work the same way: `kpi_alert_rules.condition.renotifyMin` (`008_alerting_r3.sql`).
- Incident event kinds: `pending`, `firing`, `acknowledged`, `resolved`, `suppressed`, `reminder`.
- Outbox (`notifications.kind`): `firing`, `resolved`, `reminder`, `acknowledged` (Slack App targets only; written to the thread
  of the acked incident's firing message).

### Evaluation details (alert-engine)

| Type | Source / algorithm |
|---|---|
| `anomaly` | Same minute-of-week ±1 hour over the last 4 weeks, `red_1m` (`red_1h` for whole-hour windows); robust z-score |
| `composite` | `and`/`or` over the states of other monitors; evaluated after all other monitors |
| `job_missed` | `consumer_1m` (process) or `red_1m` (route); value = minutes since the last run |
| `error_new` | `errors_1h` history; on the stream path a ClickHouse check at +2 s / +6 s |
| `consumer_lag` `oldest_age` | `broker_lag_1m.oldest_age_s`; messaging system from `broker_clusters.kind` (sqs→`aws_sqs`, pubsub→`gcp_pubsub`); `no_data` on a Kafka cluster |
| `log_match` (stream) | `TT_ALERT_STREAM=true`; franz-go group `tt-alert-stream`, consumed only by the owner of shard 0 (stream leader); in-memory counting, firing in ~1 s. With multiple shards the stream leader evaluates the stream rules of **all** shards; a match on another shard's monitor is handed to its owner via an `alert_stream_hits` (unlogged) row + `NOTIFY tt_stream_hit`, and the owner picks it up with `LISTEN` + 1 s poll (~100 ms) and only escalates. The stream only escalates; resolve happens in the periodic loop |
| `infra` | `query: {entity: host\|node\|cluster\|container\|pod\|workload\|database, metric, agg: avg (default)\|max, filter: {cluster, namespace, host, workload, service}}` (`target.service` replaces filter.service; database: `filter: {database, engine}`). Metrics per entity: contracts §20. Window value per entity (per-minute `max`, then `agg`; a cluster takes the per-minute **mean** of its nodes); the monitor value is the worst entity (the smallest if op is `<`), `detail.entities` the worst 5 (`id`, labels, `value`). `restarts` = increase within the window; `cpu_ratio`/`memory_ratio` = Σ usage / Σ limit (containers with a limit); pod `not_ready` / node `not_ready` = **count** of entities whose last sample has ready=0 (excluding phase `Succeeded` and — deleted — entities 2 min older than the window's newest minute); workload `unavailable` = latest `desired − available`. `no_data` if the matching entity has no value at all |
| `faas` | `query: {functionId? \| filter: {provider, region, service}, metric, relativeToTimeout?}`. Per function: counts summed, `error_rate` = errors/invocations, `duration_p95/p99` invocation-weighted average (`relativeToTimeout` → ÷ timeout), `concurrency`/`iterator_age_ms` max, `cold_starts` = entry spans with `faas.coldstart='true'` in `spans` of the function's service. The `service` filter, cold start and timeout are resolved from Postgres `serverless_functions`. Functions below `minRequests` do not decide a breach on ratio/duration metrics. Unreported metric (e.g. Azure throttles) → `no_data` |

**Agent monitors (ADR-032):** those created via MCP with `tt_create_monitor` / `tt_create_kpi` with user approval
carry `source='agent'`; the engine evaluates them like `manual`. "Agent" badge in the UI (Bot icon); the agent can only delete
what it created. Flow: `skills/trooper-alerts`.

**Templates** (`services/alerting/internal/eval/templates.go`, `source='template'`): pod CrashLoop (`restarts > 3`, 10 min
window), node NotReady (`not_ready > 0`, 5 min), host CPU > 0.9 (avg, 10 min), host disk > 0.9, workload `unavailable > 0`
(10 min), function error rate > 0.05 (5 min, minRequests 20), throttles > 0 (5 min), p95 > timeout × 0.8 (`relativeToTimeout`).

**Infrastructure root cause candidates (ADR-019):** for every incident with a service (monitor and KPI), in addition to call-tree RCA, the last 15 min
of the same `service`'s pods (`k8s_pods_1m`) and the hosts running the service (`containers_1m` hosts + pod nodes,
`hosts_1m`) are scanned: `OOMKilled` (0.8) / `CrashLoopBackOff` (0.75) / restart increase only (0.6), +0.05 if restarts ≥ 3, +0.05 if more
than one pod is affected; host average CPU > 0.9 → "Host <h> CPU saturated" (0.6 + (cpu − 0.9) × 2.5). Confidence is clamped to the
0.6–0.85 range, at most 5 candidates. If there is no call tree, it ends in the service's own code, explains nothing, or its
confidence is lower than the best candidate, the summary/confidence become the best candidate's ("Pod api-1 restarted (OOMKilled).", the Slack "Likely cause" line
shows this); the tree's answer stays in `treeSummary`/`treeConfidence`. Candidates appear in `root_cause.infra[]` and in the root node's
`evidence` (`kind: resource`).

KPI rule state is kept in `kpi_alert_rules.state/state_since/last_value/last_eval_at` and loaded at startup; KPI incidents carry `kpi_rule_id`. Before the firing notification is enqueued, in the same transaction
`incidents.root_cause` (RCA result, including `confidence`) is written; the payload carries `rootCause {summary, confidence}` and the service's
deploy in the last 1 hour (`deploy`). Query JSON details: `services/alerting/README.md`.

## Code-aware automatic alerts

Flow (details: [code-intelligence.md](code-intelligence.md)):

1. `codeintel` extracts an inventory from the repo: endpoints, consumers, crons, external dependencies, critical business flows (payment, auth, order).
2. A **criticality score** is computed for each inventory item:
   - Static signals: `payment|checkout|auth|login|order` etc. in the path/name, does it do DB writes, does it call an external payment provider, does it use transactions, does it have auth middleware.
   - LLM signal: given the handler code + its surroundings, a "business impact" classification (`critical | important | low`) and rationale.
   - Observational signals (if available): real traffic volume, current error rate, latency profile.
3. **Template selection** by criticality and type:
   - HTTP endpoint (critical): error rate + p95 latency + throughput drop
   - HTTP endpoint (important): error rate
   - Consumer: lag + failure rate + DLQ > 0
   - Cron: job_missed
   - External dependency: client span error rate
   - Log: meaningful messages from `logger.error(...)` calls in code → `log_match` suggestion
4. **Thresholds** are derived from observational data: if a 14-day baseline exists, `p95 * 1.5`; for error rate `max(baseline_p99, 1%)`. Without data, the template default + a "calibration" label; re-suggested automatically after 7 days.
5. Suggestions are written to the `alert_suggestions` table as `proposed`. The user bulk-accepts in the UI, or if **auto-accept** is enabled in project settings they become `active` monitors directly.
6. Optional: a `.trooper/monitors/` PR is opened in the repo for accepted monitors.
7. Diff analysis on new commits: new endpoint → new suggestion; deleted endpoint → suggestion to archive the monitor.

**Coverage metric:** what percentage of critical inventory items have an active monitor → shown on the Code insights screen.

## KPIs (SLO)

A KPI is a target for an outcome the user sees: *"the homepage loads fast"*, *"checkout succeeds"*,
*"POST /v1/orders does not return 5xx"*. Technically it is an SLO:

| Field | Example |
|---|---|
| Target | `page` (app + path), `endpoint` (service + route), `journey` (ordered steps), `app_start` (mobile cold start) |
| SLI type | `latency` (under threshold + error-free), `availability` (page rendered / 2xx), `success_rate` (not 5xx, journey completed) |
| Good event | `LCP ≤ 2.5 s and no JS error`, `duration ≤ 300 ms and status < 500` |
| Objective + window | `95%`, `28d` (or `7d`) |
| Error budget | Allowance of `1 − objective` bad events; the remaining ratio is shown |

**Alerts are defined on the KPI** and look at the error budget burn rate (short fluctuations do not page):

- *Fast burn*: burn rate > N× (e.g. 6×) over 1 hour **and** 5 min windows → page (PagerDuty).
- *Slow burn*: burn rate > M× (e.g. 3×) over 6 hours **and** 30 min windows → ticket / Slack.
- *SLI below*: simple threshold (e.g. "below 97% for 15 min") — optional.

Burn rate = (1 − good_ratio) / (1 − objective). 6× = the 28-day budget runs out in ~4.7 days.
Every KPI alert (`kpi_burn` monitor) adds the **root cause sentence** to the notification.

Suggestion from code: when code intelligence finds critical pages like landing / checkout and critical endpoints in the router, it suggests
KPIs (e.g. `/` → "Homepage loads fast", checkout flow → "Checkout succeeds").

## Root cause analysis

Goal: instead of saying "error rate increased", say **"this problem isn't yours; payments-api → iyzico calls are timing out"** or
**"the homepage is slow because recommendation-api's reco-db queries got 63× slower"**. Both KPIs and service /
endpoint pages use the same engine (the `rca` package inside `alert-engine`; the UI side uses the same algorithm in `apps/web/src/lib/rca.ts`).

**Input — call tree.** A per-minute edge rollup from spans (parent/child + `peer.service` / `db.system`):
`edges_1m(caller_service, caller_endpoint, callee_kind, callee_service, callee_endpoint|dependency, calls, errors, duration_sum)`.
From this a tree is built for a node: page → API endpoint → downstream service endpoint → DB / cache / queue / external API.
For each node the metric is computed over two windows: **now** (last 10–15 min) and **baseline** (1 hour before the change started /
the same hour last week). Metric: latency → average ms per request, errors → failure rate.

**Algorithm (delta attribution):**
1. The node's change to explain: `current − baseline`.
2. Each child's contribution: `calls/request × (child.current − child.baseline)`. For parallel calls (concurrent HTTP in the same request)
   only the slowest branch is on the critical path.
3. The remainder not explained by children → the node's **own code**.
4. Positive contributions are normalised (share 0..1); the candidate with the largest share is followed, others ≥ 15% are shown as "also".
5. Stop: if own code wins, a leaf node (DB, cache, external API) is reached, or there is no change.
6. Confidence = product of the shares across steps.

**Output:** path (before → now at each step, explained share), root node, one-sentence summary and **evidence**: deploys / migrations
overlapping in time with the root node (deploy marker + repo diff), the most-changed query fingerprint
(normalised `db.statement`), resource metrics (DB CPU, connection pool), related log patterns, external provider errors.

**Label on the service page:** if the root cause is the viewed service's own code, **"Your code"**; if it is a dependency the service
calls directly (its own DB, external API), **"Your dependency"**; if it is under another service,
**"Not your code · downstream"**. The KPIs affected by the root cause ("users feel it in") are also listed.

The card is shown only when the metric changed noticeably: relative change ≥ 30% and absolute change ≥ 0.1 points (errors) / 20 ms (latency).

## Notifications

Channels: Slack, PagerDuty, Opsgenie, email, webhook, Microsoft Teams (Adaptive Card; Incoming Webhook and Workflows URLs, `teams:`/`msteams:` prefix).
Notification content: chart snapshot, related deploy (last 1 hour), sample trace/log link, code reference (file:line, from the inventory).

**Slack App (ADR-017, contracts §17):** target `slack:<channel_id>#<name>` → `chat.postMessage` with the org's bot token; the first message's
`ts` is written to `notification_messages`, `resolved` / `reminder` / `acknowledged` go as replies in the same message's thread and the first
message is updated with `chat.update` (status color, buttons). Message: header (status emoji + title), fields (service, value vs threshold,
duration, severity), **"Likely cause: … (NN% confidence)"** (`< 30%` → "low confidence"; no line if there is no RCA), deploy / code context,
buttons *Open incident* (`TT_PUBLIC_APP_URL`), *Ack* (`tt_ack:<incidentId>`), *Mute 1h* (`tt_mute1h:<monitorId>`) — api
`POST /slack/interactions` handles them. Errors: `not_in_channel` → `conversations.join` + retry (private channel → permanent error, "invite the app
to the channel"), `channel_not_found` / `invalid_auth` → permanent, 429 → `Retry-After`. `slack:https://hooks.slack.com/…` webhook
targets work as before.

**AI quota notification (ADR-018, contracts §19):** every `TT_AI_QUOTA_CHECK_INTERVAL` (5 min) alert-engine computes this month's (UTC)
`ai_usage` total per org; if the effective limit (`orgs.ai_monthly_credits` ?? plan, `-1` unlimited) reached 80% / 100%
and the org has an `ai_notify` target, it writes a row of kind `ai_quota` to the `notifications` queue (no incident; payload `{orgId, projectId,
period, threshold, used, limit, resetsAt, link}`). Dedupe via `ai_quota_alerts (org_id, period, threshold)` — a single
message per month+threshold; an org that jumps straight to 100% gets only 100% (80% is marked too). Thanks to the row PK any replica can run
the check (no leader needed). `projectId` = the org's smallest project id (for resolving the channel name / Slack installation).
Notifier text is a single line on all channels: "AI usage at 80% of this month's credits (4,012 / 5,000). Resets Nov 1." (at 100%
"AI enrichment falls back to static rules until then." is appended). Slack App target → plain `chat.postMessage` (no thread / button /
`notification_messages`); Slack webhook → `{text}`; email → plain text (subject "AI usage at 80% of this month's credits");
Teams → Adaptive Card with a single TextBlock; webhook → `{event:"ai_quota", …payload, text}` (`X-Trooper-Event: ai_quota`, same signature);
PagerDuty/Opsgenie → warning/P4, dedup `tt-ai-quota-<org>-<period>-<threshold>`. `email:<address>[,<address>]` targets
resolve even without a `notification_channels` row.
