package trooperredis

import (
	"errors"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
)

type config struct {
	noMetrics bool
	attrs     []attribute.KeyValue
	tracing   []redisotel.TracingOption
	metrics   []redisotel.MetricsOption
}

// Option configures Instrument.
type Option func(*config)

// WithNamespace sets db.namespace (e.g. the logical database index "0" or a cluster name).
func WithNamespace(ns string) Option {
	return func(c *config) { c.attrs = append(c.attrs, attribute.String("db.namespace", ns)) }
}

// WithoutMetrics disables redisotel metrics.
func WithoutMetrics() Option { return func(c *config) { c.noMetrics = true } }

// WithTracingOptions passes raw redisotel tracing options (applied after trooper defaults, so
// redisotel.WithDBStatement(true) re-enables command text — avoid it if values may contain PII).
func WithTracingOptions(opts ...redisotel.TracingOption) Option {
	return func(c *config) { c.tracing = append(c.tracing, opts...) }
}

// WithMetricsOptions passes raw redisotel metrics options.
func WithMetricsOptions(opts ...redisotel.MetricsOption) Option {
	return func(c *config) { c.metrics = append(c.metrics, opts...) }
}

// Instrument adds tracing (and metrics) hooks to rdb. Errors only concern telemetry setup.
func Instrument(rdb redis.UniversalClient, opts ...Option) error {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	attrs := append([]attribute.KeyValue{attribute.String("db.system.name", "redis")}, c.attrs...)
	topts := append([]redisotel.TracingOption{
		redisotel.WithDBSystem("redis"),
		redisotel.WithDBStatement(false),
		redisotel.WithCallerEnabled(false),
		redisotel.WithAttributes(attrs...),
	}, c.tracing...)
	err := redisotel.InstrumentTracing(rdb, topts...)
	if !c.noMetrics {
		mopts := append([]redisotel.MetricsOption{redisotel.WithDBSystem("redis"), redisotel.WithAttributes(c.attrs...)}, c.metrics...)
		err = errors.Join(err, redisotel.InstrumentMetrics(rdb, mopts...))
	}
	return err
}
