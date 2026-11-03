// Package selftel wires the platform's own telemetry (ADR-027, contracts §27): trooper.Start with the fixed
// service name, slog → OTel logs, redacted HTTP client spans, pgx and ClickHouse spans, and root spans for periodic work.
// Without TROOPER_DSN (and OTEL_EXPORTER_OTLP_ENDPOINT) nothing is started and behaviour is unchanged.
package selftel

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
	trooperpgx "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/pgx"
	trooperslog "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/slog"
)

const scope = "github.com/tasktrooper/telemetry/services/alerting"

// Enabled reports whether self telemetry has somewhere to go.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv("TROOPER_DSN")) != "" || strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

// Config returns the trooper.Config for service (the fixed ADR-027 name); env overrides follow contracts §27.
func Config(service, version string) trooper.Config {
	return trooper.Config{
		Service:     first(os.Getenv("OTEL_SERVICE_NAME"), os.Getenv("TROOPER_SERVICE"), service),
		Environment: first(os.Getenv("TROOPER_ENV"), os.Getenv("TT_ENV"), "development"),
		Version:     first(os.Getenv("TROOPER_VERSION"), version),
	}
}

// Start starts the SDK when Enabled (profiling via TROOPER_PROFILING is read by the SDK) and returns a flush
// function for shutdown. Errors are logged; the service keeps running.
func Start(ctx context.Context, service, version string, log *slog.Logger) func() {
	if !Enabled() {
		return func() {}
	}
	shutdown, err := trooper.Start(ctx, Config(service, version))
	if err != nil {
		log.Warn("self telemetry disabled", "err", err)
		return func() {}
	}
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}
}

// Logger tees log records to OTel logs (with trace_id/span_id on the local output) when Enabled; the local
// handler and its level are kept. Records logged under an SDK entry carry trooper.entry.route / .method (EntryRoute).
func Logger(log *slog.Logger, service string) *slog.Logger {
	if !Enabled() || log == nil {
		return log
	}
	return slog.New(EntryRoute(trooperslog.NewHandler(service, log.Handler(), trooperslog.WithLevel(minLevel(log)))))
}

func minLevel(log *slog.Logger) slog.Level {
	for _, l := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if log.Enabled(context.Background(), l) {
			return l
		}
	}
	return slog.LevelError
}

// PGTracer is the pgx tracer (sanitized SQL, no parameters). otelpgx only starts spans under a recording parent,
// so background polls without a root span stay silent; pool.acquire spans are disabled (noise).
func PGTracer() *trooperpgx.Tracer { return trooperpgx.NewTracer(otelpgx.WithDisableAcquireTracer()) }

func tracer() trace.Tracer { return otel.Tracer(scope) }

// StartJob starts the job entry span (contracts §4: kind consumer, trooper.process = name, trooper.entry.method = JOB)
// of one run of periodic work that did something; idle polls start none. name must be low cardinality; tenant ids
// go in attributes only (Tenant). A local parent in ctx is not used (new root + link).
func StartJob(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return trooper.StartJob(ctx, name, trace.WithAttributes(attrs...))
}

// StartJobAt is StartJob with an explicit start time (work measured before it was known to be a run).
func StartJobAt(ctx context.Context, name string, at time.Time, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return trooper.StartJob(ctx, name, trace.WithTimestamp(at), trace.WithAttributes(attrs...))
}

// StartSpan starts an internal child span.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
}

// End records err (if any, as an exception event with a stack trace) and ends span.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// Tenant returns the tt.org_id / tt.project_id span attributes (never metric labels).
func Tenant(orgID, projectID int64) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.Int64("tt.org_id", orgID), attribute.Int64("tt.project_id", projectID)}
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
