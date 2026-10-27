# trooper-agent (services/collector)

Database, message-broker, host and Kubernetes collector (modes below). In `db` mode it reads statistics from PostgreSQL, MySQL, Redis, MongoDB, Kafka and
RabbitMQ with read-only monitoring users and ships them to TaskTrooper as **OTLP metrics** (contracts §5)
via the OpenTelemetry Go SDK (`otlpmetrichttp`) to ingest-gw. It never consumes or publishes messages and
never runs anything but catalog/statistics queries.

```
targets (static YAML and/or GET /api/v1/internal/collector-config every 60 s)
   └─ one loop per target, every `interval` (15 s), per-target `timeout`
        └─ scraper (pgx / go-sql-driver / go-redis / mongo-driver v2 / franz-go kadm / RabbitMQ management API /
                    Prometheus text endpoints / gopsutil)
             └─ emitter: gauges (latest snapshot per target) + delta sums (query calls/ms/rows)
                  └─ OTLP/HTTP protobuf, gzip → {TT_DSN host}/v1/metrics, header x-trooper-key
```

A failing target never stops the agent: the error is logged (each distinct error once, repeats every
5 min), its gauges are dropped and `trooper.collector.up{trooper.collector.target, trooper.collector.kind}` = 0.
Panics inside a scraper are recovered the same way.

## Run

```bash
cd services/collector
make build test                      # GOWORK=off by default (module uses replace ../pkg)

# static config, local ingest-gw
TT_DSN=http://tt_dev_ingest_key@localhost:4318/1 \
ORDERS_DB_PASSWORD=… go run ./cmd/trooper-agent --config examples/agent.yaml

# targets from the api (+ optional static ones; static wins on id clashes)
TT_DSN=… TT_API_URL=http://localhost:8080 TT_API_KEY=tt_dev_api_key go run ./cmd/trooper-agent

# "test connection": scrape every target twice (5 s apart), print samples as JSON lines, exit 1 if any failed
go run ./cmd/trooper-agent --config examples/agent.yaml --once            # TT_DSN optional here
```

