-- trooper-agent db mode monitors ClickHouse too (collector engine `clickhouse`, contracts §5).
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_engine_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_check CHECK (engine IN ('postgres', 'mysql', 'redis', 'mongodb', 'clickhouse'));
