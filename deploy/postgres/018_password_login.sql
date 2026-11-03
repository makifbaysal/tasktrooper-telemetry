-- Email + password login (ADR-035, contracts §15). Owner: services/api. Additive and idempotent.
-- password_hash is a bcrypt hash, set only by `api set-password` (no self-service sign-up or reset); NULL = the user
-- signs in with OIDC only. POST /auth/password-login exists only when the api runs with TT_AUTH_PASSWORD=true.
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash       text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_updated_at timestamptz;
