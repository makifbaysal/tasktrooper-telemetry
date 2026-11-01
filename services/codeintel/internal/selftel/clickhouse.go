package selftel

import (
	"context"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
)

// ClickHouse wraps conn so Select/Query/QueryRow/Exec/PrepareBatch produce db.system=clickhouse client spans with
// sanitized SQL (trooper.SanitizeSQL). Query spans end when the rows are closed. database and addr name the
// dependency (db.namespace, server.address).
func ClickHouse(conn driver.Conn, database, addr string) driver.Conn {
	if conn == nil {
		return nil
	}
	return &chConn{Conn: conn, db: database, addr: addr}
}

type chConn struct {
	driver.Conn
	db, addr string
}

// Operation returns the first SQL keyword (SELECT, INSERT, …), skipping a leading WITH clause name only.
func Operation(query string) string {
	q := strings.TrimSpace(query)
	for strings.HasPrefix(q, "--") || strings.HasPrefix(q, "/*") {
		if strings.HasPrefix(q, "--") {
			i := strings.IndexByte(q, '\n')
			if i < 0 {
				return ""
			}
			q = strings.TrimSpace(q[i+1:])
			continue
		}
		i := strings.Index(q, "*/")
		if i < 0 {
			return ""
		}
		q = strings.TrimSpace(q[i+2:])
	}
	end := strings.IndexFunc(q, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') })
	if end < 0 {
		end = len(q)
	}
	op := strings.ToUpper(q[:end])
	if op == "WITH" {
		return "SELECT"
	}
	return op
}

// noSpan is a non-recording span that is safe to End.
var noSpan = trace.SpanFromContext(context.Background())

func (c *chConn) start(ctx context.Context, query string) (context.Context, trace.Span) {
	// like otelpgx: no orphan client spans (background polls without a root span)
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, noSpan
	}
	op := Operation(query)
	name := "clickhouse"
	if op != "" {
		name = op + " " + c.db
	}
	attrs := []attribute.KeyValue{
		attribute.String("db.system", "clickhouse"),
		attribute.String("db.system.name", "clickhouse"),
		attribute.String("db.query.text", trooper.SanitizeSQL(query)),
	}
	if op != "" {
		attrs = append(attrs, attribute.String("db.operation.name", op))
	}
	if c.db != "" {
		attrs = append(attrs, attribute.String("db.namespace", c.db))
	}
	if c.addr != "" {
		attrs = append(attrs, attribute.String("server.address", c.addr))
	}
	return tracer().Start(ctx, strings.TrimSpace(name), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
}

func (c *chConn) Select(ctx context.Context, dest any, query string, args ...any) error {
	ctx, span := c.start(ctx, query)
	err := c.Conn.Select(ctx, dest, query, args...)
	End(span, err)
	return err
}

func (c *chConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	ctx, span := c.start(ctx, query)
	rows, err := c.Conn.Query(ctx, query, args...)
	if err != nil {
		End(span, err)
		return rows, err
	}
	return &chRows{Rows: rows, span: span}, nil
}

func (c *chConn) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	ctx, span := c.start(ctx, query)
	row := c.Conn.QueryRow(ctx, query, args...)
	if row != nil {
		End(span, row.Err())
	} else {
		span.End()
	}
	return row
}

func (c *chConn) Exec(ctx context.Context, query string, args ...any) error {
	ctx, span := c.start(ctx, query)
	err := c.Conn.Exec(ctx, query, args...)
	End(span, err)
	return err
}

func (c *chConn) PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	ctx, span := c.start(ctx, query)
	b, err := c.Conn.PrepareBatch(ctx, query, opts...)
	if err != nil {
		End(span, err)
		return b, err
	}
	return &chBatch{Batch: b, span: span}, nil
}

type chRows struct {
	driver.Rows
	span trace.Span
	done bool
}

func (r *chRows) Close() error {
	err := r.Rows.Close()
	if !r.done {
		r.done = true
		e := err
		if e == nil {
			e = r.Rows.Err()
		}
		End(r.span, e)
	}
	return err
}

// chBatch ends the span on Send (or Abort).
type chBatch struct {
	driver.Batch
	span trace.Span
	done bool
}

func (b *chBatch) finish(err error) {
	if !b.done {
		b.done = true
		End(b.span, err)
	}
}

func (b *chBatch) Send() error {
	err := b.Batch.Send()
	b.finish(err)
	return err
}

func (b *chBatch) Abort() error {
	err := b.Batch.Abort()
	b.finish(err)
	return err
}

func (b *chBatch) Close() error {
	err := b.Batch.Close()
	b.finish(nil)
	return err
}
