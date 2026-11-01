# Load testing

## Goal

Define and run load tests from inside the platform, and see the result **on the same screen** together with the target service's real telemetry (CPU, p95, error rate, DB spans).

## Components (ADR-015, contracts §14 — `services/loadtest`)

- **`loadtest-ctl`** (:8097, `/healthz`, `/metrics`): picks up runs left `queued` by the api with `FOR UPDATE SKIP LOCKED`,
  runs admission checks (verified domain, plan limits, monthly VU-minutes), starts one runner per region, merges the runners'
  shard rows and writes them to `loadtest_runs.live` / `summary`, handles abort and orphan runs, and performs domain verification.
  Scenario CRUD and run enqueueing live in the api.
- **`loadtest-runner`**: our own Go load engine (k6 is **not** embedded, ADR-015). Runs one region shard of a run,
  updates the `loadtest_run_shards` row on every tick (5 s) and sends OTLP metrics. For script scenarios it calls `k6` from PATH.
- **Executor**: `TT_LOADTEST_EXECUTOR=local` → child process; `k8s` → one `batch/v1` Job per region (no client-go, plain REST with the
  in-cluster SA token; nodeSelector via `TT_LOADTEST_K8S_REGION_LABEL`, secrets via `envFrom`).
- **Correlation**: the runner adds a new `traceparent`, `baggage: tt.svc=loadtest:<run_id>`, `x-trooper-loadtest-run: <run_id>`,
  `user-agent: TaskTrooper-LoadTest/1` to every request → SDKs in the target service record this as `trooper.caller`; the UI separates test traffic.

## Flow

```
api  POST /loadtests/scenarios/{id}/runs → loadtest_runs(state=queued)
ctl  claim (SKIP LOCKED) → admission → state=running, started_at=now(), shards (pending), VU-minutes reserved
     └─ executor: one runner per region (share = 1/number of regions; TT_LOADTEST_MAX_VUS is split across regions)
run  shard running → every tick: shard.live + cumulative stats (mergeable log-bucket histogram) + OTLP
ctl  merge shards every 2 s → loadtest_runs.live + summary while running
     abort_requested_at (or the api writing state=aborted directly) → stop runners (the runner also reads the row every 1 s)
     gate abort in one region → other regions are stopped too; runner exiting without a result / deadline → shard error
     all shards done → final summary, thresholds_passed, state passed|failed|aborted|error, usage corrected to the real value
```

Restart: a ctl started with the same `TT_LOADTEST_INSTANCE` marks its own `running` runs as `error` ("loadtest-ctl restarted…")
and sets `abort_requested_at` (runners stop on their own). Runs of other instances not updated for 3 min also become `error`.

## Schema (`deploy/postgres/006_loadtest.sql`)

- `loadtest_runs` + `region` (comma-separated region list), `live`, `abort_requested_at`, `error`, `created_at`, `requested_by`,
  `claimed_by`, `vu_minutes` (reserved at claim, real value when done).
- `loadtest_domains (id, project_id, host, token, verified_at, method dns|http, created_at, verify_requested_at, last_checked_at, last_error)`.
- `loadtest_usage (project_id, month, vu_minutes)`.
- `loadtest_run_shards (run_id, region, share, state pending|running|done|aborted|error, live, stats, summary, error, abort_reason, …)`.

## Scenario format

The runner runs the spec saved by the UI `ScenarioEditor` (`apps/web/src/lib/loadtest.ts` `saveScenario`) **as is**;
the alternative shapes in contracts §14 are also accepted:

| Field | Meaning |
|---|---|
| `target`, `regions` | Base URL; region list (if missing `TT_LOADTEST_REGIONS`, if that is missing too a single `local` shard) |
| `mode` | `vus` = closed model (VUs follow stage targets), `rps` = open model (arrival rate, pre-allocated VU pool; `dropped` if no free VU) |
| `levels [{name, ramp, hold, target}]` | Load profile; ramp 0 = jump. Otherwise `levels [{durationS, target}]` or `stages [{duration, target}]`. Linear between targets, starts from 0 |
| `steps ["METHOD /route"]`, `weights` | Weights are ratios or percentages (normalized). `flow: "sequential"` → all steps in order on every iteration (default if no weights); otherwise one weighted step per iteration |
| `requests[key]` | `params` (`name: value` lines → `:name` path segment or query), `headers` (lines or object), `body` (JSON template text) or `json`, `checks` (`"status == 201"`, `"latency < 300ms"`; default status < 400), `pools [{name, values}]` |
| `thinkTime` | Seconds; after each request in the closed model |
| `pools {name: [values]}`, `session {name: template}` | Template data; a pool row can be an object (`{{pool.users.email}}`) |
| `thresholds` | `["http_req_failed rate < 1%", "http_req_duration p(95) < 500 ms"]` (also p(99), avg, max, med) or `{p95Ms, p99Ms, errorRate}` |
| `gate` | `abortErrorRate`, `abortP95Ms`, `minRequests` (default 20) → automatic abort if exceeded in a 5 s window |
| `maxVus`, `timeoutS`, `script` | rps pool; request timeout (30 s); raw k6 JS |

