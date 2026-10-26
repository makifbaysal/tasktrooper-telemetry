package troopersql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel/attribute"

	trooper "github.com/tasktrooper/telemetry-go"
)

type config struct {
	system    string
	namespace string
	peer      string
	extra     []otelsql.Option
	stats     bool
}

// Option configures the instrumentation.
type Option func(*config)

// WithSystem sets db.system (e.g. "postgresql", "mysql"). Default: inferred from the driver name.
func WithSystem(system string) Option { return func(c *config) { c.system = system } }

// WithNamespace sets db.namespace (database name).
func WithNamespace(name string) Option { return func(c *config) { c.namespace = name } }

// WithPeer sets server.address (overrides the value parsed from the DSN).
func WithPeer(host string) Option { return func(c *config) { c.peer = host } }

// WithDBStats registers connection pool metrics (db.sql.connection.*) for databases returned by Open.
func WithDBStats() Option { return func(c *config) { c.stats = true } }

// WithOtelSQLOptions passes raw otelsql options (applied after the trooper defaults).
func WithOtelSQLOptions(opts ...otelsql.Option) Option {
	return func(c *config) { c.extra = append(c.extra, opts...) }
}

// SystemFromDriver infers db.system from a database/sql driver name.
func SystemFromDriver(driverName string) string {
	d := strings.ToLower(driverName)
	switch {
	case strings.Contains(d, "pgx"), strings.Contains(d, "postgres"), d == "pq", strings.Contains(d, "cockroach"):
		return "postgresql"
	case strings.Contains(d, "mysql"):
		return "mysql"
	case strings.Contains(d, "sqlite"):
		return "sqlite"
	case strings.Contains(d, "sqlserver"), strings.Contains(d, "mssql"):
		return "mssql"
	case strings.Contains(d, "clickhouse"):
		return "clickhouse"
	case strings.Contains(d, "oracle"), d == "godror", d == "oci8":
		return "oracle"
	default:
		return "other_sql"
	}
}

func build(driverName, dsn string, opts []Option) (*config, []otelsql.Option) {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	if c.system == "" {
		c.system = SystemFromDriver(driverName)
	}
	attrs := []attribute.KeyValue{
		attribute.String("db.system", c.system),
		attribute.String("db.system.name", c.system),
	}
	if c.namespace != "" {
		attrs = append(attrs, attribute.String("db.namespace", c.namespace))
	}
	if c.peer != "" {
		attrs = append(attrs, attribute.String("server.address", c.peer))
	} else if dsn != "" {
		attrs = append(attrs, otelsql.AttributesFromDSN(dsn)...)
	}
	oo := []otelsql.Option{
		otelsql.WithAttributes(attrs...),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableQuery:         true, // raw text replaced by the sanitized getter below
			DisableErrSkip:       true,
			OmitConnResetSession: true,
			OmitRows:             true,
		}),
		otelsql.WithAttributesGetter(queryAttributes),
	}
	return c, append(oo, c.extra...)
}

// queryAttributes records the sanitized statement; arguments are ignored.
func queryAttributes(_ context.Context, _ otelsql.Method, query string, _ []driver.NamedValue) []attribute.KeyValue {
	if query == "" {
		return nil
	}
	return []attribute.KeyValue{attribute.String("db.query.text", trooper.SanitizeSQL(query))}
}

// Options returns the otelsql options trooper uses, for use with otelsql APIs directly.
func Options(driverName, dsn string, opts ...Option) []otelsql.Option {
	_, oo := build(driverName, dsn, opts)
	return oo
}

// Open is an instrumented sql.Open.
func Open(driverName, dsn string, opts ...Option) (*sql.DB, error) {
	c, oo := build(driverName, dsn, opts)
	db, err := otelsql.Open(driverName, dsn, oo...)
	if err != nil {
		return nil, err
	}
	if c.stats {
		_, _ = otelsql.RegisterDBStatsMetrics(db, oo...)
	}
	return db, nil
}

// OpenDB is an instrumented sql.OpenDB. driverName is only used to infer db.system.
func OpenDB(driverName string, connector driver.Connector, opts ...Option) *sql.DB {
	c, oo := build(driverName, "", opts)
	db := otelsql.OpenDB(connector, oo...)
	if c.stats {
		_, _ = otelsql.RegisterDBStatsMetrics(db, oo...)
	}
	return db
}

// Register registers an instrumented wrapper of driverName and returns the new driver name
// to pass to sql.Open.
func Register(driverName string, opts ...Option) (string, error) {
	_, oo := build(driverName, "", opts)
	return otelsql.Register(driverName, oo...)
}

// WrapDriver wraps a driver.Driver. driverName is only used to infer db.system.
func WrapDriver(driverName string, d driver.Driver, opts ...Option) driver.Driver {
	_, oo := build(driverName, "", opts)
	return otelsql.WrapDriver(d, oo...)
}
