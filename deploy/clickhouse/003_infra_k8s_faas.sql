-- Hosts, containers, processes, Kubernetes (ADR-019, contracts §20) and serverless functions (ADR-020, contracts §21).
-- Written by ingest-writer from trooper.host.* / trooper.container.* / trooper.process.* / trooper.k8s.* / trooper.faas.*
-- OTLP gauges. AggregatingMergeTree, one row per entity and minute: gauges are SimpleAggregateFunction(max) and a value
-- that was not reported is -Inf (contracts §5: readers treat non-finite values as missing); state strings are anyLast.
-- Empty `cluster` = not Kubernetes. Idempotent.

CREATE TABLE IF NOT EXISTS telemetry.hosts_1m
(
    org_id       UInt32,
    project_id   UInt32,
    cluster      LowCardinality(String),
    host         LowCardinality(String),                  -- host.name
    host_id      String,                                  -- host.id (machine-id)
    os           LowCardinality(String),
    arch         LowCardinality(String),
    node         LowCardinality(String),                  -- k8s.node.name
    minute       DateTime,
    cpu          SimpleAggregateFunction(max, Float64),   -- 0..1
    cpu_cores    SimpleAggregateFunction(max, Float64),
    memory       SimpleAggregateFunction(max, Float64),   -- 0..1
    memory_bytes SimpleAggregateFunction(max, Float64),
    disk         SimpleAggregateFunction(max, Float64),   -- 0..1, fullest filesystem
    disk_read    SimpleAggregateFunction(max, Float64),   -- bytes/s
    disk_write   SimpleAggregateFunction(max, Float64),
    net_in       SimpleAggregateFunction(max, Float64),   -- bytes/s
    net_out      SimpleAggregateFunction(max, Float64),
    load1        SimpleAggregateFunction(max, Float64),
    uptime_s     SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, host, minute)
TTL minute + INTERVAL 30 DAY;

-- `host` is part of the key (after the contract's columns) so equally named non-Kubernetes containers on different
-- hosts (cluster/namespace/pod empty) do not merge into one row.
CREATE TABLE IF NOT EXISTS telemetry.containers_1m
(
    org_id        UInt32,
    project_id    UInt32,
    cluster       LowCardinality(String),
    host          LowCardinality(String),
    namespace     LowCardinality(String),
    pod           String,
    pod_uid       String,
    container     LowCardinality(String),
    container_id  String,
    image         LowCardinality(String),
    workload_kind LowCardinality(String),
    workload      LowCardinality(String),
    service       LowCardinality(String),
    minute        DateTime,
    cpu           SimpleAggregateFunction(max, Float64),  -- cores
    cpu_limit     SimpleAggregateFunction(max, Float64),  -- cores
    memory_bytes  SimpleAggregateFunction(max, Float64),
    memory_limit  SimpleAggregateFunction(max, Float64),  -- bytes
    restarts      SimpleAggregateFunction(max, Float64),  -- cumulative counter
    net_in        SimpleAggregateFunction(max, Float64),  -- bytes/s
    net_out       SimpleAggregateFunction(max, Float64),
    throttled     SimpleAggregateFunction(max, Float64)   -- 0..1
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster, namespace, pod, container, host, minute)
TTL minute + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS telemetry.processes_1m
(
    org_id       UInt32,
    project_id   UInt32,
    host         LowCardinality(String),
    container_id String,
    pid          UInt32,
    exe          LowCardinality(String),
    command      String,                                  -- first token only, ≤ 128 chars
    minute       DateTime,
    cpu          SimpleAggregateFunction(max, Float64),   -- cores
    memory_bytes SimpleAggregateFunction(max, Float64),
    threads      SimpleAggregateFunction(max, Float64),
    fds          SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, host, pid, exe, minute)
TTL minute + INTERVAL 7 DAY;

CREATE TABLE IF NOT EXISTS telemetry.k8s_nodes_1m
(
    org_id             UInt32,
    project_id         UInt32,
    cluster            LowCardinality(String),
    node               LowCardinality(String),
    minute             DateTime,
    ready              SimpleAggregateFunction(max, Float64),  -- 0/1
    allocatable_cpu    SimpleAggregateFunction(max, Float64),  -- cores
    allocatable_memory SimpleAggregateFunction(max, Float64),  -- bytes
    pods               SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster, node, minute)
TTL minute + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS telemetry.k8s_pods_1m
(
    org_id         UInt32,
    project_id     UInt32,
    cluster        LowCardinality(String),
    namespace      LowCardinality(String),
    pod            String,
    pod_uid        String,
    node           LowCardinality(String),
    workload_kind  LowCardinality(String),
    workload       LowCardinality(String),
    service        LowCardinality(String),
    minute         DateTime,
    phase          SimpleAggregateFunction(anyLast, String),   -- Pending | Running | Succeeded | Failed | Unknown
    reason         SimpleAggregateFunction(anyLast, String),   -- CrashLoopBackOff | OOMKilled | ... ('' = none)
    ready          SimpleAggregateFunction(max, Float64),      -- 0/1
    restarts       SimpleAggregateFunction(max, Float64),      -- cumulative (sum over containers)
    cpu_request    SimpleAggregateFunction(max, Float64),      -- cores
    cpu_limit      SimpleAggregateFunction(max, Float64),
    memory_request SimpleAggregateFunction(max, Float64),      -- bytes
    memory_limit   SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster, namespace, pod, pod_uid, minute)
TTL minute + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS telemetry.k8s_workloads_1m
(
    org_id     UInt32,
    project_id UInt32,
    cluster    LowCardinality(String),
    namespace  LowCardinality(String),
    kind       LowCardinality(String),                    -- Deployment | StatefulSet | DaemonSet | Job | CronJob
    name       LowCardinality(String),
    minute     DateTime,
    desired    SimpleAggregateFunction(max, Float64),
    ready      SimpleAggregateFunction(max, Float64),
    available  SimpleAggregateFunction(max, Float64),
    updated    SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster, namespace, kind, name, minute)
TTL minute + INTERVAL 30 DAY;

-- Serverless (trooper-cloud: CloudWatch AWS/Lambda, Azure Monitor). Provider minute values; a range total is the sum
-- of the minutes. function_id = serverless_functions.slug.
CREATE TABLE IF NOT EXISTS telemetry.faas_1m
(
    org_id          UInt32,
    project_id      UInt32,
    function_id     LowCardinality(String),
    provider        LowCardinality(String),               -- aws | azure
    region          LowCardinality(String),
    minute          DateTime,
    invocations     SimpleAggregateFunction(max, Float64),
    errors          SimpleAggregateFunction(max, Float64),
    throttles       SimpleAggregateFunction(max, Float64),
    duration_avg    SimpleAggregateFunction(max, Float64), -- ms
    duration_max    SimpleAggregateFunction(max, Float64),
    duration_p95    SimpleAggregateFunction(max, Float64),
    duration_p99    SimpleAggregateFunction(max, Float64),
    concurrency     SimpleAggregateFunction(max, Float64),
    iterator_age_ms SimpleAggregateFunction(max, Float64),
    memory_bytes    SimpleAggregateFunction(max, Float64),
    execution_units SimpleAggregateFunction(max, Float64)  -- MB·ms (Azure)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, function_id, minute)
TTL minute + INTERVAL 30 DAY;
