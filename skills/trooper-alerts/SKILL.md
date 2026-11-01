---
name: trooper-alerts
description: Design and set up alerting for a project with the TaskTrooper MCP tools — monitors (alert rules), KPIs / SLOs with burn-rate alerts, notification routing, and a baseline load test when there is too little traffic to pick thresholds. Use when the user asks to "set up alerts", "define monitors / KPIs / SLOs for this project", "what should we alert on", "add an alert for X", "send alerts to #channel", or "run a load test so we can set thresholds". Writes need TT_MCP_ALLOW_WRITE=true and the user's approval for every change. Not for investigating a live problem (use trooper-diagnose).
---

# trooper-alerts

Goal: a small set of alerts that fire on real user impact, are quiet otherwise, and reach the right channel. Every
threshold comes from the project's own data, and nothing is written without the user's approval.

Background on the read tools: the `trooper-query` skill. Full contract: `.ai/mcp.md` (ADR-032).

## Ground rules

- **Approval.** Every write tool (`tt_create_*`, `tt_update_*`, `tt_delete_*`, `tt_accept_suggestion`,
  `tt_send_slack_test`, `tt_run_loadtest`) first runs without `confirm` and returns a dry run. Show the user what
  will change, wait for a clear yes, then call again with `confirm: true`. A yes covers the items listed in that
  message, nothing more. `tt_stop_loadtest` needs no approval; use it at once if a run hurts the target.
- **No write tools?** Without `TT_MCP_ALLOW_WRITE=true` only the read tools exist. Still do steps 1–4, then hand the
  user the list (kind, target, condition, channel) to create in the dashboard, and say how to enable writes.
- **Data, not guesses.** A threshold is derived from a number you read (cite it: "p95 over 7d = 240 ms"). If the
  data is missing or thin, say so and go to step 3b; do not invent a "typical" value.
- **No duplicates.** Before proposing, check `tt_list_monitors`, `tt_list_kpis` and `tt_list_alert_suggestions`.
  Prefer accepting a matching suggestion (`tt_accept_suggestion`) over writing the same monitor by hand.
- **Few, loud alerts.** Page (critical) only on user impact: availability, error rate, latency on key routes, SLO
  burn, jobs not running, consumer lag. Everything else is warning or info. Aim for 3–8 monitors per service.
- Ids are never guessed: service names from `tt_list_services`; `target.endpoint` is `"METHOD /route"` from
  `tt_list_endpoints` (never the slug); channel strings from `tt_list_notify_channels`.

## Workflow

### 1. Inventory

1. `tt_list_services { range: "7d" }`: services, type (`http`, `worker`, `web`, `mobile`) and traffic.
2. Per service in scope: `tt_list_endpoints { service, range: "7d" }` (requests, error rate, p95 per route).
3. What exists: `tt_list_monitors { service }`, `tt_list_kpis { service }`, `tt_list_alert_suggestions`.
4. Extra signals when they apply: `tt_list_inventory { kind: "cron" }` (crons, for `job_missed`),
   `tt_list_consumer_groups` (queues), `tt_list_hosts` / `tt_list_databases` (infra),
   `tt_get_app_summary { app }` (web / mobile), `tt_get_ai_overview` (LLM calls), `tt_list_errors` (noisy errors).
5. Where alerts go: `tt_list_notify_channels {}` (`refresh: true` for a channel created minutes ago).

Ask the user only what the data cannot tell you: which flows are business-critical (checkout, login, payments),
who owns which service, and which channel is for paging vs. for FYI. Keep it to one short question.

### 2. Pick what to watch

| Service type | Usually worth it |
|---|---|
| `http` | Error rate on the service; p95 on the 1–3 most important routes; availability KPI on the key route; throughput drop if traffic is steady |
| `worker` / consumer | `consumer_lag` (lag or time_behind) per group; `dlq` failures; `job_missed` per cron |
| `web` | `rum_vital` LCP p75 on key pages; JS error rate via `error_new`; page KPI |
| `mobile` | `crash_rate` per session; app start via `rum_vital`; `error_new` |
| Infra / serverless / LLM | Copy from `tt_list_monitor_templates` (CrashLoop, NotReady, disk, function errors, LLM cost/hour) |
| Any | `error_new` (new error fingerprints) as `info` or `warning`, never critical |

### 3. Derive thresholds from data

Every measured alert is phrased as **"over the last N minutes, X is above Y"** and sets `condition.window`
explicitly: "error rate over the last 5 minutes > 2 %", "p95 over the last 5 minutes > 800 ms", "requests over the
last 10 minutes dropped > 50 % vs. last week". The engine re-checks every minute. Pick the window by traffic:
`5m` for busy routes (≥ ~50 requests per 5 min), `15m`–`30m` for quieter ones (so `minRequests` can be met), `1m`
only for hard-down checks on very busy services. `forMin` is separate: how long the windowed value must stay bad
before firing (0–10 min); a 5m window with `forMin: 0` is usually enough.

