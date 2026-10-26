package troopersql_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	troopersql "github.com/tasktrooper/telemetry-go/integrations/sql"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

// fake driver ---------------------------------------------------------------

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }
func (fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &fakeRows{}, nil
}
func (fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct{ done bool }

func (*fakeRows) Columns() []string { return []string{"n"} }
func (*fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

func init() { sql.Register("fakepostgres", fakeDriver{}) }

func TestSystemFromDriver(t *testing.T) {
	for in, want := range map[string]string{
		"pgx": "postgresql", "postgres": "postgresql", "mysql": "mysql", "sqlite3": "sqlite",
		"sqlite": "sqlite", "sqlserver": "mssql", "mssql": "mssql", "clickhouse": "clickhouse", "foo": "other_sql",
	} {
		if got := troopersql.SystemFromDriver(in); got != want {
			t.Errorf("SystemFromDriver(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuerySpans(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		args      []any
		wantQuery string
		secrets   []string
		exec      bool
	}{
		{"select literal", "SELECT * FROM users WHERE email = 'bob@example.com' AND age > 42", nil,
			"SELECT * FROM users WHERE email = ? AND age > ?", []string{"bob@example.com", "42"}, false},
		{"placeholders and args", "SELECT id FROM orders WHERE id = $1", []any{"secret-arg-123"},
			"SELECT id FROM orders WHERE id = $1", []string{"secret-arg-123"}, false},
		{"insert exec", "INSERT INTO t (a, b) VALUES ('x''y', 3.14)", nil,
			"INSERT INTO t (a, b) VALUES (?, ?)", []string{"x''y", "3.14"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			db, err := troopersql.Open("fakepostgres", "postgres://u:p@db.internal:5432/orders", troopersql.WithNamespace("orders"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			ctx, srv := otel.Tracer("x").Start(context.Background(), "srv", trace.WithSpanKind(trace.SpanKindServer))
			ctx, _ = trooper.BeginEntry(ctx, "GET", "/orders/:id", nil)
			if tt.exec {
				_, err = db.ExecContext(ctx, tt.query, tt.args...)
			} else {
				var rows *sql.Rows
				rows, err = db.QueryContext(ctx, tt.query, tt.args...)
				if err == nil {
					for rows.Next() {
					}
					rows.Close()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			srv.End()

			var found bool
			for _, s := range h.Spans() {
				if s.SpanKind() != trace.SpanKindClient {
					continue
				}
				if troopertest.AttrString(s, "db.query.text") == "" {
					continue
				}
				found = true
				checks := map[string]string{
					"db.system":            "postgresql",
					"db.namespace":         "orders",
					"db.query.text":        tt.wantQuery,
					"trooper.entry.route":  "/orders/:id",
					"trooper.entry.method": "GET",
				}
				for k, want := range checks {
					if got := troopertest.AttrString(s, attrKey(k)); got != want {
						t.Errorf("%s: %s = %q, want %q", s.Name(), k, got, want)
					}
				}
				for _, kv := range s.Attributes() {
					for _, sec := range tt.secrets {
						if strings.Contains(kv.Value.Emit(), sec) {
							t.Errorf("attribute %s leaks %q", kv.Key, sec)
						}
					}
				}
			}
			if !found {
				t.Fatalf("no db client span with db.query.text; spans: %d", len(h.Spans()))
			}
		})
	}
}
