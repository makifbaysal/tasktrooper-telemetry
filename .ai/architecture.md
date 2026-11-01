# Architecture

## Overview

```
 ┌──────────── Customer side ─────────────┐
 │ Backend SDK  Worker SDK  Browser SDK    │
 │ (node/go/py) (consumer)  Mobile SDK     │
 └──────┬──────────┬───────────┬───────────┘
        │ OTLP/HTTP (protobuf, gzip) + RUM JSON beacon
        ▼
 ┌──────────────┐   ┌───────────┐   ┌──────────────────┐
 │ ingest-gw    │──▶│ Redpanda  │──▶│ ingest-writer    │──▶ ClickHouse
 │ (auth, rate  │   │ topics:   │   │ (batch insert,   │     (metrics, spans,
 │ limit, valid)│   │ spans,    │   │  RED rollup,     │      logs, rum_events)
 └──────────────┘   │ logs,rum, │   │  route normalize)│
                    │ metrics   │   └──────────────────┘
                    └─────┬─────┘
                          │ (stream consumption)
                          ▼
                   ┌──────────────┐      ┌──────────────┐
                   │ alert-engine │─────▶│ notifier     │──▶ Slack, PagerDuty,
                   │ (eval loop + │      │ (dedupe,     │    email, webhook
                   │  stream rule)│      │  routing)    │
                   └──────┬───────┘      └──────────────┘
                          │
 ┌──────────┐   ┌─────────┴────┐   ┌──────────────┐   ┌──────────────────┐
 │ apps/web │──▶│ api (query + │──▶│ PostgreSQL   │   │ codeintel        │
 │ dashboard│   │ control)     │   │ (metadata)   │◀──│ (repo sync, AST, │
 └──────────┘   └──────┬───────┘   └──────────────┘   │  LLM, suggestion)│
                       │                              └──────────────────┘
                       ▼
                ┌──────────────┐    ┌─────────────────┐
                │ loadtest-ctl │───▶│ k6 runners       │──▶ customer target
                │ (orchestration│    │ (K8s Job, region)│
                └──────────────┘    └─────────────────┘
```

## Services

| Service | Language | Responsibility | Scale |
|---|---|---|---|
| `ingest-gw` | Go | OTLP/HTTP + gRPC, RUM beacon; API key validation, rate limit (Redis), schema validation, writing to Redpanda. Stateless. | Horizontal, highest traffic |
| `ingest-writer` | Go | Consumes Redpanda, batch inserts into ClickHouse (≥10k rows or 1 s). Route normalization, error fingerprint, geo-ip, user-agent parse. | As many as partitions |
| `api` | Go | The dashboard's only backend. Query (ClickHouse), control plane CRUD (Postgres), auth (OIDC + session), tenant isolation. | Horizontal |
| `alert-engine` | Go | Evaluates monitors periodically (ClickHouse query) + stream evaluation for low-latency rules. Incident state machine. KPI burn-rate evaluation and the `rca` package (root cause via delta attribution on the call tree; see alerting.md § Root cause analysis). | Leader election + shard (monitor id hash) |
| `notifier` | Go | Notification channels, dedupe, grouping, escalation. | Small |
| `codeintel` | Go + tree-sitter | Repo sync (GitHub/GitLab App), inventory extraction via AST, LLM enrichment, alert/scenario suggestions. | Job-based queue |
| `loadtest-ctl` | Go | Scenario storage, run orchestration, distributing work to runners, collecting results. | Small |
| `loadtest-runner` | Go (k6 embed) | Runs the scenario, sends metrics as OTLP to our own ingest. | K8s Job, multi-region |

All services are instrumented with our own SDK (dogfooding). Data goes to a separate `TaskTrooper` project; ingest does not produce spans for its own project's traffic (no loop). See ADR-027, contracts §27.

## Data flow decisions

- **Wire protocol is OTLP.** The SDKs are a thin layer on top of OpenTelemetry. Customers can also point their existing OTel collector at us. For RUM there is an additional lightweight JSON beacon (`navigator.sendBeacon`) — the OTel JS browser packages are heavy.
- **Redpanda as buffer.** If ClickHouse slows down, ingest does not drop; replay is possible. Retention per topic is 24 hours.
- **Pre-aggregation.** `ingest-writer` produces per-minute RED rollups from spans (`red_1m`), a ClickHouse materialized view rolls them down to hourly (`red_1h`). Dashboards read from rollups; raw spans are only for trace/drill-down.
- **Sampling.** Head sampling in the SDK (default 100%, configurable); errors and slow requests are always kept (tail sampling in ingest-writer). RED metrics are counted **before** sampling, so no accuracy is lost.

## Multi-tenancy & security

