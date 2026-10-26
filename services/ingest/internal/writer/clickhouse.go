package writer

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Inserter writes one batch of rows into a table. Implementations must be all-or-nothing per call
// from the writer's point of view: on error the whole batch is retried.
type Inserter interface {
	Insert(ctx context.Context, table, columns string, n int, row func(i int) []any) error
}

// ClickHouse inserts with native-protocol batches.
type ClickHouse struct{ Conn driver.Conn }

// OpenClickHouse connects using a clickhouse:// DSN.
func OpenClickHouse(ctx context.Context, dsn string) (*ClickHouse, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
	if opts.MaxOpenConns == 0 {
		opts.MaxOpenConns = 16
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &ClickHouse{Conn: conn}, nil
}

// Insert implements Inserter.
func (c *ClickHouse) Insert(ctx context.Context, table, columns string, n int, row func(i int) []any) error {
	b, err := c.Conn.PrepareBatch(ctx, "INSERT INTO "+table+" ("+columns+")")
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if err := b.Append(row(i)...); err != nil {
			_ = b.Abort()
			return err
		}
	}
	return b.Send()
}

// Close closes the connection pool.
func (c *ClickHouse) Close() error { return c.Conn.Close() }
