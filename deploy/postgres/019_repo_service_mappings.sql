-- Folder → service mapping of connected repositories (ADR-036, .ai/code-intelligence.md § Folder → service mapping,
-- contracts §13). Owner: services/codeintel writes the detected folders and automatic matches; services/api writes the
-- user's decisions (confirmed / ignored), which codeintel never changes. Additive and idempotent.
CREATE TABLE IF NOT EXISTS repo_service_mappings (
    repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    project_id    bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    path          text NOT NULL,                          -- folder, slash separated; '.' = repository root
    detected_name text NOT NULL,                          -- name the code gives it
    evidence      text NOT NULL DEFAULT 'module' CHECK (evidence IN ('config', 'sdk', 'module', 'repo')),
    kind          text NOT NULL DEFAULT 'library' CHECK (kind IN ('backend', 'web', 'mobile', 'worker', 'library')),
    items         integer NOT NULL DEFAULT 0,             -- inventory items (dependencies excluded)
    service       text,                                   -- linked service; NULL while unmatched / ignored
    status        text NOT NULL CHECK (status IN ('matched', 'unmatched', 'confirmed', 'ignored')),
    decided_by    bigint REFERENCES users(id) ON DELETE SET NULL,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, path)
);
CREATE INDEX IF NOT EXISTS repo_service_mappings_project ON repo_service_mappings (project_id);
