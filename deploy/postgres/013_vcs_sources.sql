-- GitLab / Bitbucket repository sources (ADR-014 addendum, contracts §18a). Owner: services/codeintel.
-- Additive and idempotent.

-- Self-managed GitLab address (https://gitlab.example.com[/path]); NULL = gitlab.com. Only GitLab repositories carry one
-- (Bitbucket Cloud and GitHub hosts are fixed / come from TT_GITHUB_API_URL).
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS base_url text;
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_base_url_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_base_url_check
    CHECK (base_url IS NULL OR (provider = 'gitlab' AND base_url LIKE 'https://%'));

-- Last verified webhook delivery for this repository (any provider; GitHub App repos get theirs through the App).
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS webhook_received_at timestamptz;

-- Webhook lookups: provider + case-insensitive full name.
CREATE INDEX IF NOT EXISTS repositories_provider_name_idx ON repositories (provider, lower(full_name));
