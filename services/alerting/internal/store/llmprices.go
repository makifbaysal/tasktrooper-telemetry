package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tasktrooper/telemetry/services/pkg/llmprice"
)

// LLMPrices returns the org's llm_prices override rows (deploy/postgres/012_llm_prices.sql) for llm monitors' cost
// metrics; before 012 is applied the result is empty (defaults only). Same columns the api reads (contracts §25).
func (st *Store) LLMPrices(ctx context.Context, orgID int64) ([]llmprice.Price, error) {
	rows, err := st.DB.Query(ctx, `SELECT provider, model, input_per_mtok::float8, output_per_mtok::float8,
		cache_read_per_mtok::float8, cache_write_per_mtok::float8 FROM llm_prices WHERE org_id = $1 ORDER BY provider, model`, orgID)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P01" { // undefined_table
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []llmprice.Price
	for rows.Next() {
		var p llmprice.Price
		if err := rows.Scan(&p.Provider, &p.Model, &p.InputPerMTok, &p.OutputPerMTok, &p.CacheReadPerMTok, &p.CacheWritePerMTok); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
