// Package conn opens the Postgres pool and ClickHouse connection from TT_* settings.
package conn

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres opens and pings a pool.
func Postgres(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("postgres url: %w", err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return pool, nil
}

// ClickHouse opens and pings a native connection (clickhouse://user:pass@host:9000/db).
func ClickHouse(ctx context.Context, url string) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(url)
	if err != nil {
		return nil, fmt.Errorf("clickhouse url: %w", err)
	}
	opts.MaxOpenConns = 16
	opts.DialTimeout = 5 * time.Second
	opts.Settings = clickhouse.Settings{"max_execution_time": 30}
	c, err := clickhouse.Open(opts)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Ping(pctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	return c, nil
}
