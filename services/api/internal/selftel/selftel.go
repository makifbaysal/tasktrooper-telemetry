// Package selftel wires the api's own telemetry (ADR-027, contracts §27): trooper.Start with the fixed service name
// tt-api, slog → OTel logs, chi server spans with route templates, outgoing HTTP client spans, pgx and ClickHouse
// spans. Without TROOPER_DSN (and OTEL_EXPORTER_OTLP_ENDPOINT) nothing is started and behaviour is unchanged.
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

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
	trooperchi "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/chi"
	trooperhttp "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/nethttp"
	trooperpgx "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/pgx"
	trooperslog "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/slog"
	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/llm"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// Service is the fixed service name of this binary (ADR-027); TROOPER_SERVICE overrides it.
const Service = "tt-api"

const scope = "github.com/tasktrooper/telemetry/services/api"

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

// Start starts the SDK when Enabled (TROOPER_PROFILING etc. are read by the SDK) and returns a flush function
// for the graceful-shutdown path. Errors are logged; the service keeps running.
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

// Middleware is chi middleware (register it with r.Use on the root router) creating server spans with the chi
// route template; Untraced paths and CORS preflights (answered before routing, so they have no route) bypass it.
// Panics are recorded and re-panicked to the outer recoverer.
func Middleware() func(http.Handler) http.Handler {
	mw := trooperchi.Middleware()
	return func(next http.Handler) http.Handler {
		traced := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if Untraced(r.URL.Path) || preflight(r) {
				next.ServeHTTP(w, r)
				return
			}
			traced.ServeHTTP(w, r)
		})
	}
}

func preflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

// TagTenant puts tt.org_id / tt.project_id of the request tenant on the server span (span attributes only, never
// metric labels). Register it after authentication.
func TagTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t, ok := tenant.From(r.Context()); ok {
			if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
				span.SetAttributes(tenantAttrs(t)...)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func tenantAttrs(t tenant.Tenant) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.Int64("tt.org_id", int64(t.OrgID)), attribute.Int64("tt.project_id", int64(t.ProjectID))}
}

// Client returns c (nil = a new client) with a transport that adds client spans and W3C propagation. peer names the
// dependency (peer.service). Never use it for requests to the ingest gateway (feedback loop, ADR-027). When not
// Enabled, c is returned unchanged.
func Client(c *http.Client, peer string) *http.Client {
	if !Enabled() {
		return c
	}
	if c == nil {
		c = &http.Client{}
	}
	cp := *c
	cp.Transport = Transport(c.Transport, peer)
	return &cp
}

// Transport wraps base (nil = http.DefaultTransport) with client spans + propagation.
func Transport(base http.RoundTripper, peer string) http.RoundTripper {
	var opts []trooperhttp.ClientOption
	if peer != "" {
		opts = append(opts, trooperhttp.WithPeerService(peer))
	}
	return trooperhttp.NewTransport(base, opts...)
}

// LLMTransport is the transport for the incident-summary provider: GenAI client spans (model, tokens; content
// capture stays off unless TROOPER_LLM_CAPTURE_CONTENT=true) over an HTTP client span. nil when not Enabled.
func LLMTransport() http.RoundTripper {
	if !Enabled() {
		return nil
	}
	return llm.Transport(Transport(nil, ""),
		llm.WithProvider("api.meta.ai", "meta"), llm.WithProvider("inference.baseten.co", "baseten"))
}

// PGTracer is the pgx tracer (sanitized SQL, no parameters).
func PGTracer() *trooperpgx.Tracer { return trooperpgx.NewTracer() }

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
