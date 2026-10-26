package trooperpgx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperpgx "github.com/tasktrooper/telemetry-go/integrations/pgx"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestTracerSanitizes(t *testing.T) {
	tests := []struct {
		name    string
		run     func(ctx context.Context, tr *trooperpgx.Tracer)
		want    string
		secrets []string
	}{
		{
			name: "query",
			run: func(ctx context.Context, tr *trooperpgx.Tracer) {
				ctx = tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
					SQL: "SELECT * FROM users WHERE email = 'alice@example.com' AND id = $1", Args: []any{"arg-secret"}})
				tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
			},
			want:    "SELECT * FROM users WHERE email = ? AND id = $1",
			secrets: []string{"alice@example.com", "arg-secret"},
		},
		{
			name: "prepare",
			run: func(ctx context.Context, tr *trooperpgx.Tracer) {
				ctx = tr.TracePrepareStart(ctx, nil, pgx.TracePrepareStartData{Name: "s1", SQL: "UPDATE t SET v = 99 WHERE k = 'key-secret'"})
				tr.TracePrepareEnd(ctx, nil, pgx.TracePrepareEndData{})
			},
			want:    "UPDATE t SET v = ? WHERE k = ?",
			secrets: []string{"key-secret", "99"},
		},
		{
			name: "query error",
			run: func(ctx context.Context, tr *trooperpgx.Tracer) {
				ctx = tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "DELETE FROM t WHERE id = 7"})
				tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("boom")})
			},
			want:    "DELETE FROM t WHERE id = ?",
			secrets: []string{"= 7"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			tr := trooperpgx.NewTracer()
			ctx, srv := otel.Tracer("x").Start(context.Background(), "srv", trace.WithSpanKind(trace.SpanKindServer))
			ctx, _ = trooper.BeginEntry(ctx, "GET", "/orders/:id", nil)
			tt.run(ctx, tr)
			srv.End()

			s := h.SpanByKind(trace.SpanKindClient)
			if s == nil {
				t.Fatal("no client span")
			}
			for k, want := range map[string]string{
				"db.system":           "postgresql",
				"db.query.text":       tt.want,
				"trooper.entry.route": "/orders/:id",
			} {
				if got := troopertest.AttrString(s, attrKey(k)); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			for _, kv := range s.Attributes() {
				for _, sec := range tt.secrets {
					if strings.Contains(kv.Value.Emit(), sec) {
						t.Errorf("attribute %s leaks %q", kv.Key, sec)
					}
				}
			}
			if strings.Contains(s.Name(), "secret") {
				t.Errorf("span name leaks: %s", s.Name())
			}
		})
	}
}