- `org_id`, `project_id` on every row. In ClickHouse they are not part of the partition key, but at the start of ORDER BY.
- API key types: `ingest` (write only, may be exposed in the browser), `api` (read/write, secret).
- The browser ingest key is protected by a domain allowlist.
- Repo access via GitHub App, with minimum permissions (contents:read, metadata:read, pull_requests:write — for alert-as-code PRs).
- Source code is not stored permanently: after analysis only the inventory + summaries are kept.

## Monorepo

Current layout and module ownership: [`contracts.md` § 1](contracts.md#1-repo-layout-and-ownership). Summary:

```
apps/web/                         React dashboard
services/                         Go 1.26, go.work
  pkg/  ingest/  api/  alerting/  collector/      (codeintel, loadtest later)
sdks/
  go/  jvm/  dotnet/  js/packages/  android/  ios/  flutter/
proto/rum/v1/                     RUM wire schema
deploy/
  docker-compose.yml  clickhouse/*.sql  postgres/*.sql
.ai/                              docs
```

Service ↔ binary: `ingest` → `ingest-gw`, `ingest-writer`; `alerting` → `alert-engine`, `notifier`; `collector` →
`trooper-agent` (DB/broker, host, container, process, GPU and Kubernetes metrics; sends them to ingest via OTLP).

### trooper-agent modes (ADR-019, contracts §20 + §22)

A single binary, three modes that can be combined with `TT_AGENT_MODES` (default `db` = old behavior). Each mode uses a
separate emitter with its own OTLP resource; if a source cannot be read the agent does not stop, it writes
`trooper.collector.up=0` (`kind` = `host|kubelet|containers|k8s`).

| Mode | Where | Source | Interval | Output |
|---|---|---|---|---|
| `db` | Cluster Deployment in Helm (`db.enabled`), VM / `docker run` | SQL, Kafka/RabbitMQ API; targets from static YAML (`db.targets` → ConfigMap) and/or api (`db.fromApi`) | config `interval` | §5 metrics |
| `host` | On K8s a Linux DaemonSet (hostPID, `/proc` `/sys` `/` read-only) + optional Windows DaemonSet (HostProcess), outside K8s `docker run` / systemd / Windows service | gopsutil; containers on K8s via kubelet `/stats/summary` + `/pods` (+ `/metrics/cadvisor` throttling), outside K8s in order Docker Engine API (Linux socket, Windows `//./pipe/docker_engine`) → Podman (Docker-compatible socket) → cgroup v2 tree (Linux; containerd/nerdctl/others); for the same container id the first source wins. GPU: if `nvidia-smi` exists | 15 s | `trooper.host.*`, `trooper.host.gpu.*`, `trooper.container.*`, `trooper.process.*` |
| `cluster` | Single-replica Deployment | K8s API: `list` + `watch` for each kind (resourceVersion, bookmark; 410 Gone → re-list; reconnect with backoff), in-memory state; no client-go | 30 s snapshot | `trooper.k8s.*` + Warning event (watch) → OTLP log (`service.name=k8s`) |

Cluster mode reads the watch stream with plain `net/http` + streaming JSON; each kind (nodes, pods, replicasets, deployments,
statefulsets, daemonsets, jobs, cronjobs, Warning events) is a `Reflector`. Until the initial list of nodes/pods finishes, scrape
returns `up=0`; other kinds that cannot be read (RBAC / API group missing) only drop their own metrics (retried every
5 min). GPU metrics go to the `gpus_1m` table (routed by the writer, `deploy/clickhouse/004_gpu.sql`); if `nvidia-smi`
is missing they are silently skipped. Windows build: `GOOS=windows` (no process `fds`, `os.type=windows`).

Deployment: a single chart `deploy/helm/trooper-agent` — Linux host DaemonSet (`kubernetes.io/os: linux`), cluster Deployment
(`cluster` and/or `db`), `windows.enabled` → HostProcess DaemonSet with `kubernetes.io/os: windows` nodeSelector
(`services/collector/Dockerfile.windows`), RBAC, Secret, optional OBI eBPF DaemonSet. The old
`services/collector/deploy/helm/trooper-agent` chart was removed.
Contract: contracts.md §20 + §22; details and env list in `services/collector/README.md`.

## Local development (target)

`make dev` → infrastructure via docker compose + services with hot reload via `air` + `apps/web` Vite dev server.
The web dashboard only reads real data from the api (ADR-013, ADR-028 — no mock mode); `src/data/*` are synchronous functions + remote cache,
screens do not know the data source. On first launch (no org) the onboarding screen creates the org + first project; data arrives once an SDK connects.

## Scale targets (v1)

- Ingest: 200k spans/s, 500k log lines/s (single region)
- Dashboard queries: p95 < 1 s (last 24 hours, rollup)
- Alert evaluation latency: < 60 s (periodic), < 10 s (stream)
