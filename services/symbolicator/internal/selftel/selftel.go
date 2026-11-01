// Package selftel wires the symbolicator's own telemetry (ADR-027, contracts §27): trooper.Start with the fixed
// service name tt-symbolicator, slog → OTel logs, net/http server spans with route templates, pgx, go-redis and S3
// client spans. Without TROOPER_DSN (and OTEL_EXPORTER_OTLP_ENDPOINT) nothing is started and behaviour is unchanged.
package selftel

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperredis "github.com/tasktrooper/telemetry-go/integrations/goredis"
	trooperhttp "github.com/tasktrooper/telemetry-go/integrations/nethttp"
	trooperpgx "github.com/tasktrooper/telemetry-go/integrations/pgx"
	trooperslog "github.com/tasktrooper/telemetry-go/integrations/slog"
)

// Service is the fixed service name of this binary (ADR-027); TROOPER_SERVICE overrides it.
const Service = "tt-symbolicator"

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

// Start starts the SDK when Enabled and returns a flush function for the graceful-shutdown path. Errors are
// logged; the service keeps running.
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

// Untraced paths never produce spans (health checks and scrapes).
func Untraced(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/metrics"
}

// Handler wraps mux with server spans (route templates from the mux patterns); Untraced paths bypass them. Panics
// are recorded and re-panicked to the outer recoverer.
func Handler(mux *http.ServeMux) http.Handler {
	traced := trooperhttp.Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Untraced(r.URL.Path) {
			mux.ServeHTTP(w, r)
			return
		}
		traced.ServeHTTP(w, r)
	})
}

// TagProject puts tt.project_id on the server span (span attribute only, never a metric label).
func TagProject(ctx context.Context, project int64) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attribute.Int64("tt.project_id", project))
	}
}

// Client returns an *http.Client with client spans (peer.service = peer) for an AWS SDK / S3 client; nil when not
// Enabled (the SDK default client is kept).
func Client(peer string) *http.Client {
	if !Enabled() {
		return nil
	}
	return &http.Client{Transport: trooperhttp.NewTransport(nil, trooperhttp.WithPeerService(peer))}
}

// PGTracer is the pgx tracer (sanitized SQL, no parameters).
func PGTracer() *trooperpgx.Tracer { return trooperpgx.NewTracer() }

// Redis instruments rc (client spans, no command arguments) when Enabled.
func Redis(rc *redis.Client) error {
	if !Enabled() || rc == nil {
		return nil
	}
	return trooperredis.Instrument(rc)
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
