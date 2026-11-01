-- Monitors, KPIs and load test scenarios created by a coding agent through the MCP server (ADR-032).
-- Additive and idempotent; runs after 005, which re-creates the monitors constraint on every migrate, so the full
-- lists are repeated here.
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_source_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_source_check CHECK (source IN ('manual', 'code', 'template', 'suggestion', 'suggested', 'agent'));
ALTER TABLE kpis DROP CONSTRAINT IF EXISTS kpis_source_check;
ALTER TABLE kpis ADD CONSTRAINT kpis_source_check CHECK (source IN ('code', 'template', 'manual', 'agent'));
ALTER TABLE loadtest_scenarios DROP CONSTRAINT IF EXISTS loadtest_scenarios_source_check;
ALTER TABLE loadtest_scenarios ADD CONSTRAINT loadtest_scenarios_source_check CHECK (source IN ('manual', 'generated', 'agent'));
