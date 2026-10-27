-- Session replay (ADR-021, contracts §23), continuous profiling (ADR-022, §24), AI / LLM monitoring (ADR-023, §25).
-- Same conventions as 001_init.sql: single node / dev DDL, idempotent (IF NOT EXISTS), statements separated by ";\n".

-- ───────────────────────────── Session replay ─────────────────────────────
-- One row per uploaded segment; the segment body lives in S3 (blob_key, bucket TT_REPLAY_S3_BUCKET).
-- A segment delivered twice (at-least-once) has two rows: queries dedupe with argMax(…, received_at).
CREATE TABLE IF NOT EXISTS telemetry.replay_segments
(
    org_id            UInt32,
    project_id        UInt32,
    session_id        String,
    segment           UInt32,
    service           LowCardinality(String),        -- resource.service (client app)
    env               LowCardinality(String),
    version           LowCardinality(String),
    platform          LowCardinality(String),        -- web | android | ios
    format            LowCardinality(String),        -- rrweb | wireframe
    start_ts          DateTime64(3),
    end_ts            DateTime64(3),
    events            UInt32,
    bytes             UInt32,                         -- compressed (gzip) blob size
    blob_key          String,                         -- <org>/<project>/<yyyy-mm-dd>/<session_id>/<segment:06d>.json.gz
    has_full_snapshot UInt8,
    clicks            UInt16,
    errors            UInt16,
    rage_clicks       UInt16,
    view_name         String,
    user_id           String,
    anonymous_id      String,
    received_at       DateTime64(3)
)
ENGINE = MergeTree
PARTITION BY toDate(start_ts)
ORDER BY (org_id, project_id, session_id, segment)
TTL toDateTime(start_ts) + INTERVAL 30 DAY;

