-- Messaging system on spans/consumers, queue age, topic metadata. Idempotent (ALTER ... IF NOT EXISTS; MV recreated).

ALTER TABLE telemetry.spans ADD COLUMN IF NOT EXISTS messaging_system LowCardinality(String) AFTER topic;

ALTER TABLE telemetry.consumer_1m ADD COLUMN IF NOT EXISTS messaging_system LowCardinality(String) AFTER consumer_group;

DROP VIEW IF EXISTS telemetry.consumer_1m_mv;

CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry.consumer_1m_mv TO telemetry.consumer_1m AS
SELECT org_id, project_id, service, env, route AS process, topic, consumer_group, messaging_system,
       toStartOfMinute(ts) AS minute,
       sum(sample_weight) AS processed,
       sumIf(sample_weight, is_error = 1) AS failed,
       sumIf(sample_weight, attempt > 1) AS retried,
       sumIf(sample_weight, dlq = 1) AS dlq,
       sum(sample_weight * duration_ns / 1e6) AS duration_sum,
       quantilesTDigestWeightedState(0.5, 0.95, 0.99)(duration_ns / 1e6, toUInt32(greatest(1, round(sample_weight)))) AS duration
FROM telemetry.spans
WHERE is_entry = 1 AND kind = 'consumer'
GROUP BY org_id, project_id, service, env, process, topic, consumer_group, messaging_system, minute;

-- SQS / Pub/Sub / RabbitMQ: age of the oldest waiting message (seconds).
ALTER TABLE telemetry.broker_lag_1m ADD COLUMN IF NOT EXISTS oldest_age_s SimpleAggregateFunction(max, Float64);

-- Topic / queue metadata and throughput (collector + cloud integrations).
CREATE TABLE IF NOT EXISTS telemetry.broker_topics_1m
(
    org_id         UInt32,
    project_id     UInt32,
    cluster_id     LowCardinality(String),
    topic          LowCardinality(String),
    minute         DateTime,
    partitions     SimpleAggregateFunction(max, Float64),
    replication    SimpleAggregateFunction(max, Float64),
    retention_ms   SimpleAggregateFunction(max, Float64),
    avg_msg_bytes  SimpleAggregateFunction(max, Float64),
    in_rate        SimpleAggregateFunction(max, Float64),      -- msgs/s produced
    bytes_in_rate  SimpleAggregateFunction(max, Float64),      -- bytes/s
    size_bytes     SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(minute)
ORDER BY (org_id, project_id, cluster_id, topic, minute)
TTL minute + INTERVAL 30 DAY;