| Flag | Env | Default | |
|---|---|---|---|
| `--config` | `TT_COLLECTOR_CONFIG` | | static YAML target list |
| `--api-url`, `--api-key` | `TT_API_URL`, `TT_API_KEY` | | pull targets from the api every `--poll` |
| `--poll` | `TT_COLLECTOR_POLL` | `60s` | remote config interval; on failure the current targets are kept |
| `--dsn` | `TT_DSN` | | `<scheme>://<ingest key>@<host>[:port]/<project>` → endpoint `<scheme>://<host>/v1/metrics` + `x-trooper-key` |
| `--agent-modes` | `TT_AGENT_MODES` | `db` | `db`, `host`, `cluster` (comma list) — see [Agent modes](#agent-modes-db-host-cluster-contracts-20-adr-019) |
| `--modes` | `TT_COLLECTOR_MODES` | all | only targets whose `collector` is in the list (`agent,direct,cloud`) |
| `--host-metrics` | `TT_COLLECTOR_HOST_METRICS` | `false` | report this machine's CPU/memory/disk for `collector: agent` databases |
| `--host-disk-path` | `TT_COLLECTOR_HOST_DISK_PATH` | `/` | filesystem behind `trooper.db.disk` |
| `--once`, `--once-gap` | | `5s` | one-shot mode (see above) |
| `--log-level` | `TT_LOG_LEVEL` | `info` | JSON logs (`services/pkg/logx`) |

Resource: `service.name=trooper-agent`, `service.version`. Export interval = config `interval`.

## Agent modes: `db`, `host`, `cluster` (contracts §20, ADR-019)

`TT_AGENT_MODES` (`--agent-modes`, comma list, default `db`) selects what this process does; modes combine
(`host,cluster`, `cluster,db`). `db` is the database / broker collector described in the rest of this file; without
`db` no `--config` / `--api-url` is needed.
Each mode has its own emitter (own OTLP resource, own export interval) to the same `TT_DSN`.

| Mode | Interval | Source | Targets (`trooper.collector.up` kind / target) |
|---|---|---|---|
| `host` | 15 s | gopsutil (this host), top processes; containers from the **kubelet** when `NODE_IP` / `TT_KUBELET_URL` is set, else the **Docker Engine API** when the socket exists | `host` / host name; `kubelet` / node name or `docker` / host name |
| `cluster` | 30 s | Kubernetes API server (read-only list calls) + Warning events → OTLP **logs** | `k8s` / cluster name |

A source that fails (kubelet TLS/RBAC, no API access, docker socket permission) never stops the agent: the error is
logged once (repeats every 5 min), that target's gauges are dropped and `trooper.collector.up` = 0; other targets go on.
No client-go and no docker SDK: both APIs are spoken with plain `net/http` (small binary, few dependencies).

| Env | Default | |
|---|---|---|
| `TT_AGENT_MODES` | `db` | `db`, `host`, `cluster` |
| `TT_K8S_CLUSTER` | | `k8s.cluster.name` (set it for the DaemonSet too) |
| `NODE_NAME`, `NODE_IP` | | downward API (`spec.nodeName`, `status.hostIP`) → `k8s.node.name`, kubelet `https://$NODE_IP:10250` |
| `TT_KUBELET_URL` | `https://$NODE_IP:10250` | kubelet override (tests) |
| `TT_KUBELET_INSECURE` | `false` | skip kubelet serving-cert verification (self-signed kubelets: kind, minikube, kubeadm without `serverTLSBootstrap`); otherwise the SA `ca.crt` + system roots verify it |
| `TT_K8S_API_URL`, `TT_K8S_TOKEN`, `TT_K8S_INSECURE` | in-cluster `KUBERNETES_SERVICE_HOST` + SA token (re-read every request) + SA CA | API server override (tests, out-of-cluster) |
| `TT_AGENT_DOCKER_SOCKET` | `/var/run/docker.sock` | Docker Engine socket; missing → no container metrics outside K8s |
| `TT_AGENT_PROCFS` | `/host/proc` if it exists, else `/proc` | host `/proc` (→ gopsutil `HOST_PROC`); `HOST_SYS` / `TT_AGENT_SYSFS` likewise `/host/sys` |
| `TT_AGENT_ROOTFS` | `/host/root` if it exists | host `/` (→ `HOST_ROOT`, `HOST_ETC`): disk usage of host mounts, `/etc/hostname`, `/etc/machine-id` |
| `TT_AGENT_TOP_PROCESSES` | `20` | processes per host (`0` = off) |
| `TT_AGENT_HOSTNAME` | `NODE_NAME`, host `/etc/hostname`, `os.Hostname()` | `host.name` |
| `TT_CLOUD_PROVIDER`, `TT_CLOUD_REGION` | from the node object (`spec.providerID` scheme, `topology.kubernetes.io/region`) | `cloud.provider` / `cloud.region` |

**Resources.** host: `service.name=trooper-agent`, `host.name`, `host.id` (machine-id; else DMI uuid / IOPlatformUUID),
`os.type`, `host.arch`, `k8s.cluster.name`, `k8s.node.name`, `cloud.provider`, `cloud.region` (when known).
cluster: `service.name=trooper-agent`, `k8s.cluster.name`, `cloud.*`. Event logs: `service.name=k8s`, `k8s.cluster.name`.

**host** (all gauges):

| Metric | Source / rule |
|---|---|
| `trooper.host.cpu` (0..1) | 1 − Δ(idle+iowait)/Δtotal of `cpu.Times` between scrapes (first scrape: none) |
| `trooper.host.cpu_cores` | logical CPUs |
| `trooper.host.memory` / `.memory_bytes` | (total − available) / total, bytes |
| `trooper.host.disk` (0..1) | fullest local filesystem ≥ 100 MB (`/host/proc/1/mountinfo`; tmpfs, overlay, squashfs, nfs, kubelet/docker/snap mounts skipped; statfs through `TT_AGENT_ROOTFS`) |
| `trooper.host.disk_read` / `.disk_write` (byte/s) | Δ `diskstats` over whole disks (partitions, loop, ram, dm, md skipped) |
| `trooper.host.net_in` / `.net_out` (byte/s) | Δ `/host/proc/1/net/dev` (the host's namespace, not the pod's) over physical interfaces (lo, veth, docker, cni, cali, flannel, … skipped) |
| `trooper.host.load1`, `trooper.host.uptime_s` | `loadavg`, `uptime` |
| `trooper.process.cpu` (core), `.memory_bytes` (RSS), `.threads`, `.fds` (Linux, needs `SYS_PTRACE`) | top `TT_AGENT_TOP_PROCESSES`: ⌈N/2⌉ busiest by Δ CPU time, the rest by RSS. Attrs `process.pid`, `process.executable.name`, `process.command` = **first token of argv[0] only** (`postgres: checkpointer` → `postgres`, ≤ 128 bytes; arguments are never read into a metric — PII), `container.id` (64-hex id from `/proc/<pid>/cgroup`, empty for host processes) |

**Containers** (attrs `container.id`, `container.name`, `container.image.name`, `k8s.namespace.name`, `k8s.pod.name`,
`k8s.pod.uid`, `trooper.k8s.workload.kind`, `trooper.k8s.workload.name`, `trooper.service` — always all keys, empty
outside K8s):

| Metric | kubelet (K8s) | Docker Engine API |
|---|---|---|
| `trooper.container.cpu` (core) | `usageNanoCores` (else Δ `usageCoreNanoSeconds`) | Δ `cpu_usage.total_usage` / s |
| `.memory_bytes` | `workingSetBytes` | `usage − inactive_file` (cgroup v2; v1 `total_inactive_file`) |
| `.cpu_limit` / `.memory_limit_bytes` | pod spec `limits` (absent = unlimited, no sample) | `NanoCpus` or `CpuQuota/CpuPeriod`, `HostConfig.Memory` (0 = none) |
| `.restarts` (cumulative) | `containerStatuses[].restartCount` | `RestartCount` |
| `.net_in` / `.net_out` (byte/s) | Δ pod `network.rxBytes/txBytes` — pod network is shared, so it is reported **on the first container only** | Δ Σ `networks.*.rx_bytes/tx_bytes` |
| `.throttled` (0..1) | Δ`container_cpu_cfs_throttled_periods_total` / Δ`…_periods_total` from `/metrics/cadvisor` (optional: a 4xx disables it for 10 min) | Δ`throttling_data.throttled_periods` / Δ`periods` |

Workload in host mode: ReplicaSet → Deployment by stripping the `pod-template-hash` suffix, Job `<name>-<8+ digits>` →
CronJob (cluster mode resolves via the objects' ownerReferences). `trooper.service`: pod label `tasktrooper.io/service` >
`app.kubernetes.io/name` > `app` (else empty); Docker: label `tasktrooper.io/service`, else the container name.

**cluster**: `GET /api/v1/nodes`, `/api/v1/pods`, `/apis/apps/v1/{replicasets,deployments,statefulsets,daemonsets}`,
`/apis/batch/v1/{jobs,cronjobs}` (paged, `limit=500`), `/api/v1/events?fieldSelector=type=Warning`. Nodes and pods are
required; a forbidden/missing workload kind only drops that kind.

| Metric | Rule |
|---|---|
| `trooper.k8s.node.ready` / `.allocatable_cpu` / `.allocatable_memory` / `.pods` | Ready condition; `status.allocatable`; non-terminated pods on the node |
| `trooper.k8s.pod.ready` / `.restarts` | Ready condition; Σ `restartCount` |
| `trooper.k8s.pod.cpu_request` / `.memory_request` | Σ container requests (request defaults to the limit) |
| `trooper.k8s.pod.cpu_limit` / `.memory_limit` | Σ limits, only when **every** container has one |
| `trooper.k8s.workload.desired/ready/available/updated` | Deployment/StatefulSet `spec.replicas` + status; DaemonSet `desiredNumberScheduled`, `numberReady`, `numberAvailable`, `updatedNumberScheduled`; **Job** (standalone, running or finished < 24 h): `completions`, ready (else active) pods, `succeeded`, `succeeded`; **CronJob**: 1 (0 suspended), active jobs, 0 when the newest finished run failed else 1, active jobs |

Pod attrs: `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, `k8s.node.name`, `k8s.pod.phase`,
`trooper.k8s.pod.reason`, `trooper.k8s.workload.kind`, `trooper.k8s.workload.name`, `trooper.service`.
`trooper.k8s.pod.reason`: `status.reason` (Evicted) > a container's waiting reason > terminated reason > a
`lastState.terminated` reason younger than 15 min (so a restarted pod still says *OOMKilled*; CrashLoopBackOff after an
OOM kill reports `OOMKilled`). Bounded set: CrashLoopBackOff, OOMKilled, ImagePullBackOff, ErrImagePull,
CreateContainerConfigError, CreateContainerError, InvalidImageName, RunContainerError, ContainerCannotRun, Evicted, Error,
DeadlineExceeded, StartError; anything else (ContainerCreating, Completed) → empty.

**Warning events** → OTLP logs (OTel Go SDK `otlploghttp`, batched, `{TT_DSN host}/v1/logs`, `x-trooper-key`): body =
message, severity `warn` (SeverityNumber WARN), attrs `k8s.event.reason`, `k8s.event.type`, `k8s.object.kind`,
`k8s.object.name`, `k8s.namespace.name` (empty for cluster-scoped objects such as Nodes), `k8s.event.count`
(`series.count` for new-style events), timestamp = latest occurrence. Each uid + count is sent once; after a restart
only events from the last 5 min are sent (no replay of the cluster's history). Run **one** cluster-mode replica.

### Running outside Kubernetes (`docker run`)

```bash
docker run -d --name trooper-agent --restart unless-stopped \
  --pid host --user 0 \
  -v /proc:/host/proc:ro -v /sys:/host/sys:ro -v /:/host/root:ro,rslave \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -e TT_AGENT_MODES=host \
  -e TT_DSN=https://<ingest key>@ingest.tasktrooper.io/<project> \
  ghcr.io/tasktrooper/trooper-agent:0.2.0
```

`--pid host` + root (or `--cap-add SYS_PTRACE`) for other processes' `/proc` entries and fds; the docker socket is only
used for `GET`s, but the socket itself is not read-only at the API level — mount it only on hosts you trust the agent
with. `--network host` is not needed (host traffic is read from `/host/proc/1/net/dev`). Without Docker: run the binary
under systemd (`TT_AGENT_MODES=host`, no mounts needed). Locally on a laptop:
`go run ./cmd/trooper-agent --agent-modes host --once` prints one JSON line per sample (plus the resource).

### Kubernetes: Helm chart [`deploy/helm/trooper-agent`](../../deploy/helm/trooper-agent)

```bash
helm upgrade --install trooper-agent deploy/helm/trooper-agent -n tasktrooper --create-namespace \
  --set clusterName=prod-eks \
  --set dsn=https://<ingest key>@ingest.tasktrooper.io/<project> \
  --set ebpf.enabled=true                       # optional OBI DaemonSet
```

Renders: `<release>-host` DaemonSet (`TT_AGENT_MODES=host`, hostPID, `/proc` `/sys` `/` read-only, `NODE_NAME` / `NODE_IP`,
tolerates every taint, root with only `SYS_PTRACE` + `DAC_READ_SEARCH`, read-only root fs), `<release>-cluster`
Deployment (1 replica, `Recreate`, `TT_AGENT_MODES=cluster`, + `db` with `db.enabled` / `db.apiUrl` / `db.apiKey`), a
ServiceAccount + ClusterRole (get/list/watch on nodes, nodes/stats, nodes/proxy, nodes/metrics, pods, namespaces,
events, apps/*, batch/*), a Secret (`dsn`, `ingestKey`; or `existingSecret`). `host.kubelet.insecureSkipVerify=true` for
self-signed kubelets. `cloud.provider` / `cloud.region` override detection.
`ebpf.enabled=true` adds an **OpenTelemetry eBPF Instrumentation** DaemonSet (`otel/ebpf-instrument:v0.15.2`,
privileged, hostPID, own read-only RBAC) exporting OTLP/HTTP to `ebpf.endpoint` (default: scheme + host of `dsn`) with
`OTEL_EXPORTER_OTLP_HEADERS=x-trooper-key=<ingest key>`, `OTEL_EBPF_KUBE_CLUSTER_NAME`, and `ebpf.config` as its config
file (default: instrument every namespace except kube-system & co. and the release namespace; services already using an
OTel SDK are skipped by OBI). The DB-only chart in [`deploy/helm/trooper-agent`](deploy/helm/trooper-agent) (this
directory) stays for the database / broker agent.

## Config reference

Static YAML and the api JSON use the same camelCase fields (Postgres `databases` / `broker_clusters`
columns + collector-only knobs). Full example: [`examples/agent.yaml`](examples/agent.yaml). Unknown YAML
fields are rejected (typo safety); unknown JSON fields are ignored (forward compatibility).

**Top level:** `interval` (default `15s`; `"15s"` or seconds as a number), `timeout` (`10s`), `databases[]`, `brokerClusters[]`.

**`databases[]`**

| Field | Source | |
|---|---|---|
| `id`, `slug`, `name` | row | `trooper.db.id` = `slug` ‖ `id` ‖ `name`; `trooper.db.name` = `name` (the dependency name `postgres · orders-db`) |
| `engine` | row | `postgres` \| `mysql` \| `redis` \| `mongodb` |
| `host`, `port` | row | port defaults per engine |
| `collector` | row | `agent` \| `direct` \| `cloud` (filter with `--modes`) |
| `credentialsRef` | row | `env:NAME` or `file:/path`. The secret is a **full DSN/URL** (used as is) or a **bare password** (DSN built from host/port/user/database) |
| `cloudResourceId`, `env` | row | passed through, not used yet |
| `dsn` | agent | inline DSN (`$VARS` expanded); wins over `credentialsRef` |
| `user` | agent | default `trooper_monitor` (Redis: none / ACL user) |
| `database` | agent | Postgres/MySQL database to connect to (Postgres default `postgres`) |
| `tls` | agent | Postgres `sslmode` (default `prefer`), MySQL `tls=` value, Redis `"true"` → `rediss://` |
| `node` | agent | `trooper.db.node` (default host) |
| `hostMetrics` | agent | override `--host-metrics` for this database |
| `interval`, `timeout`, `topQueries` (50), `slowQueryMs` (500) | agent | |
| `topQueriesSource` | agent | MongoDB: `auto` (default: `$queryStats`, else the profiler) \| `querystats` \| `profiler` \| `none` |

**`brokerClusters[]`**

| Field | Source | |
|---|---|---|
| `id`, `slug`, `name` | row | `trooper.broker.cluster` = `slug` ‖ `id` ‖ `name` |
| `kind` | row | `kafka` \| `rabbitmq` |
| `endpoint` | row | Kafka bootstrap servers (comma separated) / RabbitMQ management URL (`http(s)://host:15672`) |
| `vhost` | row | RabbitMQ, default `/` |
| `auth` | row | `{ mechanism: none\|plain\|scram-sha-256\|scram-sha-512\|iam\|oauthbearer, tls: bool }` + agent-only `caFile`, `certFile`, `keyFile` (mTLS), `insecureSkipVerify`, and the IAM / OAuth fields below |
| `credentialsRef` | row | secret is `user:password` or a bare password (user from `user`, default `trooper`); IAM: `accessKeyId:secret[:sessionToken]` or empty; OAuth: `clientId:clientSecret` (with `tokenUrl`) or a static token |
| `auth.awsRegion`, `auth.awsProfile`, `auth.roleArn` | agent | **AWS MSK IAM** (`mechanism: iam`, alias `aws-msk-iam`; TLS forced): credentials from `credentialsRef` or the default AWS chain (env, shared config/profile, web identity/IRSA, ECS, EC2 IMDS), optionally assuming `roleArn` (STS, cached). The signing region comes from the broker host name |
| `auth.tokenUrl`, `auth.scopes`, `auth.extensions` | agent | **SASL OAUTHBEARER**: OAuth 2 client-credentials grant (token cached until 30 s before expiry), or a static token when `tokenUrl` is empty |
| `nodes[]` | agent | `{ name, brokerId, metricsUrl, diskMount }` — Prometheus endpoints for `trooper.broker.node.*` (see below) |
| `nodeMetrics` | agent | RabbitMQ: `auto` (default) probes the `rabbitmq_prometheus` plugin on `<management host>:15692/metrics` when no `nodes` are listed; `off` |
| `topicMetadataEvery` | agent | Kafka retention (DescribeConfigs) refresh, default `5m` |
| `collector` | row | as for databases |
| `user`, `topicFilter`, `groupFilter` (RE2), `interval`, `timeout` | agent | |

### `GET {TT_API_URL}/api/v1/internal/collector-config` (expected from the api)

`Authorization: Bearer <api key>`; 200 with the project's targets (the key's project). Ids may be numbers.

```json
{
  "interval": 15,
  "databases": [
    { "id": 12, "slug": "orders-db", "name": "orders-db", "engine": "postgres", "host": "10.0.0.5", "port": 5432,
      "collector": "direct", "credentialsRef": "env:ORDERS_DB_DSN", "cloudResourceId": null, "env": "production" }
  ],
  "brokerClusters": [
    { "id": 3, "slug": "kafka-main", "name": "kafka-main", "kind": "kafka", "endpoint": "b-1:9096,b-2:9096", "vhost": null,
      "auth": { "mechanism": "scram-sha-512", "tls": true }, "credentialsRef": "file:/secrets/kafka-main", "collector": "agent" }
  ]
}
```

`credentialsRef` is resolved **on the agent** (`env:` / `file:`); secrets never travel through the api.
The agent diff-applies the list every poll: new targets start, changed ones restart, removed ones stop
and their series disappear.

## Monitoring users

Same SQL the Connect-database drawer shows:

```sql
-- PostgreSQL (top queries need the extension + shared_preload_libraries = 'pg_stat_statements')
CREATE ROLE trooper_monitor LOGIN PASSWORD '…';
GRANT pg_monitor TO trooper_monitor;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
-- MySQL
CREATE USER 'trooper_monitor'@'%' IDENTIFIED BY '…';
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'trooper_monitor'@'%';
GRANT SELECT ON performance_schema.* TO 'trooper_monitor'@'%';
```
```js
// MongoDB (top queries: $queryStats needs 7.0.12+/8.0 — clusterMonitor includes queryStatsRead; on
// self-managed servers enable collection with setParameter internalQueryStatsRateLimit=-1 (or a rate).
// Profiler fallback: db.setProfilingLevel(1, {slowms: 100}) + read on <db>.system.profile)
db.getSiblingDB("admin").createUser({ user: "trooper_monitor", pwd: "…", roles: ["clusterMonitor"] })
```
Kafka: `Describe` on cluster, topics and groups, `DescribeConfigs` on topics (retention). MSK IAM policy:
`kafka-cluster:Connect`, `DescribeCluster`, `DescribeTopic`, `DescribeTopicDynamicConfiguration`, `DescribeGroup`.
RabbitMQ: a user with the `monitoring` tag and no vhost rights.

## What each source fills (contracts §5)

Gauges unless noted. "—" = not available from this source (comes from the host scraper in Agent mode or
the cloud integration in Cloud mode; the writer leaves the column NaN).

| Metric | PostgreSQL | MySQL | Redis |
|---|---|---|---|
| `trooper.db.connections` / `max_connections` | client backends in `pg_stat_activity` / `max_connections` | `Threads_connected` / `@@max_connections` | `connected_clients` / `maxclients` |
| `trooper.db.ops` (/s) | Δ(`xact_commit`+`xact_rollback`) | Δ`Questions` | Δ`total_commands_processed` |
| `trooper.db.hit_ratio` | Δ`blks_hit` / Δ(hit+read) | 1 − Δ`Innodb_buffer_pool_reads` / Δ`…read_requests` | Δ`keyspace_hits` / Δ(hits+misses) |
| `trooper.db.slow` (/min) | Δcalls of statements whose interval mean ≥ `slowQueryMs` | Δ`Slow_queries` | new `SLOWLOG` entries |
| `trooper.db.latency.p95` (ms) | calls-weighted p95 of statements' interval mean latency | same, over digests | p95 of PING RTT sampled every 500 ms |
| `trooper.db.replication_lag` (ms) | replica: `now() − pg_last_xact_replay_timestamp()` (0 when caught up); primary: per standby `pg_stat_replication.replay_lag` (node = application_name) | `Seconds_Behind_Source` × 1000 | replica: `master_last_io_seconds_ago`; master: per replica `slaveN lag=` (s since last ACK) |
| `trooper.db.memory` | — | — | `used_memory / maxmemory` (or `/ total_system_memory`) |
| `trooper.db.evictions` (/min) | — | — | Δ`evicted_keys` |
| `trooper.db.cpu` / `memory` / `disk` | `--host-metrics` (gopsutil) | `--host-metrics` | `--host-metrics` (memory: Redis keeps its own `used_memory/maxmemory`) |
| `trooper.db.role` attr | `pg_is_in_recovery()` | replica status present | `role` |
| `trooper.db.query.calls` / `total_ms` / `rows` (**delta sums**) | `pg_stat_statements` Δ per `queryid` | `events_statements_summary_by_digest` Δ per digest | `INFO commandstats` Δ per command (no rows) |
| `trooper.db.query.p95_ms` | mean + 2·stddev (cumulative; pg keeps no percentiles — ≈ p97.7 for a normal distribution, deliberately conservative) | `QUANTILE_95` (8.0+, else `AVG_TIMER_WAIT`); prepared statements: interval mean | — |

MySQL top queries also cover **server-side prepared statements** (binary protocol, `PREPARE`), which never reach
`events_statements_summary_by_digest`: `performance_schema.prepared_statements_instances` deltas per (thread, statement)
are merged into the digest of their text (`STATEMENT_DIGEST`, 8.0+, with `?` → `0` so the parser accepts it — the result
equals the text-protocol digest, so the same statement run both ways is one row). Only statements still allocated at
scrape time are seen (JDBC/connector statement caches keep them; prepare-execute-close per call is missed).

**MongoDB** (`db.system=mongodb`, user with `clusterMonitor`; a single-host `mongodb://` URI connects directly so
metrics are per node):

| Metric | Source |
|---|---|
| `connections` / `max_connections` | `serverStatus.connections.current` / current + available |
| `ops` (/s) | Δ Σ `opcounters` (insert, query, update, delete, getmore, command) |
| `hit_ratio` | 1 − Δ`pages read into cache` / Δ`pages requested from the cache` (WiredTiger) |
| `evictions` (/min) | Δ(`unmodified` + `modified pages evicted`) |
| `memory` | `mem.resident` / `hostInfo.system.memSizeMB` |
| `latency.p95` (ms) | Δ `opLatencies` histograms (`serverStatus({opLatencies:{histograms:true}})`) of reads + writes, p95 interpolated linearly inside the bucket (buckets are ~2–8× wide, so this is an approximation); commands are excluded (awaitable `hello`/getMore long-polls would dominate). No histograms → ops-weighted interval mean |
| `role` / `replication_lag` | `replSetGetStatus`: secondary → primary optime − own optime; primary → per secondary (`trooper.db.node` = member host); standalone → `primary`, no lag; arbiter → `arbiter` |
| `slow` (/min) | profiler only: profiled ops with `millis ≥ slowQueryMs` |
| `query.calls` / `total_ms` / `rows` | `$queryStats`: Δ`execCount` / Δ`totalExecMicros.sum` / Δ`docsReturned.sum` per key, merged per `queryShapeHash`; hash = first 64 bits of `queryShapeHash`; text = `<command> <db>.<coll> <shape JSON>` (literals are already `?string`/`?number`). Fallback: `system.profile` entries since the last scrape in every database with profiling on (only what the profiler kept — at level 1 just ops ≥ slowms), grouped by a normalized shape (literals → `"?"`, session/cursor fields dropped), hash = FNV-1a of that text |
| `query.p95_ms` | `$queryStats`: mean + 2σ from `totalExecMicros.sumOfSquares` (cumulative); profiler: interval p95 of `millis` |

Query attributes: `trooper.db.id`, `trooper.db.query.hash` (decimal uint64: pg `queryid`; MySQL first 64 bits
of `DIGEST`; Redis FNV-1a of the command), `db.query.text` (engine-normalized — `$1` / `?` — whitespace collapsed,
≤ 1 KB). Only the top `topQueries` statements by DB time in the interval are sent. A statement that first
appears between two scrapes counts entirely in that interval.

| Metric | Kafka (kadm) | RabbitMQ (management API) |
|---|---|---|
| `trooper.broker.lag` | end − committed per (group, topic, partition); assigned but never committed: end − log start | `messages_ready` (partition `0`) |
| `trooper.broker.end_offset` / `committed_offset` | ListOffsets(latest) / OffsetFetch | — |
| `trooper.broker.produce_rate` (msg/s) | Δend offset | queue `publish` rate |
| `trooper.broker.consume_rate` (msg/s) | Δcommitted offset | `deliver_get` rate (split across groups by consumer count) |
| `trooper.broker.unacked` | — | `messages_unacknowledged` (split by consumer count) |
| `trooper.broker.group.members` (+ `trooper.broker.group.state`) | DescribeGroups | consumers per group; `Stable` |
| `trooper.broker.group.rebalances` (/h) | sliding 1 h count (see below) | — |
| `trooper.broker.oldest_age_s` | — | now − `head_message_timestamp` (classic queues, when publishers set the `timestamp` property) |
| `trooper.broker.node.memory` | `nodes[].metricsUrl` (below) | `mem_used / mem_limit` (limit = high watermark) |
| `trooper.broker.node.disk` | `nodes[].metricsUrl`, else DescribeLogDirs 1 − Σusable / Σtotal per broker (Kafka 3.3+; node `broker-<id>` or the `nodes[]` name with that `brokerId`) | `disk_free_limit / disk_free` (proximity to the free-disk alarm, 1 = publishers blocked; the API has no volume total) |
| `trooper.broker.node.cpu` / `net_in` (MB/s) | `nodes[].metricsUrl` | Prometheus (plugin or node exporter) |
| `trooper.broker.node.fd` / `.proc` (custom, 0..1) | — | `fd_used / fd_total`, `proc_used / proc_total` (`/api/nodes`) |

Topic / queue metadata (`broker_topics_1m`, attrs `trooper.broker.cluster`, `messaging.system`, `messaging.destination.name`):

| `trooper.broker.topic.*` | Kafka | RabbitMQ (per queue) |
|---|---|---|
| `partitions` / `replication` | metadata (replication = max replica count) | `1` / quorum or stream `members` (classic `1`) |
| `retention_ms` | DescribeConfigs `retention.ms` (`-1` = unlimited), every `topicMetadataEvery` | `x-message-ttl` argument, else policy `message-ttl`, else `-1` |
| `size_bytes` | DescribeLogDirs, per partition the largest replica (one copy, not × RF) | `message_bytes` (ready + unacked bodies) |
| `in_rate` (msg/s) | Σ partitions Δend offset / s | `publish` rate |
| `avg_msg_bytes` | Δsize / Δend offsets over the interval; when there is no usable delta (first scrape, no traffic, segments deleted by retention) size / retained messages (end − start). **No record is fetched**: these are on-disk bytes, i.e. after producer compression and including record-batch overhead | `message_bytes / messages` |
| `bytes_in_rate` (byte/s) | Δsize / s (else in_rate × avg) | in_rate × avg |

**Broker node metrics (`nodes[].metricsUrl`)** — any Prometheus text endpoint; first match wins:

| | node exporter | Kafka JMX exporter | Redpanda `/public_metrics` | `rabbitmq_prometheus` (:15692) |
|---|---|---|---|---|
| cpu | 1 − Δ(idle+iowait)/Δall `node_cpu_seconds_total` | `java_lang_OperatingSystem_CpuLoad` (or `SystemCpuLoad`), else Δ`process_cpu_seconds_total` / processors | Δ`redpanda_cpu_busy_seconds_total` / s / shards | scheduler utilisation from `erlang_vm_msacc_*` (only with microstate accounting enabled) |
| memory | 1 − MemAvailable/MemTotal | 1 − Free/TotalMemorySize | allocated / (allocated + free) | resident / limit |
| disk | 1 − avail/size on `diskMount` (default `/`) | — (log dirs) | 1 − free/total | limit / available |
| net_in (MB/s) | Δ`node_network_receive_bytes_total` (not `lo`) | Δ`kafka_server_BrokerTopicMetrics_BytesIn…` (aggregate series, else Σ topics) | Δ`redpanda_kafka_request_bytes_total{redpanda_request="produce"}` | Δ`erlang_vm_statistics_bytes_received_total` |

Counter-based values need two scrapes. RabbitMQ: without `nodes`, `nodeMetrics: auto` probes the plugin on the management host
(that node only; re-probed every 10 min after a failure); the node name comes from `rabbitmq_identity_info`.

* Topics/queues nobody reads are reported with `messaging.consumer.group.name=""` (end offset + produce rate) so the UI can warn.
* **Kafka rebalances:** DescribeGroups has no generation id, so a generation is the sorted member-id set seen while
  the group is settled (Stable/Empty). A change, or any `PreparingRebalance`/`CompletingRebalance` observation in
  between, counts one rebalance. Rebalances shorter than the scrape interval with identical member ids are missed.
* **RabbitMQ group = consumer tag prefix:** trailing per-instance tokens are stripped (uuid, numbers, hex ids,
  tokens with ≥ 2 digits such as k8s pod suffixes): `billing-worker-5d8f7b9c4-x2k9p` → `billing-worker`,
  `mailer_3` → `mailer`, `worker2` → `worker2`. Server-generated tags (`amq.ctag-…`) and all-random tags → queue name.
  SDKs should use `<process>-<instance>` consumer tags.
* Kafka internal topics and topics starting with `_` are skipped.
* **KIP-848 consumer groups** (new protocol, Kafka 4.0+): when a broker advertises `ConsumerGroupDescribe` (API 69,
  checked every 10 min) groups are described with it; classic groups answer it with an error and fall back to
  DescribeGroups (ListGroups' type filter needs 3.8+ and is silently dropped by older brokers, so it is not relied on).
  State is title-cased (`Stable`, `Assigning`, `Reconciling`, `Empty`); a rebalance = a group epoch bump (member
  join/leave/subscription change), so unlike classic groups no rebalance is missed. Share/streams groups are not reported.

## Deferred / not covered

* host/cluster: no `watch` (cluster mode lists every 30 s — fine up to a few thousand pods; raise the Deployment's
  memory limit beyond that), no per-container network in K8s (pod network is on the first container), no GPU metrics,
  no Windows nodes, no containerd-only hosts outside K8s (Docker Engine API only), no Podman.
* Kafka share groups (KIP-932) and streams groups; Kerberos (GSSAPI).
* MongoDB sharded clusters via `mongos` (point the agent at each shard's mongod instead); `$queryStats` on
  self-managed servers needs `internalQueryStatsRateLimit` set (0 = off by default).
* Cloud mode (CloudWatch / RDS / MSK metrics) is a separate integration; this agent only does SQL/protocol scraping for `cloud` targets that have credentials.

## Helm

Host / cluster / eBPF on Kubernetes: the top-level chart [`deploy/helm/trooper-agent`](../../deploy/helm/trooper-agent)
(see [Agent modes](#kubernetes-helm-chart-deployhelmtrooper-agent)). Database / broker agent: this directory's chart
[`deploy/helm/trooper-agent`](deploy/helm/trooper-agent) (values `dsn`, `apiUrl`, `apiKey`, `targets`,
`integrations.<engine>.*`, `hostMetrics`, `kind: Deployment|DaemonSet`). The UI snippet works as is plus the DSN:

```bash
helm upgrade --install trooper-agent trooper/agent \
  --set dsn=https://<ingest key>@ingest.tasktrooper.io/<project> \
  --set integrations.postgres.enabled=true \
  --set integrations.postgres.address=localhost:5432 \
  --set integrations.postgres.user=trooper_monitor \
  --set integrations.postgres.passwordSecret.name=orders-db-monitor   # Secret key "password"
```

`integrations.mongodb.*` renders a MongoDB target (`topQueriesSource` too). For MSK IAM with IRSA set
`serviceAccount.annotations."eks\.amazonaws\.com/role-arn"`; broker `nodes[]` go into `targets.brokerClusters`.

`kind=DaemonSet` + `hostMetrics=true` mounts `/proc`, `/sys`, `/` read-only so CPU/memory/disk are the node's.

## Docker

```bash
docker build -f services/collector/Dockerfile -t trooper-agent services/   # context = services/ (needs ../pkg)
```

## Tests

```bash
make test                 # unit: rate/delta math, hit ratio, rebalance counting, DSN + config parsing,
                          # credentials, consumer-tag mapping, fake RabbitMQ API, agent → in-process OTLP receiver
                          # host/cluster: fake kubelet (TLS), Docker socket, API server; §20 name golden list;
                          # `--agent-modes host,cluster --once` → in-process OTLP metrics + logs receiver
make itest-up             # ttcol-postgres (pg_stat_statements), ttcol-mysql, ttcol-redis, ttcol-rabbitmq (+ :15692),
                          # ttcol-mongo (mongo:8 single-node replica set), ttcol-kafka4 (Apache Kafka 4.1, KIP-848)
make itest                # -tags integration: every scraper against the real servers + Redpanda (localhost:19092,
                          # /public_metrics on :9644, ttcol-* topic/group), and `trooper-agent --once` end to end
                          # against an OTLP test server (asserts db, query, lag, topic.*, oldest_age_s, node.* names)
make itest-down
```
Integration addresses can be overridden with `TTCOL_PG_ADMIN`, `TTCOL_MYSQL_ADMIN`, `TTCOL_REDIS`, `TTCOL_RABBIT_MGMT`,
`TTCOL_RABBIT_AMQP`, `TTCOL_RABBIT_PROM`, `TTCOL_KAFKA`, `TTCOL_KAFKA_METRICS`, `TTCOL_KAFKA4`, `TTCOL_MONGO` (see `internal/itest`).
