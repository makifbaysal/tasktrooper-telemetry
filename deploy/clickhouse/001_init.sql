-- TaskTrooper Telemetry · ClickHouse schema (single node / dev).
-- Production: same DDL with ON CLUSTER, Replicated* engines and Distributed tables in front
-- (sharding key cityHash64(org_id, project_id, service)). See .ai/data-model.md and .ai/contracts.md.
-- Idempotent: every statement uses IF NOT EXISTS. Statements are separated by ";\n" (migration runner splits on that).

CREATE DATABASE IF NOT EXISTS telemetry;

-- ───────────────────────────── Raw: spans ─────────────────────────────
-- ingest-writer flattens OTLP spans into this table; everything the rollups need is a typed column,
-- the rest stays in attrs.
CREATE TABLE IF NOT EXISTS telemetry.spans
(
    org_id          UInt32,
    project_id      UInt32,
    ts              DateTime64(6) CODEC(Delta, ZSTD(1)),
    duration_ns     UInt64 CODEC(T64, ZSTD(1)),
    trace_id        FixedString(16),
    span_id         FixedString(8),
    parent_span_id  FixedString(8),
    service         LowCardinality(String),
    env             LowCardinality(String),
    version         LowCardinality(String),
    kind            Enum8('internal' = 1, 'server' = 2, 'client' = 3, 'producer' = 4, 'consumer' = 5),
    name            String,
    is_entry        UInt8,                          -- server/consumer span whose parent is in another service (or none)
    sample_weight   Float32 DEFAULT 1,              -- 1 / trooper.sample_rate
    route           LowCardinality(String),         -- entry: own route; consumer: process
    http_method     LowCardinality(String),
    status_code     UInt16,
    is_error        UInt8,
    error_type      LowCardinality(String),
    error_fingerprint UInt64,
    entry_route     LowCardinality(String),         -- trooper.entry.route (route of the request this span belongs to)
    entry_method    LowCardinality(String),
    caller          LowCardinality(String),         -- entry spans: who called (service, web, mobile, partner, webhook, probe, unknown)
    caller_kind     LowCardinality(String),
    dep             LowCardinality(String),         -- client/producer spans: "postgres · orders-db", "HTTP · payments-api", "kafka · orders.created"
    dep_kind        LowCardinality(String),         -- db | cache | http | queue | ''
    dep_service     LowCardinality(String),         -- callee service when known (peer.service), for edges
    topic           LowCardinality(String),         -- consumer/producer: messaging.destination.name
    consumer_group  LowCardinality(String),
    attempt         UInt16,
    dlq             UInt8,
    db_query_hash   UInt64,
    attrs           Map(LowCardinality(String), String) CODEC(ZSTD(3)),
    resource_attrs  Map(LowCardinality(String), String) CODEC(ZSTD(3)),
    events          Nested(ts DateTime64(6), name LowCardinality(String), attrs Map(LowCardinality(String), String)),

    INDEX idx_trace trace_id TYPE bloom_filter(0.001) GRANULARITY 4,
    INDEX idx_err error_fingerprint TYPE set(100) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, service, route, ts)
TTL toDateTime(ts) + INTERVAL 15 DAY
SETTINGS index_granularity = 8192;