Templates: `{{int a-b}}`, `{{uuid}}`, `{{pool.<name>[.<field>]}}` (same row within the same request; `random.<name>` is a synonym),
`{{weighted a:70 b:30}}`, `{{prev "KEY" .field.0.sub}}` (the last JSON response of step KEY in the same VU; reset at the start of each iteration
in the sequential flow), `{{session.<name>}}`, `{{optional 12% <expr>}}` (if not taken, the JSON key is dropped), `{{faker.words N|sentence|name|email|number}}`.
A JSON string value consisting of exactly one template keeps its type (a number stays a number). Unknown template / empty pool → placeholder value +
`summary.warnings`.

Script mode: if `spec.script` exists, the runner runs `k6 run --summary-export` and converts the JSON summary into `summary` (exit 99 = threshold violation);
if `k6` is missing the run gets `error: "k6 not installed"`. In script scenarios `target` is also required and must be verified (k6 cannot
technically be prevented from going to another host — known gap).

## Output

- `live` = `{ts, vus, rps, p95, errorRate, progress}` (progress 0..1; with multiple regions vus/rps are summed, p95 is the worst region).
- Final `summary`: `progress, rps, p95, errorRate, vus` (peak), `thresholds [{name, passed, value}]`, `p50, p99, avgMs, maxMs,
  requests, errors, durationS, vuMinutes, dropped`, `steps [{key, count, errors, errorRate, rps, p50, p95, p99, avgMs}]`,
  `statusCodes {"200": n, "error": n}` (`error` = transport error), `checks`, `abortReason`, `warnings`, `regions` (with multiple regions).
  While running `summary` = `progress, rps, p95, errorRate, vus, thresholds, requests, errors` (the api's run model reads these).
- Error rate = transport error **or** failed check (status ≥ 400 if there are no checks).
- OTLP (delta, every tick): `trooper.loadtest.requests`, `.errors` (sum), `.duration_ms` (histogram, bounds 5…10000 ms),
  `.vus` (gauge); resource `service.name=loadtest`, point attrs `trooper.loadtest.run`, `.step`, `.region`.
  Identity: `TT_INTERNAL_TOKEN` → `x-trooper-internal` + `x-trooper-project`; otherwise the key in `TT_DSN`.

## Scenario generation

- **From inventory:** endpoints extracted by codeintel + `loadtest_hint` (payload schema) → a draft for the selected endpoints.
- **From real traffic:** route distribution of the last 7 days and RUM flows (page transition probabilities) → a realistic traffic mix. Targets like "3× production traffic".
- A generated scenario is always `draft`; it does not run without user approval.
- **Agent (MCP, ADR-032):** `tt_create_loadtest_scenario` writes a `source=agent`, `state=ready` scenario with user approval (if there is no `gate`,
  `abortErrorRate 0.5`, `abortP95Ms 10000` are added); `tt_run_loadtest` runs it with a separate approval, `tt_stop_loadtest` stops it without
  approval. Typical use: a short baseline load for endpoints without enough traffic to pick an alert threshold (`skills/trooper-alerts`).

## Security

- Target domain verification is mandatory: DNS TXT `trooper-verify=<token>` (on the host) or `https://<host>/.well-known/trooper-verify`
  body = token (redirects to another host are not followed). api `POST /loadtests/domains/{id}/verify` → `verify_requested_at`; the ctl worker
  (10 s) checks it and writes `verified_at` + `method` or `last_error`; for DNS propagation it retries every minute for 15 min after the request.
- Match: exact host, or a subdomain of a domain verified **via DNS**. With `TT_LOADTEST_ALLOW_LOCAL=true`, `localhost`,
  `*.localhost`, loopback IPs are exempt (dev).
- Limits: `TT_LOADTEST_MAX_VUS` (in rps mode the UI estimate is `ceil(rate × (0.2 + think))`; hard VU cap in the runner),
  `TT_LOADTEST_MAX_DURATION`, `TT_LOADTEST_MONTHLY_VU_MINUTES` (project + UTC month). Rejection → `state=error`, `error` is a user-facing message.
- Emergency stop: `abort_requested_at`; automatic abort: `gate.abortErrorRate` / `gate.abortP95Ms`.

## Run UI

Live: VU count, RPS, p95, error rate (runner side) + the target service's RED and resource metrics (SDK side), on the same time axis but in **separate charts** (dual-axis is forbidden). When done: threshold results, slowest endpoints, comparison with the previous run.

## Open items

- UI `progression: manual|ai` and `gate.latencyX/cpu` (AI gate that looks at target service telemetry) are not implemented in the runner; the run advances by timer (`auto`).
- UI pools only carry `{name, detail}`; real values must be given via `spec.pools` / `requests[k].pools[].values`, otherwise synthetic values.
- No login flow for `{{session.*}}`; `spec.session` templates are generated once per VU.
- UI run states do not know about `error` (api/UI side).
- No region weights (`eu-central: 70`); load is split equally across regions. In the local executor, regions are separate processes on the same machine.
