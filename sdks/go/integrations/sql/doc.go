// Package troopersql instruments database/sql via github.com/XSAM/otelsql with TaskTrooper
// defaults: db.system / db.namespace / server.address attributes, and db.query.text sanitized
// with trooper.SanitizeSQL (literals replaced by '?'; arguments are never recorded).
//
//	db, err := troopersql.Open("pgx", dsn, troopersql.WithNamespace("orders"))
//
// Spans started under a request handled by a trooper integration carry trooper.entry.route.
package troopersql
