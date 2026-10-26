package trooperpgx

import (
	"context"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	trooper "github.com/tasktrooper/telemetry-go"
)

// Tracer is an otelpgx tracer that sanitizes SQL text. It implements pgx.QueryTracer,
// pgx.BatchTracer, pgx.CopyFromTracer, pgx.PrepareTracer, pgx.ConnectTracer and
// pgxpool.AcquireTracer.
type Tracer struct {
	*otelpgx.Tracer
}

var (
	_ pgx.QueryTracer    = (*Tracer)(nil)
	_ pgx.BatchTracer    = (*Tracer)(nil)
	_ pgx.PrepareTracer  = (*Tracer)(nil)
	_ pgx.CopyFromTracer = (*Tracer)(nil)
	_ pgx.ConnectTracer  = (*Tracer)(nil)
)

// NewTracer returns a sanitizing tracer. Extra otelpgx options are applied after the defaults;
// do not pass otelpgx.WithIncludeQueryParameters unless parameters are known to be free of PII.
func NewTracer(opts ...otelpgx.Option) *Tracer {
	base := []otelpgx.Option{
		otelpgx.WithTracerAttributes(attribute.String("db.system", "postgresql")),
	}
	return &Tracer{Tracer: otelpgx.NewTracer(append(base, opts...)...)}
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *Tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	data.SQL = trooper.SanitizeSQL(data.SQL)
	data.Args = nil
	return t.Tracer.TraceQueryStart(ctx, conn, data)
}

// TraceBatchQuery implements pgx.BatchTracer.
func (t *Tracer) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	data.SQL = trooper.SanitizeSQL(data.SQL)
	data.Args = nil
	t.Tracer.TraceBatchQuery(ctx, conn, data)
}

// TracePrepareStart implements pgx.PrepareTracer.
func (t *Tracer) TracePrepareStart(ctx context.Context, conn *pgx.Conn, data pgx.TracePrepareStartData) context.Context {
	data.SQL = trooper.SanitizeSQL(data.SQL)
	return t.Tracer.TracePrepareStart(ctx, conn, data)
}

// RecordStats records pgxpool statistics as metrics (see otelpgx.RecordStats).
func RecordStats(db otelpgx.PoolStats, opts ...otelpgx.StatsOption) error {
	return otelpgx.RecordStats(db, opts...)
}
