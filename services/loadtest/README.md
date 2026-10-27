# services/loadtest

Load test controller and runner (ADR-015, contracts §14). Module `github.com/tasktrooper/telemetry/services/loadtest`.

| Binary | What it does |
|---|---|
| `loadtest-ctl` (:8097, `/healthz`, `/metrics`) | Claims `queued` runs (`FOR UPDATE SKIP LOCKED`), admits them (verified domain, VU / duration / monthly VU-minute limits), starts one runner per region, merges the runners' shard rows into `loadtest_runs.live` / `summary`, handles aborts, recovers orphans, verifies target domains. |
| `loadtest-runner` | Runs one region shard of a run with the built-in Go engine (or `k6 run` for script scenarios), writes `loadtest_run_shards` every tick, exports OTLP metrics to ingest-gw. |

Schema: `deploy/postgres/006_loadtest.sql` (`loadtest_runs` columns, `loadtest_domains`, `loadtest_usage`, `loadtest_run_shards`).

## Flow

```
api: POST /loadtests/scenarios/{id}/runs → loadtest_runs(state=queued)
ctl: claim (SKIP LOCKED) → admit → state=running, shards(pending), usage reserved
     └─ executor: local subprocess | k8s Job   (one per region, share = 1/regions)
runner: shard running → every tick (5 s) shard.live + cumulative stats (mergeable histograms) + OTLP
ctl: every 2 s merge shards → loadtest_runs.live + running summary
     abort_requested_at / state=aborted → stop runners (they also poll the row every second)
     a gate abort in one region stops the others; runner exit without result / deadline → shard error
     all shards terminal → final summary, thresholds_passed, state passed|failed|aborted|error, usage settled
```

## Scenario spec

The runner executes what the UI `ScenarioEditor` saves (`apps/web/src/lib/loadtest.ts` `saveScenario`), plus the
contracts §14 variants:

- `target` (base URL), `regions` (`["eu-central", …]`), `mode` `vus` (closed model) | `rps` (open model, arrival rate).
- Load shape: `levels [{name, ramp, hold, target}]` (ramp 0 = jump) → else `levels [{durationS, target}]` → else `stages [{duration, target}]`.
  Linear ramps between targets starting at 0.
- Steps: `steps ["METHOD /route"]`, `weights {key: fraction | percent}` (normalized), `flow: "sequential"` runs all steps in order
  per iteration (default when there are no weights), otherwise one weighted step per iteration. `thinkTime` seconds after each request
  (closed model; the open model paces by arrival rate).
- `requests[key]`: `params` (`name: value` lines → `:name` path segment or query param), `headers` (`Header: value` lines or object),
  `body` (JSON template text; non-JSON is sent as text) or `json` (object), `method`/`path` overrides, `checks`
  (`["status == 201", "latency < 300ms"]` or `[{status}, {maxMs}]`; default: status < 400), `pools[{name, values}]`.
- Data: `pools {name: [values]}` (values may be objects → `{{pool.users.email}}`), `session {name: template}` (per VU).
- `thresholds`: `["http_req_failed rate < 1%", "http_req_duration p(95) < 500 ms"]` (also `p(99)`, `avg`, `max`, `med`) or `{p95Ms, p99Ms, errorRate}`.
- `gate`: `abortErrorRate`, `abortP95Ms`, `minRequests` (default 20) — evaluated on each 5 s window → auto-abort. `latencyX`/`cpu`/`progression`
  are stored by the UI but not enforced (see gaps).
- `maxVus` (rps mode pre-allocated VUs; default 2 × UI estimate), `timeoutS` (request timeout, default 30), `script` (raw k6 JS).

Templates: `{{int a-b}}`, `{{uuid}}`, `{{pool.<name>[.<field>]}}` (same row within one request; `random.<name>` alias),
`{{weighted a:70 b:30}}`, `{{prev "KEY" .field.0.sub}}` (previous JSON response of step KEY in this VU — reset per iteration in
sequential flow), `{{session.<name>}}`, `{{optional 12% <expr>}}` (dropped JSON key / empty), `{{faker.words N|sentence|name|email|number}}`.
A JSON string that is exactly one template keeps its type (numbers stay numbers). Unknown templates / missing pools render a
placeholder and are listed in `summary.warnings`.

Every request carries `traceparent` (new per request), `baggage: tt.svc=loadtest:<run>`, `x-trooper-loadtest-run: <run>`,
`user-agent: TaskTrooper-LoadTest/1`.

## Output