Read the 7-day numbers (`tt_list_endpoints`, `tt_get_service_kpis`, `range: "7d"`, and `"24h"` for the recent shape):

| Kind | Threshold | Condition |
|---|---|---|
| `latency` (p95) | max(1.5 × 7d p95, 7d p95 + 100 ms), rounded to a clean number | `window: "5m"`, `forMin: 0–5`, `minRequests` ≈ 10 % of the requests in one window (≥ 20) |
| `error_rate` | max(2 × 7d error rate, 0.01) for critical routes, 0.05 for the rest; a fraction (2 % = 0.02) | `window: "5m"`, `forMin: 0–5`, `minRequests` as above |
| `throughput` drop | `direction: "drop"`, threshold 0.5 (−50 % vs. the same window last week) | only for steady traffic (no zero hours) |
| `anomaly` | z = 3 (`threshold: 3`), `direction: "up"` for latency / errors | when the level varies by hour; needs ≥ 2 weeks of data |
| `job_missed` | `intervalMin` = the cron period, `graceMin` ≈ 10–20 % | `threshold: 0` (derived) |
| `consumer_lag` | `time_behind` > 2–3 × the normal processing time, or lag above the 7d max | `forMin: 10` |

If the current value already breaches the threshold, say so; the user may want to fix the issue first, or accept a
higher starting threshold.

#### 3b. Not enough data → offer a load test

A threshold needs volume. Treat a route as **thin** when, over 7 days, it has fewer than ~500 requests, fewer than
~20 requests in its busiest hour, or no data at all (a new service or a staging project). For thin routes:

1. Say it plainly: "`POST /v1/orders` had 37 requests in 7 days, too few for a meaningful p95."
2. Offer a short baseline load test and wait for a yes. Mention the target URL, peak load, duration, that it sends
   real traffic (writes on `POST` routes create real data), and that it aborts itself on high errors.
3. Check `tt_list_loadtest_scenarios`: reuse an existing scenario for the same routes if there is one. Its
   `domains` list shows which hosts may be load tested; a target must be a verified domain. If none is verified,
   stop and tell the user to add and verify one under Load tests → Domains.
4. Draft the scenario: the thin routes as `steps`, a gentle profile (warm 30 s ramp to 5 VUs for 60 s, then
   30 s ramp to 20 VUs held 3–5 min), `thinkTime: 1`, `thresholds` from the user's expectations, and checks
   per route. Prefer `GET` routes; include writes only if the user agrees and the target is not production.
   `tt_create_loadtest_scenario { name, spec }` → dry run → approval → `confirm: true`.
5. `tt_run_loadtest { scenarioId }` → dry run (shows target, peak VUs, duration) → approval → `confirm: true`.
6. Poll `tt_get_loadtest_run { id }` every 15–30 s; report progress briefly. On `failed` / `error`, show the
   reason. If the target degrades (error rate climbing), `tt_stop_loadtest { id }` immediately.
7. After the run, re-read `tt_list_endpoints { service, range: "1h" }`. Load-test p95 is a capacity reference,
   not normal behaviour: set latency thresholds from it with more headroom (≥ 2× the p95 at moderate load), and
   tell the user to revisit them after a week of real traffic.

### 4. Preview every monitor

`tt_preview_monitor { monitor, range: "7d" }` (or `"24h"` after a load test). Read `episodes` and `firingNow`:

- More than ~1 episode a day → too noisy: raise the threshold, raise `forMin` or add `minRequests`, preview again.
- 0 episodes is fine for a guard rail, but check the series actually has values (all zeros = wrong target or query).
- `throughput` previews show plain RPM, not the drop vs. last week. Kinds without a preview (`anomaly`, `job_missed`,
  `composite`, `error_new`, `apdex`, `infra`, `faas`) return an
  error: say they are checked only after saving.

### 5. Propose, then write

Present one numbered table per service and ask for approval once:

```
#  Kind        Target                     Condition                          Sev       Channel         Preview (7d)
1  latency     checkout POST /v1/orders   p95 over last 5m > 800 ms          critical  #alerts-oncall  0 episodes (p95 now 310 ms)
2  error_rate  checkout (service)         error rate over last 5m > 2 %,     critical  #alerts-oncall  1 episode (Oct 28 deploy)
                                          ≥ 50 req in the window
3  job_missed  billing nightly-invoice    every 1440m +120m      warning   #team-billing   n/a
KPI  Checkout succeeds: availability of POST /v1/orders ≥ 99.5 % / 28d, burn alerts fast+slow
```

After the yes: for each item, call the tool with `confirm: true` (the dry run was the table itself; skip a second
dry run unless the user changed something). Report what was created with ids, and anything that failed with the
API's message. Do not retry a rejected body unchanged; fix it or ask.

## Shapes

`tt_create_monitor { monitor, confirm }`. `channels` is required (`[]` = incident only, no message). `condition.window`
defaults to `5m` (`15m` for anomaly) when omitted; always state it to the user.

