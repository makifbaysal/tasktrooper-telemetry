package selftel

import (
	"context"
	"regexp"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// ClickHouse wraps conn so Select/Query/QueryRow/Exec/PrepareBatch produce db.system=clickhouse client spans named
// "<OPERATION> <table>" with sanitized SQL (trooper.SanitizeSQL, so the inlined tenant ids become ?) and the request
// tenant as tt.org_id / tt.project_id. Query spans end when the rows are closed. database and addr name the
// dependency (db.namespace, server.address). Ping is not traced (health checks).
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

var tableRe = regexp.MustCompile(`(?i)\b(?:FROM|INTO|JOIN|TABLE)\s+([A-Za-z_][A-Za-z0-9_.]*)`)

// Operation returns the first SQL keyword (SELECT, INSERT, …); a leading WITH counts as SELECT.
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

// Table returns the first table named after FROM / INTO / JOIN / TABLE ("" for a subquery-only statement).
func Table(query string) string {
	m := tableRe.FindStringSubmatch(query)
	if m == nil {
		return ""
	}
	t := m[1]
	if i := strings.LastIndexByte(t, '.'); i >= 0 { // telemetry.spans → spans
		t = t[i+1:]
	}
	return t
}

func (c *chConn) start(ctx context.Context, query string) (context.Context, trace.Span) {
	op, table := Operation(query), Table(query)
	name := strings.TrimSpace(op + " " + table)
	if name == "" {
		name = "clickhouse"
	}
	attrs := []attribute.KeyValue{
		attribute.String("db.system", "clickhouse"),
		attribute.String("db.system.name", "clickhouse"),
		attribute.String("db.query.text", trooper.SanitizeSQL(query)),
	}
	if op != "" {
		attrs = append(attrs, attribute.String("db.operation.name", op))
	}
	if table != "" {
		attrs = append(attrs, attribute.String("db.collection.name", table))
	}
	if c.db != "" {
		attrs = append(attrs, attribute.String("db.namespace", c.db))
	}
	if c.addr != "" {
		attrs = append(attrs, attribute.String("server.address", c.addr))
	}
	if t, ok := tenant.From(ctx); ok {
		attrs = append(attrs, tenantAttrs(t)...)
	}
	return otel.Tracer(scope).Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
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

// chBatch ends the span on Send (or Abort / Close).
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
