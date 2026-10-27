-- AI / LLM monitoring (ADR-023, contracts §25): org overrides of the LLM price table. Owner: services/api
-- (defaults: services/api/internal/llmprice/defaults.json). Additive and idempotent.

-- Prices are USD per 1M tokens. provider = gen_ai.provider.name or a price family (openai, anthropic, google …);
-- model = exact model or a prefix (lower case; longest prefix wins). NULL cache prices = input price.
CREATE TABLE IF NOT EXISTS llm_prices (
    org_id               bigint NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider             text NOT NULL,
    model                text NOT NULL,
    input_per_mtok       numeric(14, 6) NOT NULL CHECK (input_per_mtok >= 0),
    output_per_mtok      numeric(14, 6) NOT NULL CHECK (output_per_mtok >= 0),
    cache_read_per_mtok  numeric(14, 6) CHECK (cache_read_per_mtok >= 0),
    cache_write_per_mtok numeric(14, 6) CHECK (cache_write_per_mtok >= 0),
    updated_by           bigint REFERENCES users(id) ON DELETE SET NULL,   -- NULL = api key
    updated_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, provider, model)
);