-- Trace lookup: trace_id → (service, minute) so a trace page reads a few granules, not the whole day.
CREATE TABLE IF NOT EXISTS telemetry.trace_index
(
    org_id     UInt32,
    project_id UInt32,
    trace_id   FixedString(16),
    start      SimpleAggregateFunction(min, DateTime64(6)),
    finish     SimpleAggregateFunction(max, DateTime64(6)),
    services   SimpleAggregateFunction(groupUniqArrayArray, Array(String)),
    spans      SimpleAggregateFunction(sum, UInt64),
    errors     SimpleAggregateFunction(sum, UInt64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toDate(start)
ORDER BY (org_id, project_id, trace_id)
TTL toDateTime(start) + INTERVAL 15 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.trace_index_mv TO telemetry.trace_index AS
SELECT org_id, project_id, trace_id,
       min(ts) AS start, max(ts + toIntervalMicrosecond(intDiv(duration_ns, 1000))) AS finish,
       groupUniqArray(service) AS services, count() AS spans, countIf(is_error = 1) AS errors
FROM telemetry.spans
GROUP BY org_id, project_id, trace_id;

-- ───────────────────────────── Rollups from spans ─────────────────────────────
-- RED per endpoint (entry server spans). Counts are multiplied by sample_weight so sampling never skews RED.
CREATE TABLE IF NOT EXISTS telemetry.red_1m
(
    org_id      UInt32,
    project_id  UInt32,
    service     LowCardinality(String),
    env         LowCardinality(String),
    version     LowCardinality(String),
    route       LowCardinality(String),
    method      LowCardinality(String),
    minute      DateTime,
    count       SimpleAggregateFunction(sum, Float64),
    errors      SimpleAggregateFunction(sum, Float64),
    s2xx        SimpleAggregateFunction(sum, Float64),
    s3xx        SimpleAggregateFunction(sum, Float64),
    s4xx        SimpleAggregateFunction(sum, Float64),
    s5xx        SimpleAggregateFunction(sum, Float64),
    duration_sum SimpleAggregateFunction(sum, Float64),           -- ms
    duration    AggregateFunction(quantilesTDigestWeighted(0.5, 0.75, 0.95, 0.99), Float64, UInt32)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, service, env, route, method, version, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.red_1m_mv TO telemetry.red_1m AS
SELECT org_id, project_id, service, env, version, route, http_method AS method,
       toStartOfMinute(ts) AS minute,
       sum(sample_weight) AS count,
       sumIf(sample_weight, is_error = 1) AS errors,
       sumIf(sample_weight, status_code BETWEEN 200 AND 299) AS s2xx,
       sumIf(sample_weight, status_code BETWEEN 300 AND 399) AS s3xx,
       sumIf(sample_weight, status_code BETWEEN 400 AND 499) AS s4xx,
       sumIf(sample_weight, status_code >= 500) AS s5xx,
       sum(sample_weight * duration_ns / 1e6) AS duration_sum,
       quantilesTDigestWeightedState(0.5, 0.75, 0.95, 0.99)(duration_ns / 1e6, toUInt32(greatest(1, round(sample_weight)))) AS duration
FROM telemetry.spans
WHERE is_entry = 1 AND kind = 'server'
GROUP BY org_id, project_id, service, env, version, route, method, minute;

-- Hourly downsample (13 months).
CREATE TABLE IF NOT EXISTS telemetry.red_1h AS telemetry.red_1m
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, service, env, route, method, version, minute)
TTL minute + INTERVAL 13 MONTH;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.red_1h_mv TO telemetry.red_1h AS
SELECT org_id, project_id, service, env, version, route, method, toStartOfHour(minute) AS minute,
       sum(count) AS count, sum(errors) AS errors, sum(s2xx) AS s2xx, sum(s3xx) AS s3xx, sum(s4xx) AS s4xx, sum(s5xx) AS s5xx,
       sum(duration_sum) AS duration_sum, quantilesTDigestWeightedMergeState(0.5, 0.75, 0.95, 0.99)(duration) AS duration
FROM telemetry.red_1m
GROUP BY org_id, project_id, service, env, version, route, method, minute;

-- Consumers: unit is (service, process); topic is a separate dimension.
CREATE TABLE IF NOT EXISTS telemetry.consumer_1m
(
    org_id      UInt32,
    project_id  UInt32,
    service     LowCardinality(String),
    env         LowCardinality(String),
    process     LowCardinality(String),
    topic       LowCardinality(String),
    consumer_group LowCardinality(String),
    minute      DateTime,
    processed   SimpleAggregateFunction(sum, Float64),
    failed      SimpleAggregateFunction(sum, Float64),
    retried     SimpleAggregateFunction(sum, Float64),
    dlq         SimpleAggregateFunction(sum, Float64),
    duration_sum SimpleAggregateFunction(sum, Float64),
    duration    AggregateFunction(quantilesTDigestWeighted(0.5, 0.95, 0.99), Float64, UInt32)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, service, env, process, topic, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.consumer_1m_mv TO telemetry.consumer_1m AS
SELECT org_id, project_id, service, env, route AS process, topic, consumer_group,
       toStartOfMinute(ts) AS minute,
       sum(sample_weight) AS processed,
       sumIf(sample_weight, is_error = 1) AS failed,
       sumIf(sample_weight, attempt > 1) AS retried,
       sumIf(sample_weight, dlq = 1) AS dlq,
       sum(sample_weight * duration_ns / 1e6) AS duration_sum,
       quantilesTDigestWeightedState(0.5, 0.95, 0.99)(duration_ns / 1e6, toUInt32(greatest(1, round(sample_weight)))) AS duration
FROM telemetry.spans
WHERE is_entry = 1 AND kind = 'consumer'
GROUP BY org_id, project_id, service, env, process, topic, consumer_group, minute;

-- Outgoing calls per endpoint (deps_1m in data-model.md) == call-tree edges for RCA.
-- parent = (service, entry_route) of the request; child = dependency (db/cache/http/queue).
CREATE TABLE IF NOT EXISTS telemetry.edges_1m
(
    org_id          UInt32,
    project_id      UInt32,
    env             LowCardinality(String),
    parent_service  LowCardinality(String),
    parent_route    LowCardinality(String),
    parent_method   LowCardinality(String),
    dep             LowCardinality(String),
    dep_kind        LowCardinality(String),
    child_service   LowCardinality(String),        -- callee service for http deps when instrumented
    minute          DateTime,
    calls           SimpleAggregateFunction(sum, Float64),
    errors          SimpleAggregateFunction(sum, Float64),
    duration_sum    SimpleAggregateFunction(sum, Float64),
    duration        AggregateFunction(quantilesTDigestWeighted(0.5, 0.95), Float64, UInt32)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, parent_service, parent_route, dep, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.edges_1m_mv TO telemetry.edges_1m AS
SELECT org_id, project_id, env, service AS parent_service, entry_route AS parent_route, entry_method AS parent_method,
       dep, dep_kind, dep_service AS child_service,
       toStartOfMinute(ts) AS minute,
       sum(sample_weight) AS calls,
       sumIf(sample_weight, is_error = 1) AS errors,
       sum(sample_weight * duration_ns / 1e6) AS duration_sum,
       quantilesTDigestWeightedState(0.5, 0.95)(duration_ns / 1e6, toUInt32(greatest(1, round(sample_weight)))) AS duration
FROM telemetry.spans
WHERE kind IN ('client', 'producer') AND dep != ''
GROUP BY org_id, project_id, env, parent_service, parent_route, parent_method, dep, dep_kind, child_service, minute;

-- Incoming traffic by caller.
CREATE TABLE IF NOT EXISTS telemetry.callers_1m
(
    org_id      UInt32,
    project_id  UInt32,
    service     LowCardinality(String),
    env         LowCardinality(String),
    route       LowCardinality(String),
    method      LowCardinality(String),
    caller      LowCardinality(String),
    caller_kind LowCardinality(String),
    minute      DateTime,
    count       SimpleAggregateFunction(sum, Float64),
    errors      SimpleAggregateFunction(sum, Float64),
    duration    AggregateFunction(quantilesTDigestWeighted(0.5, 0.95), Float64, UInt32)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, service, route, caller, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.callers_1m_mv TO telemetry.callers_1m AS
SELECT org_id, project_id, service, env, route, http_method AS method, caller, caller_kind,
       toStartOfMinute(ts) AS minute,
       sum(sample_weight) AS count,
       sumIf(sample_weight, is_error = 1) AS errors,
       quantilesTDigestWeightedState(0.5, 0.95)(duration_ns / 1e6, toUInt32(greatest(1, round(sample_weight)))) AS duration
FROM telemetry.spans
WHERE is_entry = 1 AND kind = 'server'
GROUP BY org_id, project_id, service, env, route, method, caller, caller_kind, minute;

-- Error issues (counts; status lives in Postgres error_issues).
CREATE TABLE IF NOT EXISTS telemetry.errors_1h
(
    org_id      UInt32,
    project_id  UInt32,
    service     LowCardinality(String),
    fingerprint UInt64,
    hour        DateTime,
    type        SimpleAggregateFunction(any, String),
    message     SimpleAggregateFunction(any, String),
    route       SimpleAggregateFunction(any, String),
    first_seen  SimpleAggregateFunction(min, DateTime64(6)),
    last_seen   SimpleAggregateFunction(max, DateTime64(6)),
    count       SimpleAggregateFunction(sum, Float64),
    sample_trace SimpleAggregateFunction(any, FixedString(16))
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (org_id, project_id, service, fingerprint, hour)
TTL hour + INTERVAL 90 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.errors_1h_mv TO telemetry.errors_1h AS
SELECT org_id, project_id, service, error_fingerprint AS fingerprint, toStartOfHour(ts) AS hour,
       any(error_type) AS type, any(attrs['exception.message']) AS message, any(entry_route) AS route,
       min(ts) AS first_seen, max(ts) AS last_seen, sum(sample_weight) AS count, any(trace_id) AS sample_trace
FROM telemetry.spans
WHERE is_error = 1 AND error_fingerprint != 0
GROUP BY org_id, project_id, service, fingerprint, hour;

-- ───────────────────────────── Logs ─────────────────────────────
CREATE TABLE IF NOT EXISTS telemetry.logs
(
    org_id      UInt32,
    project_id  UInt32,
    ts          DateTime64(6) CODEC(Delta, ZSTD(1)),
    service     LowCardinality(String),
    env         LowCardinality(String),
    severity    Enum8('trace' = 1, 'debug' = 2, 'info' = 3, 'warn' = 4, 'error' = 5, 'fatal' = 6),
    body        String CODEC(ZSTD(3)),
    trace_id    FixedString(16),
    span_id     FixedString(8),
    route       LowCardinality(String),           -- from the active span's trooper.entry.route
    pattern_id  UInt64,                           -- Drain template id
    attrs       Map(LowCardinality(String), String) CODEC(ZSTD(3)),

    INDEX idx_body body TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 1,
    INDEX idx_trace trace_id TYPE bloom_filter(0.001) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, service, severity, ts)
TTL toDateTime(ts) + INTERVAL 15 DAY;

CREATE TABLE IF NOT EXISTS telemetry.log_patterns_1m
(
    org_id      UInt32,
    project_id  UInt32,
    service     LowCardinality(String),
    severity    LowCardinality(String),
    pattern_id  UInt64,
    minute      DateTime,
    count       SimpleAggregateFunction(sum, UInt64),
    sample      SimpleAggregateFunction(any, String)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, service, pattern_id, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.log_patterns_1m_mv TO telemetry.log_patterns_1m AS
SELECT org_id, project_id, service, toString(severity) AS severity, pattern_id, toStartOfMinute(ts) AS minute,
       count() AS count, any(body) AS sample
FROM telemetry.logs
GROUP BY org_id, project_id, service, severity, pattern_id, minute;

-- ───────────────────────────── Metrics ─────────────────────────────
-- Custom / runtime OTLP metrics. Histograms are stored as their sum/count + bucket arrays.
CREATE TABLE IF NOT EXISTS telemetry.metrics
(
    org_id      UInt32,
    project_id  UInt32,
    ts          DateTime64(3) CODEC(Delta, ZSTD(1)),
    service     LowCardinality(String),
    env         LowCardinality(String),
    name        LowCardinality(String),
    type        Enum8('gauge' = 1, 'sum' = 2, 'histogram' = 3, 'exp_histogram' = 4, 'summary' = 5),
    unit        LowCardinality(String),
    attrs       Map(LowCardinality(String), String),
    attrs_hash  UInt64,                            -- cityHash64 of sorted attrs → series id
    value       Float64 CODEC(Gorilla, ZSTD(1)),
    count       UInt64,
    bucket_bounds Array(Float64),
    bucket_counts Array(UInt64)
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, name, service, attrs_hash, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY;

-- ───────────────────────────── Infrastructure (collector) ─────────────────────────────
-- Writer maps trooper.db.* gauges into one row per (database, node, minute). Columns not reported stay NaN.
CREATE TABLE IF NOT EXISTS telemetry.db_1m
(
    org_id          UInt32,
    project_id      UInt32,
    database_id     LowCardinality(String),
    engine          LowCardinality(String),
    node            LowCardinality(String),
    role            LowCardinality(String),
    minute          DateTime,
    cpu             SimpleAggregateFunction(max, Float64),
    memory          SimpleAggregateFunction(max, Float64),
    disk            SimpleAggregateFunction(max, Float64),
    connections     SimpleAggregateFunction(max, Float64),
    max_connections SimpleAggregateFunction(max, Float64),
    ops             SimpleAggregateFunction(max, Float64),
    p95_ms          SimpleAggregateFunction(max, Float64),
    slow            SimpleAggregateFunction(max, Float64),
    hit_ratio       SimpleAggregateFunction(min, Float64),
    evictions       SimpleAggregateFunction(max, Float64),
    repl_lag_ms     SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, database_id, node, minute)
TTL minute + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS telemetry.db_queries_1m
(
    org_id      UInt32,
    project_id  UInt32,
    database_id LowCardinality(String),
    query_hash  UInt64,
    minute      DateTime,
    query       SimpleAggregateFunction(any, String),     -- normalized text, no literals
    calls       SimpleAggregateFunction(sum, Float64),
    total_ms    SimpleAggregateFunction(sum, Float64),
    p95_ms      SimpleAggregateFunction(max, Float64),
    rows        SimpleAggregateFunction(sum, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, database_id, query_hash, minute)
TTL minute + INTERVAL 30 DAY;

-- Lag per (cluster, group, topic, partition). RabbitMQ: topic = queue, partition = 0, lag = messages_ready.
CREATE TABLE IF NOT EXISTS telemetry.broker_lag_1m
(
    org_id           UInt32,
    project_id       UInt32,
    cluster_id       LowCardinality(String),
    consumer_group   LowCardinality(String),
    topic            LowCardinality(String),
    partition        UInt32,
    minute           DateTime,
    lag              SimpleAggregateFunction(max, Float64),
    end_offset       SimpleAggregateFunction(max, Float64),
    committed_offset SimpleAggregateFunction(max, Float64),
    consume_rate     SimpleAggregateFunction(max, Float64),
    produce_rate     SimpleAggregateFunction(max, Float64),
    unacked          SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster_id, consumer_group, topic, partition, minute)
TTL minute + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS telemetry.broker_groups_1m
(
    org_id         UInt32,
    project_id     UInt32,
    cluster_id     LowCardinality(String),
    consumer_group LowCardinality(String),
    minute         DateTime,
    state          SimpleAggregateFunction(anyLast, String),
    members        SimpleAggregateFunction(max, Float64),
    rebalances     SimpleAggregateFunction(max, Float64)        -- per hour, as reported
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster_id, consumer_group, minute)
TTL minute + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS telemetry.broker_nodes_1m
(
    org_id     UInt32,
    project_id UInt32,
    cluster_id LowCardinality(String),
    node       LowCardinality(String),
    minute     DateTime,
    cpu        SimpleAggregateFunction(max, Float64),
    memory     SimpleAggregateFunction(max, Float64),
    disk       SimpleAggregateFunction(max, Float64),
    net_in     SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster_id, node, minute)
TTL minute + INTERVAL 30 DAY;

-- ───────────────────────────── RUM ─────────────────────────────
CREATE TABLE IF NOT EXISTS telemetry.rum_events
(
    org_id        UInt32,
    project_id    UInt32,
    ts            DateTime64(3) CODEC(Delta, ZSTD(1)),
    received_at   DateTime64(3),
    app           LowCardinality(String),          -- service name of the client app
    env           LowCardinality(String),
    app_version   LowCardinality(String),
    platform      LowCardinality(String),          -- web | android | ios | flutter | react_native
    type          LowCardinality(String),          -- session_start | view_start | view_end | vital | error | resource | action | app_start | custom
    session_id    String,
    view_id       String,
    view_name     LowCardinality(String),
    anonymous_id  String,
    user_id       String,
    country       LowCardinality(String),
    device_type   LowCardinality(String),
    os            LowCardinality(String),
    browser       LowCardinality(String),
    connection    LowCardinality(String),
    duration_ms   Float64,                         -- view loading / time spent / resource duration / app start
    vital_name    LowCardinality(String),
    vital_value   Float64,
    error_type    LowCardinality(String),
    error_message String,
    error_stack   String CODEC(ZSTD(3)),
    error_fingerprint UInt64,
    is_crash      UInt8,
    resource_url  String,                           -- template-normalized
    resource_method LowCardinality(String),
    status_code   UInt16,
    trace_id      FixedString(16),
    action_name   String,
    rage          UInt8,
    dead          UInt8,
    name          LowCardinality(String),           -- custom event name
    attrs         Map(LowCardinality(String), String) CODEC(ZSTD(3)),

    INDEX idx_session session_id TYPE bloom_filter(0.01) GRANULARITY 4,
    INDEX idx_user user_id TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, app, type, view_name, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS telemetry.rum_views_1m
(
    org_id      UInt32,
    project_id  UInt32,
    app         LowCardinality(String),
    env         LowCardinality(String),
    view_name   LowCardinality(String),
    device_type LowCardinality(String),
    minute      DateTime,
    views       SimpleAggregateFunction(sum, UInt64),
    errors      SimpleAggregateFunction(sum, UInt64),
    crashes     SimpleAggregateFunction(sum, UInt64),
    sessions    AggregateFunction(uniq, String),
    users       AggregateFunction(uniq, String),     -- anonymous_id
    load        AggregateFunction(quantilesTDigest(0.5, 0.75, 0.95), Float64),
    time_spent_sum SimpleAggregateFunction(sum, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, app, view_name, device_type, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.rum_views_1m_mv TO telemetry.rum_views_1m AS
SELECT org_id, project_id, app, env, view_name, device_type, toStartOfMinute(ts) AS minute,
       countIf(type = 'view_start') AS views,
       countIf(type = 'error') AS errors,
       countIf(type = 'error' AND is_crash = 1) AS crashes,
       uniqState(session_id) AS sessions,
       uniqState(anonymous_id) AS users,
       quantilesTDigestStateIf(0.5, 0.75, 0.95)(duration_ms, type = 'view_start' AND duration_ms > 0) AS load,
       sumIf(duration_ms, type = 'view_end') AS time_spent_sum
FROM telemetry.rum_events
GROUP BY org_id, project_id, app, env, view_name, device_type, minute;

CREATE TABLE IF NOT EXISTS telemetry.rum_vitals_1m
(
    org_id      UInt32,
    project_id  UInt32,
    app         LowCardinality(String),
    env         LowCardinality(String),
    view_name   LowCardinality(String),
    vital_name  LowCardinality(String),
    device_type LowCardinality(String),
    minute      DateTime,
    count       SimpleAggregateFunction(sum, UInt64),
    value       AggregateFunction(quantilesTDigest(0.5, 0.75, 0.95), Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, app, vital_name, view_name, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.rum_vitals_1m_mv TO telemetry.rum_vitals_1m AS
SELECT org_id, project_id, app, env, view_name, vital_name, device_type, toStartOfMinute(ts) AS minute,
       count() AS count, quantilesTDigestState(0.5, 0.75, 0.95)(vital_value) AS value
FROM telemetry.rum_events
WHERE type = 'vital'
GROUP BY org_id, project_id, app, env, view_name, vital_name, device_type, minute;

-- Network calls from clients (feeds the "HTTP · <backend>" view on client apps).
CREATE TABLE IF NOT EXISTS telemetry.rum_resources_1m
(
    org_id      UInt32,
    project_id  UInt32,
    app         LowCardinality(String),
    resource_url LowCardinality(String),
    method      LowCardinality(String),
    minute      DateTime,
    calls       SimpleAggregateFunction(sum, UInt64),
    errors      SimpleAggregateFunction(sum, UInt64),
    duration    AggregateFunction(quantilesTDigest(0.5, 0.95), Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, app, resource_url, method, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.rum_resources_1m_mv TO telemetry.rum_resources_1m AS
SELECT org_id, project_id, app, resource_url, resource_method AS method, toStartOfMinute(ts) AS minute,
       count() AS calls, countIf(status_code = 0 OR status_code >= 500) AS errors,
       quantilesTDigestState(0.5, 0.95)(duration_ms) AS duration
FROM telemetry.rum_events
WHERE type = 'resource'
GROUP BY org_id, project_id, app, resource_url, method, minute;

-- ───────────────────────────── KPI ─────────────────────────────
-- Good/total per KPI per minute; alert-engine writes this from red_1m/consumer_1m + KPI definitions
-- (KPI good-event rules are evaluated on rollups, not at ingest, so a KPI edit re-computes history).
CREATE TABLE IF NOT EXISTS telemetry.sli_1m
(
    org_id     UInt32,
    project_id UInt32,
    kpi_id     UInt64,
    minute     DateTime,
    good       SimpleAggregateFunction(sum, Float64),
    total      SimpleAggregateFunction(sum, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, kpi_id, minute)
TTL minute + INTERVAL 35 DAY;

-- Deploy markers (service.version first seen per env).
CREATE TABLE IF NOT EXISTS telemetry.deploys
(
    org_id     UInt32,
    project_id UInt32,
    service    LowCardinality(String),
    env        LowCardinality(String),
    version    LowCardinality(String),
    first_seen SimpleAggregateFunction(min, DateTime)
)
ENGINE = AggregatingMergeTree
ORDER BY (org_id, project_id, service, env, version);

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.deploys_mv TO telemetry.deploys AS
SELECT org_id, project_id, service, env, version, min(toDateTime(ts)) AS first_seen
FROM telemetry.spans
WHERE version != ''
GROUP BY org_id, project_id, service, env, version;
