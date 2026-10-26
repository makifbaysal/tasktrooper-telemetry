// Package tq is the only way the api talks to ClickHouse and Postgres. Every statement must contain a
// tenant marker, which is replaced by the tenant filter of the request context (AGENTS rule 7):
//
//	{{tenant}}        → ClickHouse: org_id = O AND project_id = P   · Postgres: project_id = P
//	{{tenant:alias}}  → same, qualified with alias. (e.g. {{tenant:d}} → d.project_id = P)
//	{{project}}       → P (literal, for INSERT values); {{org}} → O
//
// A statement without a marker, or a context without a tenant, is rejected before it reaches the database.
// Tenant ids are uint32, so inlining them as literals is injection-safe.
package tq

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// ErrNoTenantFilter is returned for a statement without a {{tenant}} marker.
var ErrNoTenantFilter = errors.New("tq: statement has no {{tenant}} filter")

// Flavor selects the filter syntax.
type Flavor int

const (
	ClickHouse Flavor = iota
	Postgres
)

var (
	marker  = regexp.MustCompile(`\{\{tenant(?::([A-Za-z_][A-Za-z0-9_]*))?\}\}`)
	literal = regexp.MustCompile(`\{\{(project|org)\}\}`)
)

// Bind replaces tenant markers in sql with the filter for the tenant in ctx.
func Bind(ctx context.Context, f Flavor, sql string) (string, error) {
	t, err := tenant.Must(ctx)
	if err != nil {
		return "", err
	}
	if t.ProjectID == 0 {
		return "", errors.New("tq: tenant without project")
	}
	if !marker.MatchString(sql) && !literal.MatchString(sql) {
		return "", ErrNoTenantFilter
	}
	sql = literal.ReplaceAllStringFunc(sql, func(m string) string {
		if m == "{{org}}" {
			return fmt.Sprint(t.OrgID)
		}
		return fmt.Sprint(t.ProjectID)
	})
	return marker.ReplaceAllStringFunc(sql, func(m string) string {
		alias := marker.FindStringSubmatch(m)[1]
		p := ""
		if alias != "" {
			p = alias + "."
		}
		if f == Postgres {
			return fmt.Sprintf("%sproject_id = %d", p, t.ProjectID)
		}
		return fmt.Sprintf("%sorg_id = %d AND %sproject_id = %d", p, t.OrgID, p, t.ProjectID)
	}), nil
}

// MustTenant returns the tenant of ctx (for values that are written, e.g. INSERT ... VALUES).
func MustTenant(ctx context.Context) (tenant.Tenant, error) { return tenant.Must(ctx) }

// CH wraps a ClickHouse connection.
type CH struct{ Conn driver.Conn }

// Query runs a tenant-filtered SELECT.
func (c CH) Query(ctx context.Context, sql string, args ...any) (driver.Rows, error) {
	q, err := Bind(ctx, ClickHouse, sql)
	if err != nil {
		return nil, err
	}
	return c.Conn.Query(ctx, q, args...)
}

// QueryRow runs a tenant-filtered SELECT returning one row; a bind error surfaces on Scan.
func (c CH) QueryRow(ctx context.Context, sql string, args ...any) driver.Row {
	q, err := Bind(ctx, ClickHouse, sql)
	if err != nil {
		return errRow{err}
	}
	return c.Conn.QueryRow(ctx, q, args...)
}

type errRow struct{ err error }

func (r errRow) Err() error           { return r.err }
func (r errRow) Scan(...any) error    { return r.err }
func (r errRow) ScanStruct(any) error { return r.err }

// PG wraps a Postgres pool.
type PG struct{ Pool *pgxpool.Pool }

// Query runs a tenant-filtered statement returning rows.
func (p PG) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q, err := Bind(ctx, Postgres, sql)
	if err != nil {
		return nil, err
	}
	return p.Pool.Query(ctx, q, args...)
}

// QueryRow runs a tenant-filtered statement returning one row; a bind error surfaces on Scan.
func (p PG) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q, err := Bind(ctx, Postgres, sql)
	if err != nil {
		return errRow{err}
	}
	return p.Pool.QueryRow(ctx, q, args...)
}

// Exec runs a tenant-filtered statement.
func (p PG) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q, err := Bind(ctx, Postgres, sql)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return p.Pool.Exec(ctx, q, args...)
}

// InList renders n positional Postgres placeholders starting at $start: "$3,$4,$5".
func InList(start, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("$%d", start+i)
	}
	return strings.Join(parts, ",")
}
