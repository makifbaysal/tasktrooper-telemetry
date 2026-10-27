-- GPUs (contracts §22): trooper-agent host mode reads nvidia-smi and sends trooper.host.gpu.* OTLP gauges
-- (attrs gpu.index, gpu.model, gpu.uuid). Same conventions as 003_infra_k8s_faas.sql: AggregatingMergeTree, one row per
-- GPU and minute, gauges SimpleAggregateFunction(max), an unreported value is -Inf (contracts §5). Idempotent.

CREATE TABLE IF NOT EXISTS telemetry.gpus_1m
(
    org_id        UInt32,
    project_id    UInt32,
    cluster       LowCardinality(String),                  -- k8s.cluster.name ('' = not Kubernetes)
    host          LowCardinality(String),                  -- host.name
    gpu_index     UInt8,                                   -- gpu.index
    gpu_uuid      String,                                  -- gpu.uuid
    model         LowCardinality(String),                  -- gpu.model
    minute        DateTime,
    utilization   SimpleAggregateFunction(max, Float64),   -- 0..1
    memory        SimpleAggregateFunction(max, Float64),   -- 0..1
    memory_bytes  SimpleAggregateFunction(max, Float64),   -- used bytes
    temperature_c SimpleAggregateFunction(max, Float64),
    power_w       SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, host, gpu_index, gpu_uuid, minute)
TTL minute + INTERVAL 30 DAY;
