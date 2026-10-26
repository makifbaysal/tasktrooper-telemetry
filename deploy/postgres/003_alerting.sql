-- Alerting state (from services/alerting). Additive, idempotent.
--    re-derives "firing" from the open incident after a restart.
ALTER TABLE kpi_alert_rules ADD COLUMN IF NOT EXISTS state        text NOT NULL DEFAULT 'ok' CHECK (state IN ('ok', 'pending', 'firing', 'no_data'));
ALTER TABLE kpi_alert_rules ADD COLUMN IF NOT EXISTS state_since  timestamptz NOT NULL DEFAULT now();
ALTER TABLE kpi_alert_rules ADD COLUMN IF NOT EXISTS last_value   double precision;
ALTER TABLE kpi_alert_rules ADD COLUMN IF NOT EXISTS last_eval_at timestamptz;

-- 2. One open incident per KPI rule, enforced like incidents_one_open (today: matched by kpi_id + deterministic title).
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS kpi_rule_id bigint REFERENCES kpi_alert_rules(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX IF NOT EXISTS incidents_one_open_kpi_rule ON incidents (kpi_rule_id) WHERE state <> 'resolved' AND kpi_rule_id IS NOT NULL;

-- 3. Edits must bump monitors.updated_at (the engine's cache fingerprint uses max(updated_at)).
CREATE OR REPLACE FUNCTION monitors_touch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.name, NEW.kind, NEW.target, NEW.query, NEW.condition, NEW.severity, NEW.channels, NEW.status)
       IS DISTINCT FROM (OLD.name, OLD.kind, OLD.target, OLD.query, OLD.condition, OLD.severity, OLD.channels, OLD.status) THEN
        NEW.updated_at := now();
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS monitors_touch ON monitors;
CREATE TRIGGER monitors_touch BEFORE UPDATE ON monitors FOR EACH ROW EXECUTE FUNCTION monitors_touch();
