-- Code intelligence (ADR-014, contracts §13). Owner: services/codeintel. Additive and idempotent.

-- Repositories: credentials, local provider, analysis state.
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS installation_id      bigint;       -- GitHub App installation
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS credentials_ref      text;         -- secret:<id> | env:NAME | file:/path (token)
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS local_path           text;         -- provider 'local' (dev/demo)
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS analyze_requested_at timestamptz;
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS analyzed_at          timestamptz;
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS analysis_started_at  timestamptz;  -- codeintel claim (stale claims are retaken)
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS error                text;
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS languages            text[] NOT NULL DEFAULT '{}';
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS services             text[] NOT NULL DEFAULT '{}';
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS webhook_secret_ref   text;
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS created_at           timestamptz NOT NULL DEFAULT now();
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_provider_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_provider_check CHECK (provider IN ('github', 'gitlab', 'bitbucket', 'local'));
UPDATE repositories SET status = 'pending' WHERE status NOT IN ('pending', 'analyzing', 'ready', 'error', 'disconnected');
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_status_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_status_check CHECK (status IN ('pending', 'analyzing', 'ready', 'error', 'disconnected'));

-- Inventory: shadow items (traffic without code) have no repository, so rows carry project_id themselves.
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS project_id  bigint REFERENCES projects(id) ON DELETE CASCADE;
UPDATE inventory_items i SET project_id = r.project_id FROM repositories r WHERE i.project_id IS NULL AND r.id = i.repository_id;
ALTER TABLE inventory_items ALTER COLUMN repository_id DROP NOT NULL;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS service     text;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS criticality text NOT NULL DEFAULT 'low';
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS observed    boolean NOT NULL DEFAULT false;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS monitored   boolean NOT NULL DEFAULT false;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS code_hash   text;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS summary     text;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS dead        boolean NOT NULL DEFAULT false;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS shadow      boolean NOT NULL DEFAULT false;
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS llm         jsonb;                 -- {criticality, businessDomain, failureModes[], suggestedMonitors[], loadtestHint}
ALTER TABLE inventory_items ADD COLUMN IF NOT EXISTS updated_at  timestamptz NOT NULL DEFAULT now();
ALTER TABLE inventory_items DROP CONSTRAINT IF EXISTS inventory_items_criticality_check;
ALTER TABLE inventory_items ADD CONSTRAINT inventory_items_criticality_check CHECK (criticality IN ('critical', 'high', 'medium', 'low'));
CREATE INDEX IF NOT EXISTS inventory_items_project ON inventory_items (project_id, kind);
CREATE INDEX IF NOT EXISTS inventory_items_repo ON inventory_items (repository_id);

-- Alert suggestions: display fields + dedupe (one suggestion per inventory item and monitor kind, any state).
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS title       text NOT NULL DEFAULT '';
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS kind        text NOT NULL DEFAULT 'metric_threshold';
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS target      text NOT NULL DEFAULT '';
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS condition   text NOT NULL DEFAULT '';
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS criticality text NOT NULL DEFAULT 'medium';
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS code_ref    text;
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS calibrated  boolean NOT NULL DEFAULT false;
ALTER TABLE alert_suggestions ADD COLUMN IF NOT EXISTS created_at  timestamptz NOT NULL DEFAULT now();
CREATE UNIQUE INDEX IF NOT EXISTS alert_suggestions_item_kind ON alert_suggestions (inventory_item_id, kind) WHERE inventory_item_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS alert_suggestions_project ON alert_suggestions (project_id, state);

-- Monitors: alert-as-code. 'suggested' is kept for rows written before 005 (api reads it as 'code').
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS code_ref text;                       -- '<repo full name>:<path>' for source='code'
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_source_check;
ALTER TABLE monitors ADD CONSTRAINT monitors_source_check CHECK (source IN ('manual', 'code', 'template', 'suggestion', 'suggested'));
CREATE INDEX IF NOT EXISTS monitors_code_ref ON monitors (project_id, code_ref) WHERE code_ref IS NOT NULL;

-- Load test scenarios: generated drafts never run without approval.
ALTER TABLE loadtest_scenarios ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'ready';
ALTER TABLE loadtest_scenarios DROP CONSTRAINT IF EXISTS loadtest_scenarios_state_check;
ALTER TABLE loadtest_scenarios ADD CONSTRAINT loadtest_scenarios_state_check CHECK (state IN ('draft', 'ready'));

-- GitHub webhook idempotency (X-GitHub-Delivery); codeintel prunes rows older than 7 days.
CREATE TABLE IF NOT EXISTS codeintel_webhook_deliveries (
    id          text PRIMARY KEY,
    event       text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

-- Columns created earlier without defaults (dev databases): enforce defaults + NOT NULL so reads need no coalesce.
DO $$
DECLARE c record;
BEGIN
  FOR c IN SELECT * FROM (VALUES
      ('repositories', 'languages', '''{}''::text[]'), ('repositories', 'services', '''{}''::text[]'),
      ('inventory_items', 'criticality', '''low'''), ('inventory_items', 'observed', 'false'), ('inventory_items', 'monitored', 'false'),
      ('inventory_items', 'dead', 'false'), ('inventory_items', 'shadow', 'false'),
      ('alert_suggestions', 'title', ''''''), ('alert_suggestions', 'kind', '''metric_threshold'''), ('alert_suggestions', 'target', ''''''),
      ('alert_suggestions', 'condition', ''''''), ('alert_suggestions', 'criticality', '''medium'''), ('alert_suggestions', 'calibrated', 'false')
    ) AS v(tbl, col, def)
  LOOP
    EXECUTE format('ALTER TABLE %I ALTER COLUMN %I SET DEFAULT %s', c.tbl, c.col, c.def);
    EXECUTE format('UPDATE %I SET %I = %s WHERE %I IS NULL', c.tbl, c.col, c.def, c.col);
    EXECUTE format('ALTER TABLE %I ALTER COLUMN %I SET NOT NULL', c.tbl, c.col);
  END LOOP;
END $$;
