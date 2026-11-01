// Package selftel wires tt-mcp's own telemetry (ADR-027, contracts §27): trooper.Start with the fixed service name
// tt-mcp, slog → OTel logs, one server span per tool call and client spans (with traceparent) on the calls to
// services/api. Without TROOPER_DSN (and OTEL_EXPORTER_OTLP_ENDPOINT) nothing is started.
//
// stdout is the MCP protocol channel: the SDK writes its own diagnostics (TROOPER_DEBUG) to stderr only, and nothing
// here prints.
package selftel

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperhttp "github.com/tasktrooper/telemetry-go/integrations/nethttp"
	trooperslog "github.com/tasktrooper/telemetry-go/integrations/slog"
)

// Service is the fixed service name of this binary (ADR-027); TROOPER_SERVICE overrides it.
const Service = "tt-mcp"

const scope = "github.com/tasktrooper/telemetry/services/mcp"

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

// Start starts the SDK when Enabled and returns a flush function for shutdown. Errors are logged; the server keeps
// running.
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
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}
}

// Logger tees records to OTel logs (and adds trace_id/span_id to the local stderr output) when Enabled; the local
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

// Transport wraps base (nil = http.DefaultTransport) with client spans and W3C propagation (peer.service tt-api)
// when Enabled; otherwise base is returned unchanged.
func Transport(base http.RoundTripper) http.RoundTripper {
	if !Enabled() {
		return base
	}
	return trooperhttp.NewTransport(base, trooperhttp.WithPeerService("tt-api"))
}

// ToolMiddleware traces every tools/call as a server (entry) span named after the tool (tt_*): trooper.route = tool
// name, method MCP, gen_ai.tool.name, status Error for a failed call or a result with isError. Other MCP methods
// (initialize, tools/list, …) are not traced. Arguments and results are never recorded.
func ToolMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			name := "tools/call"
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil && p.Name != "" {
				name = p.Name
			}
			ctx, span := otel.Tracer(scope).Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
				attribute.String("mcp.method.name", method),
				attribute.String("gen_ai.operation.name", "execute_tool"),
				attribute.String("gen_ai.tool.name", name),
				trooper.AttrRoute.String(name),
			))
			ctx, entry := trooper.BeginEntry(ctx, "MCP", name, nil)
			defer func() {
				if rec := recover(); rec != nil {
					trooper.RecordPanic(ctx, rec)
					entry.Finish(name)
					span.End()
					panic(rec)
				}
			}()
			res, err := next(ctx, method, req)
			entry.Finish(name)
			switch r, _ := res.(*mcp.CallToolResult); {
			case err != nil:
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			case r != nil && r.IsError:
				span.SetStatus(codes.Error, "tool error")
			}
			span.End()
			return res, err
		}
	}
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