- `loadtest_runs.live` = `{ts, vus, rps, p95, errorRate, progress}` (progress 0..1; multi-region: vus/rps summed, p95 worst region).
- `loadtest_runs.summary` (final): `progress, rps, p95, errorRate, vus` (peak), `thresholds [{name, passed, value}]`,
  `p50, p99, avgMs, maxMs, requests, errors, durationS, vuMinutes, dropped`, `steps [{key, count, errors, errorRate, rps, p50, p95, p99, avgMs}]`,
  `statusCodes {"200": n, "error": n(transport)}`, `checks {name: [passed, failed]}`, `abortReason`, `warnings`, `regions [...]` (multi-region).
  While running, `summary` holds `progress, rps, p95, errorRate, vus, thresholds, requests, errors` (what the api's run model reads).
- `thresholds_passed`, `state` passed | failed | aborted | error, `error`, `started_at` (claim time), `finished_at`, `vu_minutes`.
- OTLP (delta temporality, every tick): `trooper.loadtest.requests`, `.errors` (sum), `.duration_ms` (histogram, bounds 5…10000 ms),
  `.vus` (gauge); resource `service.name=loadtest`, points `trooper.loadtest.run`, `.step`, `.region`.

## Environment

| Variable | Default | |
|---|---|---|
| `TT_LOADTEST_ADDR` | `:8097` | ctl health/metrics |
| `TT_POSTGRES_URL`, `TT_LOG_LEVEL` | compose | both binaries |
| `TT_LOADTEST_EXECUTOR` | `local` | `local` (subprocess) or `k8s` (Job per region) |
| `TT_LOADTEST_RUNNER_BIN` | next to ctl, then PATH | local executor |
| `TT_LOADTEST_ALLOW_LOCAL` | `false` | exempt `localhost`, `*.localhost`, loopback IPs from domain verification (dev) |
| `TT_LOADTEST_MAX_VUS` | `500` | peak VUs per run (rps mode: UI estimate `ceil(rate × (0.2 + think))`); also the runners' hard VU cap |
| `TT_LOADTEST_MAX_DURATION` | `1h` | per run; also the k6 timeout |
| `TT_LOADTEST_MONTHLY_VU_MINUTES` | `0` (unlimited) | per project and UTC month (`loadtest_usage`; reserved at claim, settled to actual at the end) |
| `TT_LOADTEST_REGIONS` | — | default regions when a scenario has none (else one `local` shard) |
| `TT_LOADTEST_PROJECTS` | — (all) | restrict a ctl to project ids (tests, sharding) |
| `TT_LOADTEST_INSTANCE` | hostname | claim owner; a restart with the same id marks its running runs `error` |
| `TT_LOADTEST_POLL`, `TT_LOADTEST_VERIFY_INTERVAL`, `TT_LOADTEST_MAX_CONCURRENT` | `1s`, `10s`, `4` | |
| `TT_LOADTEST_INGEST_URL` | — | ingest-gw base URL for runner metrics (or `TT_DSN`) |
| `TT_INTERNAL_TOKEN` | — | internal ingest credential (`x-trooper-internal` + `x-trooper-project`, contracts §16); else the key in `TT_DSN` |
| `TT_LOADTEST_TICK` | `5s` | runner live/OTLP interval |
| `TT_LOADTEST_K6_BIN` | `k6` | script mode |
| `TT_LOADTEST_K8S_IMAGE` (required), `_NAMESPACE` (pod's), `_API` (`https://kubernetes.default.svc`), `_SERVICE_ACCOUNT`, `_SECRET` (envFrom: `TT_POSTGRES_URL`, `TT_INTERNAL_TOKEN`), `_REGION_LABEL` (nodeSelector key), `_CPU` (`500m`), `_MEMORY` (`512Mi`) | | k8s executor (in-cluster SA token; needs `create/get/delete` on `batch/jobs`) |

Runner-only (set by ctl): `TT_LOADTEST_RUN_ID`, `TT_LOADTEST_REGION`, `TT_LOADTEST_SHARE`, `TT_LOADTEST_VU_CAP`.

## Run / test

```bash
cd services/loadtest
GOWORK=off go build -o bin/ ./cmd/...
TT_LOADTEST_ALLOW_LOCAL=true TT_LOADTEST_INGEST_URL=http://localhost:4318 TT_INTERNAL_TOKEN=… ./bin/loadtest-ctl

GOWORK=off go test -race ./...                              # engine, spec, ctl (admission, merge, verifier, k8s), otlp, k6
GOWORK=off go test -tags integration -count=1 ./...         # + live Postgres (make infra && make migrate): claim/limits/verification + e2e
```

The e2e (`e2e/`) builds `loadtest-runner`, runs the controller with the local executor against an httptest target and a fake
ingest-gw (project 9501, cleaned up) and checks passed / user abort / gate abort / rejected target.

Docker: `docker build -f services/loadtest/Dockerfile -t tasktrooper/loadtest services/` (both binaries; no k6 in the image).
