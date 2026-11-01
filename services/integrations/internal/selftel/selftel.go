// Package selftel wires trooper-cloud's own telemetry (ADR-027, contracts §27): trooper.Start with the fixed service
// name tt-trooper-cloud, slog → OTel logs, pgx spans, client spans on the cloud provider API calls and one root span
// per discovery / metrics step. Without TROOPER_DSN (and OTEL_EXPORTER_OTLP_ENDPOINT) nothing is started.
//
// The OTLP metrics this service sends to ingest-gw (internal/otlp) are never traced: that would be a feedback loop.
package selftel

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperhttp "github.com/tasktrooper/telemetry-go/integrations/nethttp"
	trooperpgx "github.com/tasktrooper/telemetry-go/integrations/pgx"
	trooperslog "github.com/tasktrooper/telemetry-go/integrations/slog"
)

// Service is the fixed service name of this binary (ADR-027); TROOPER_SERVICE overrides it.
const Service = "tt-trooper-cloud"

// Enabled reports whether self telemetry has somewhere to go.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv("TROOPER_DSN")) != "" || strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

// Config returns the trooper.Config for this service; env overrides follow contracts §27.
func Config(version string) trooper.Config {
	return trooper.Config{
		Service:     first(os.Getenv("OTEL_SERVICE_NAME"), os.Getenv("TROOPER_SERVICE"), Service),
		Environment: first(os.Getenv("TROOPER_ENV"), os.Getenv("TT_ENV"), "development"),
		Version:     first(os.Getenv("TROOPER_VERSION"), version),
	}
}

// Start starts the SDK when Enabled and returns a flush function for shutdown. Errors are logged; the service
// keeps running.
func Start(ctx context.Context, version string, log *slog.Logger) func() {
	if !Enabled() {
		return func() {}
	}
	shutdown, err := trooper.Start(ctx, Config(version))
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

// Logger tees records to OTel logs (and adds trace_id/span_id to the local output) when Enabled; the local
// handler and its level are kept.
func Logger(log *slog.Logger) *slog.Logger {
	if !Enabled() || log == nil {
		return log
	}
	return slog.New(trooperslog.NewHandler(Config("").Service, log.Handler(), trooperslog.WithLevel(minLevel(log))))
}

func minLevel(log *slog.Logger) slog.Level {
	for _, l := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if log.Enabled(context.Background(), l) {
			return l
		}
	}
	return slog.LevelError
}

// Transport returns a client-span transport over http.DefaultTransport for cloud provider API calls, or nil (= the
// default transport) when not Enabled.
func Transport() http.RoundTripper {
	if !Enabled() {
		return nil
	}
	return trooperhttp.NewTransport(nil)
}

// Client returns a traced client with timeout for cloud provider API calls, or nil (= provider default) when not
// Enabled.
func Client(timeout time.Duration) *http.Client {
	if !Enabled() {
		return nil
	}
	return &http.Client{Timeout: timeout, Transport: Transport()}
}

// PGTracer is the pgx tracer (sanitized SQL, no parameters).
func PGTracer() *trooperpgx.Tracer { return trooperpgx.NewTracer() }

// StartJob starts the job entry span (contracts §4: kind consumer, trooper.process = name, trooper.entry.method = JOB)
// of one run of periodic work that did something; idle polls start none. name must be low cardinality; tenant ids
// go in attributes only (Tenant). A local parent in ctx is not used (new root + link).
func StartJob(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return trooper.StartJob(ctx, name, trace.WithAttributes(attrs...))
}

// End records err (if any) and ends span.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