-- Session list: one logical row per (service, session). Read with min/max/sum/any/argMinMerge … GROUP BY.
CREATE TABLE IF NOT EXISTS telemetry.replay_sessions
(
    org_id        UInt32,
    project_id    UInt32,
    service       LowCardinality(String),
    session_id    String,
    start_ts      SimpleAggregateFunction(min, DateTime64(3)),
    end_ts        SimpleAggregateFunction(max, DateTime64(3)),
    segments      SimpleAggregateFunction(sum, UInt64),
    bytes         SimpleAggregateFunction(sum, UInt64),
    clicks        SimpleAggregateFunction(sum, UInt64),
    errors        SimpleAggregateFunction(sum, UInt64),
    rage_clicks   SimpleAggregateFunction(sum, UInt64),
    env           SimpleAggregateFunction(any, String),
    version       SimpleAggregateFunction(any, String),
    platform      SimpleAggregateFunction(any, String),
    format        SimpleAggregateFunction(any, String),
    user_id       SimpleAggregateFunction(max, String),         -- max: a non-empty id wins over ''
    anonymous_id  SimpleAggregateFunction(max, String),
    first_view    AggregateFunction(argMin, String, DateTime64(3))
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(start_ts)
ORDER BY (org_id, project_id, service, session_id)
TTL toDateTime(start_ts) + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.replay_sessions_mv TO telemetry.replay_sessions AS
SELECT org_id, project_id, service, session_id,
       min(start_ts) AS start_ts, max(end_ts) AS end_ts,
       count() AS segments, sum(bytes) AS bytes,
       sum(clicks) AS clicks, sum(errors) AS errors, sum(rage_clicks) AS rage_clicks,
       any(toString(env)) AS env, any(toString(version)) AS version,
       any(toString(platform)) AS platform, any(toString(format)) AS format,
       max(s.user_id) AS user_id, max(s.anonymous_id) AS anonymous_id,
       argMinState(s.view_name, s.start_ts) AS first_view
FROM telemetry.replay_segments AS s
GROUP BY org_id, project_id, service, session_id;

-- ───────────────────────────── Continuous profiling ─────────────────────────────
-- One row per profile × type × (route, method) × unique stack; stack is root first. Identical stacks are summed by the
-- writer; ≤ 10 000 stacks per profile and type (the smallest are folded into the single-frame stack ['[other]']).
CREATE TABLE IF NOT EXISTS telemetry.profile_stacks
(
    org_id      UInt32,
    project_id  UInt32,
    ts          DateTime CODEC(Delta, ZSTD(1)),       -- profile start
    service     LowCardinality(String),
    env         LowCardinality(String),
    version     LowCardinality(String),
    host        LowCardinality(String),
    runtime     LowCardinality(String),               -- go | jvm | node | dotnet | python
    type        LowCardinality(String),               -- cpu | wall | alloc_space | alloc_objects | inuse_space | inuse_objects | goroutines | lock | block
    unit        LowCardinality(String),               -- nanoseconds | bytes | count
    route       LowCardinality(String),               -- pprof label / folded section trooper.route ('' = untagged)
    method      LowCardinality(String),
    stack_hash  UInt64 DEFAULT cityHash64(stack),
    stack       Array(LowCardinality(String)) CODEC(ZSTD(3)),
    value       Int64
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, service, type, ts, stack_hash)
TTL ts + INTERVAL 14 DAY;

-- One row per profile × type: totals for the time series (cpu cores = total / ((end_ts - ts) in ns)).
CREATE TABLE IF NOT EXISTS telemetry.profiles
(
    org_id      UInt32,
    project_id  UInt32,
    ts          DateTime64(3),                        -- profile start
    end_ts      DateTime64(3),
    service     LowCardinality(String),
    env         LowCardinality(String),
    version     LowCardinality(String),
    host        LowCardinality(String),
    runtime     LowCardinality(String),
    type        LowCardinality(String),
    unit        LowCardinality(String),
    total       Int64,
    stacks      UInt32
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, service, type, ts)
TTL toDateTime(ts) + INTERVAL 14 DAY;

-- ───────────────────────────── AI / LLM monitoring ─────────────────────────────
-- Prompt / completion content (only with captureContent), split off span attributes by the writer.
-- input / output are JSON strings (GenAI message arrays); each field ≤ 32 KB ('…[truncated]' suffix).
CREATE TABLE IF NOT EXISTS telemetry.llm_content
(
    org_id      UInt32,
    project_id  UInt32,
    ts          DateTime64(6),
    trace_id    FixedString(16),
    span_id     FixedString(8),
    service     LowCardinality(String),
    system      String CODEC(ZSTD(3)),
    input       String CODEC(ZSTD(3)),
    output      String CODEC(ZSTD(3))
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (org_id, project_id, trace_id, span_id)
TTL toDateTime(ts) + INTERVAL 7 DAY;

-- LLM calls per minute from spans (model-call spans with gen_ai.operation.name; the writer normalizes legacy OpenLLMetry
-- names into the attrs read here). Counts and tokens are multiplied by sample_weight (tail sampling).
CREATE TABLE IF NOT EXISTS telemetry.llm_1m
(
    org_id             UInt32,
    project_id         UInt32,
    service            LowCardinality(String),
    env                LowCardinality(String),
    provider           LowCardinality(String),
    model              LowCardinality(String),
    operation          LowCardinality(String),
    route              LowCardinality(String),       -- entry route of the request
    method             LowCardinality(String),
    minute             DateTime,
    calls              SimpleAggregateFunction(sum, UInt64),
    errors             SimpleAggregateFunction(sum, UInt64),
    input_tokens       SimpleAggregateFunction(sum, UInt64),
    output_tokens      SimpleAggregateFunction(sum, UInt64),
    cache_read_tokens  SimpleAggregateFunction(sum, UInt64),
    cache_write_tokens SimpleAggregateFunction(sum, UInt64),
    duration_sum       SimpleAggregateFunction(sum, Float64),   -- ms
    duration_hist      AggregateFunction(quantilesTDigestWeighted(0.5, 0.95, 0.99), Float64, UInt32),   -- ms
    ttft_hist          AggregateFunction(quantilesTDigestWeighted(0.5, 0.95, 0.99), Float64, UInt32)    -- ms, streaming only
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, service, env, provider, model, operation, route, method, minute)
TTL minute + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.llm_1m_mv TO telemetry.llm_1m AS
SELECT org_id, project_id, service, env,
       attrs['gen_ai.provider.name'] AS provider,
       if(attrs['gen_ai.response.model'] != '', attrs['gen_ai.response.model'], attrs['gen_ai.request.model']) AS model,
       attrs['gen_ai.operation.name'] AS operation,
       entry_route AS route, entry_method AS method,
       toStartOfMinute(ts) AS minute,
       sum(toUInt64(greatest(1, round(sample_weight)))) AS calls,
       sumIf(toUInt64(greatest(1, round(sample_weight))), is_error = 1) AS errors,
       sum(toUInt64(round(sample_weight * toUInt64OrZero(attrs['gen_ai.usage.input_tokens'])))) AS input_tokens,
       sum(toUInt64(round(sample_weight * toUInt64OrZero(attrs['gen_ai.usage.output_tokens'])))) AS output_tokens,
       sum(toUInt64(round(sample_weight * toUInt64OrZero(attrs['gen_ai.usage.cache_read.input_tokens'])))) AS cache_read_tokens,
       sum(toUInt64(round(sample_weight * toUInt64OrZero(attrs['gen_ai.usage.cache_creation.input_tokens'])))) AS cache_write_tokens,
       sum(sample_weight * duration_ns / 1e6) AS duration_sum,
       quantilesTDigestWeightedState(0.5, 0.95, 0.99)(duration_ns / 1e6, toUInt32(greatest(1, round(sample_weight)))) AS duration_hist,
       quantilesTDigestWeightedStateIf(0.5, 0.95, 0.99)(toFloat64OrZero(attrs['trooper.llm.ttft_ms']),
           toUInt32(greatest(1, round(sample_weight))), attrs['trooper.llm.ttft_ms'] != '') AS ttft_hist
FROM telemetry.spans
-- model calls only: tool / agent spans are not LLM calls (contracts §25)
WHERE attrs['gen_ai.operation.name'] NOT IN ('', 'execute_tool', 'invoke_agent', 'create_agent')
GROUP BY org_id, project_id, service, env, provider, model, operation, route, method, minute;

-- Hourly downsample (13 months).
CREATE TABLE IF NOT EXISTS telemetry.llm_1h AS telemetry.llm_1m
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, service, env, provider, model, operation, route, method, minute)
TTL minute + INTERVAL 13 MONTH;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.llm_1h_mv TO telemetry.llm_1h AS
SELECT org_id, project_id, service, env, provider, model, operation, route, method, toStartOfHour(minute) AS minute,
       sum(calls) AS calls, sum(errors) AS errors,
       sum(input_tokens) AS input_tokens, sum(output_tokens) AS output_tokens,
       sum(cache_read_tokens) AS cache_read_tokens, sum(cache_write_tokens) AS cache_write_tokens,
       sum(duration_sum) AS duration_sum,
       quantilesTDigestWeightedMergeState(0.5, 0.95, 0.99)(duration_hist) AS duration_hist,
       quantilesTDigestWeightedMergeState(0.5, 0.95, 0.99)(ttft_hist) AS ttft_hist
FROM telemetry.llm_1m
GROUP BY org_id, project_id, service, env, provider, model, operation, route, method, minute;

-- LLM spans are often kind=internal (OTel GenAI instrumentations, SDK helpers); edges_1m_mv only reads client/producer
-- spans, so this second view feeds the same table with internal 'ai' deps ("Time spent" and RCA see them too).
CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.edges_1m_ai_mv TO telemetry.edges_1m AS
SELECT org_id, project_id, env, service AS parent_service, entry_route AS parent_route, entry_method AS parent_method,
       dep, dep_kind, dep_service AS child_service,
       toStartOfMinute(ts) AS minute,
       sum(sample_weight) AS calls,
       sumIf(sample_weight, is_error = 1) AS errors,
       sum(sample_weight * duration_ns / 1e6) AS duration_sum,
       quantilesTDigestWeightedState(0.5, 0.95)(duration_ns / 1e6, toUInt32(greatest(1, round(sample_weight)))) AS duration
FROM telemetry.spans
WHERE kind NOT IN ('client', 'producer') AND dep_kind = 'ai' AND dep != ''
GROUP BY org_id, project_id, env, parent_service, parent_route, parent_method, dep, dep_kind, child_service, minute;
