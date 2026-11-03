-- Top / slow queries for trooper-agent db mode (ADR-027, collector README). The library is preloaded by the compose
-- postgres command; where it is not, the extension still installs and the agent just reports no query stats.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
