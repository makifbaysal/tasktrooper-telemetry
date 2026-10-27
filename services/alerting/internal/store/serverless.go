package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// ServerlessFunctions returns the project's serverless_functions rows (deploy/postgres/011_serverless.sql), optionally
// filtered by service and slug; missing functions are skipped. Before 011 is applied the result is empty.
func (st *Store) ServerlessFunctions(ctx context.Context, projectID int64, service, slug string) ([]model.Function, error) {
	rows, err := st.DB.Query(ctx, `SELECT slug, service, provider, region, coalesce(timeout_s, 0)::float8 FROM serverless_functions
		WHERE project_id = $1 AND NOT missing AND ($2 = '' OR service = $2) AND ($3 = '' OR slug = $3) ORDER BY slug`, projectID, service, slug)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P01" { // undefined_table
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []model.Function
	for rows.Next() {
		var f model.Function
		if err := rows.Scan(&f.Slug, &f.Service, &f.Provider, &f.Region, &f.TimeoutS); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
