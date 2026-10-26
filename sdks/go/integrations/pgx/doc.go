// Package trooperpgx instruments jackc/pgx v5 via github.com/exaring/otelpgx with TaskTrooper
// defaults: SQL text is sanitized with trooper.SanitizeSQL before it reaches span attributes,
// query parameters are never recorded, and db.system=postgresql is set.
//
//	cfg, _ := pgxpool.ParseConfig(dsn)
//	cfg.ConnConfig.Tracer = trooperpgx.NewTracer()
//	pool, _ := pgxpool.NewWithConfig(ctx, cfg)
//	_ = trooperpgx.RecordStats(pool) // optional pool metrics
package trooperpgx
