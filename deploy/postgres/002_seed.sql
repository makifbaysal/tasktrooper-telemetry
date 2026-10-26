-- Dev seed: org 1, project 1, keys tt_dev_ingest_key (ingest) and tt_dev_api_key (api). Never run in production.
INSERT INTO orgs (id, name, plan) VALUES (1, 'Acme', 'team') ON CONFLICT (id) DO NOTHING;
INSERT INTO users (id, email, name) VALUES (1, 'dev@tasktrooper.local', 'Dev User') ON CONFLICT (id) DO NOTHING;
INSERT INTO memberships (org_id, user_id, role) VALUES (1, 1, 'owner') ON CONFLICT DO NOTHING;
INSERT INTO projects (id, org_id, name, slug) VALUES (1, 1, 'Shop', 'shop') ON CONFLICT (id) DO NOTHING;

INSERT INTO api_keys (project_id, name, kind, hash, prefix)
VALUES (1, 'dev ingest', 'ingest', digest('tt_dev_ingest_key', 'sha256'), 'tt_dev_i'),
       (1, 'dev api', 'api', digest('tt_dev_api_key', 'sha256'), 'tt_dev_a')
ON CONFLICT (hash) DO NOTHING;

INSERT INTO notification_channels (project_id, name, type, config)
VALUES (1, '#platform', 'webhook', '{"url": "http://localhost:9999/hook"}')
ON CONFLICT DO NOTHING;

SELECT setval(pg_get_serial_sequence('orgs', 'id'), GREATEST((SELECT max(id) FROM orgs), 1));
SELECT setval(pg_get_serial_sequence('users', 'id'), GREATEST((SELECT max(id) FROM users), 1));
SELECT setval(pg_get_serial_sequence('projects', 'id'), GREATEST((SELECT max(id) FROM projects), 1));