```json
{ "name": "checkout POST /v1/orders p95", "kind": "latency",
  "target": { "service": "checkout", "endpoint": "POST /v1/orders" },
  "query": { "percentile": "p95" },
  "condition": { "op": ">", "threshold": 800, "window": "5m", "forMin": 0, "minRequests": 50 },
  "severity": "critical", "channels": ["slack:C07ABCDEF#alerts-oncall"] }
```

| Kind | `query` | `condition` notes |
|---|---|---|
| `latency` | `{percentile: "p95"}` | threshold ms |
| `error_rate` | `{}` or `{statusMin: 500}` (5xx only) | threshold fraction |
| `throughput` | `{direction: "drop"}` | op `>`, threshold 0.5 = traffic fell more than 50 % vs. the same window last week |
| `apdex` | `{tMs: 500}` | op `<`, threshold 0..1 |
| `anomaly` | `{metric: "p95", direction: "up"}` | threshold z (3), `window: "15m"` |
| `error_new` | `{}` | op `>`, threshold 0 |
| `log_count` | `{minSeverity: "error", contains?: "timeout"}` | count per `window` |
| `job_missed` | `{intervalMin: 60, graceMin: 10}`; target `{service, process}` | threshold 0 (derived) |
| `consumer_lag` | — ; top-level `queue: {clusterId, group, metric: "time_behind", threshold, forMin}` | from `tt_list_consumer_groups` |
| `dlq` | `{metric: "dlq", process?}` | count per `window` |
| `rum_vital` | `{vital: "LCP", percentile: "p75", view?}`; target.service = app | threshold ms (CLS unitless) |
| `crash_rate` | `{per: "session"}`; target.service = app | threshold fraction |
| `composite` | `{op: "and", monitorIds: ["12", "15"]}` | fires when all (and) / any (or) children fire; pass `{op: ">", threshold: 0}` |
| `infra` (host / Kubernetes) | `{entity: "host" \| "node" \| "cluster", metric: "cpu" \| "memory" \| "disk", agg: "max", filter: {cluster?, host?}}`; pods/containers `{entity: "pod", metric: "memory_ratio" \| "restarts" \| "not_ready"}`; `{entity: "workload", metric: "unavailable"}` | ratios are 0..1 (0.9 = 90 %); `cluster` = mean over the cluster's nodes, `node` = worst node |
| `infra` (database) | `{entity: "database", metric: "memory" \| "disk" \| "cpu" \| "connections_ratio" \| "p95_ms" \| "repl_lag_ms" \| "hit_ratio", agg: "max", filter: {database?: <id from tt_list_databases>}}` | ratios 0..1; `hit_ratio` with op `<` |
| `faas`, `llm` | copy a `tt_list_monitor_templates` item | `window` from the template |

`tt_create_kpi { kpi, confirm }`:

```json
{ "name": "Checkout succeeds", "description": "POST /v1/orders answers without a 5xx",
  "owner": "payments team", "sli": "availability", "objective": 0.995, "window": "28d",
  "target": { "kind": "endpoint", "serviceId": "checkout", "ref": "POST /v1/orders" },
  "good": { "statusBelow": 500 },
  "alerts": [
    { "kind": "burn_rate", "longWindow": "1h", "shortWindow": "5m", "threshold": 14.4, "severity": "critical", "channels": ["slack:C07ABCDEF#alerts-oncall"] },
    { "kind": "burn_rate", "longWindow": "6h", "shortWindow": "30m", "threshold": 6, "severity": "warning", "channels": ["slack:C07XYZ#team-payments"] } ] }
```

- `sli: "latency"` needs `good.thresholdMs` (good = at or under it and not 5xx). Pick it from data like a latency
  monitor, so the objective is reachable today (current SLI ≥ objective).
- Objective: 0.99–0.995 for most APIs; 0.999 only with lots of traffic (a 99.9 % / 28d budget is 40 minutes).
- A KPI's burn alerts replace a threshold monitor on the same route and signal; do not create both.
- `target.kind: "page"` → `serviceId` = web app, `ref` = path (`/checkout`); `app_start` for mobile.

Routing: critical → the on-call channel (or PagerDuty / Opsgenie configured channel); warning → the team channel;
info → no channel (`[]`) or a low-noise channel. Before routing to a Slack channel for the first time, offer
`tt_send_slack_test { channel }`. A private channel needs `/invite @TaskTrooper` first.

## Changing and cleaning up

- Mute instead of delete when a monitor is noisy during an incident: `tt_update_monitor { id, patch: { mutedUntil } }`.
- Tune: `tt_update_monitor { id, patch: { condition } }`, e.g. widen a noisy one from `window: "5m"` to `"15m"`. The
  whole condition is replaced: send every field (op, threshold, window, forMin, minRequests).
- Delete only what an agent created (`source: agent`); others are the user's, ask them to remove it in the dashboard.
- Monitors with `source: code` are managed in the repository (`.trooper/monitors/*.yaml`); change the YAML instead.
