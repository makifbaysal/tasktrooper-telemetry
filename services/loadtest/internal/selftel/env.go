package selftel

import (
	"context"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Runner env keys for the W3C trace context of the ctl run span (no baggage: nothing else crosses the boundary).
const (
	EnvTraceparent = "TRACEPARENT"
	EnvTracestate  = "TRACESTATE"
)

// SecretEnv are pass-through variables that carry credentials (the DSN embeds the ingest key): the k8s executor
// must take them from TT_LOADTEST_K8S_SECRET, never from the plain Job env.
var SecretEnv = []string{"TROOPER_DSN"}

// passThrough are the self-telemetry variables a runner inherits from loadtest-ctl. TROOPER_SERVICE is not
// passed (the runner has its own fixed name); the environment is resolved (TROOPER_ENV > TT_ENV > development).
var passThrough = []string{
	"TROOPER_DSN", "TROOPER_VERSION", "TROOPER_SAMPLE_RATE", "TROOPER_DEBUG", "TROOPER_ENABLED",
	"TROOPER_PROFILING", "TROOPER_PROFILING_TYPES", "TROOPER_PROFILING_PERIOD", "TROOPER_PROFILING_CPU_DURATION",
}

// RunnerEnv returns the self-telemetry env for runners started by this process (getenv nil = os.Getenv).
// Empty when self telemetry is off, so runner behaviour is unchanged.
func RunnerEnv(getenv func(string) string) map[string]string {
	if getenv == nil {
		getenv = os.Getenv
	}
	out := map[string]string{}
	if strings.TrimSpace(getenv("TROOPER_DSN")) == "" {
		return out
	}
	for _, k := range passThrough {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			out[k] = v
		}
	}
	out["TROOPER_ENV"] = first(getenv("TROOPER_ENV"), getenv("TT_ENV"), "development")
	return out
}

// TraceEnv returns TRACEPARENT (and TRACESTATE) for the span in ctx; empty when ctx has no sampled span.
func TraceEnv(ctx context.Context) map[string]string {
	c := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, c)
	out := map[string]string{}
	if v := c.Get("traceparent"); v != "" {
		out[EnvTraceparent] = v
	}
	if v := c.Get("tracestate"); v != "" {
		out[EnvTracestate] = v
	}
	return out
}

// FromEnv returns ctx with the remote parent from TRACEPARENT/TRACESTATE (getenv nil = os.Getenv); ctx unchanged
// when absent or invalid.
func FromEnv(ctx context.Context, getenv func(string) string) context.Context {
	if getenv == nil {
		getenv = os.Getenv
	}
	tp := strings.TrimSpace(getenv(EnvTraceparent))
	if tp == "" {
		return ctx
	}
	c := propagation.MapCarrier{"traceparent": tp}
	if ts := strings.TrimSpace(getenv(EnvTracestate)); ts != "" {
		c["tracestate"] = ts
	}
	out := propagation.TraceContext{}.Extract(ctx, c)
	if !trace.SpanContextFromContext(out).IsValid() {
		return ctx
	}
	return out
}

// SpanContext returns ctx's span context (for parenting later lifecycle spans of the same run).
func SpanContext(ctx context.Context) trace.SpanContext { return trace.SpanContextFromContext(ctx) }

// StartIn starts an internal span under parent (a run's claim span, which may have ended) keeping ctx's
// deadline/cancellation; a root span when parent is invalid.
func StartIn(ctx context.Context, parent trace.SpanContext, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if parent.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, parent)
	} else {
		opts = append(opts, trace.WithNewRoot())
	}
	return tracer().Start(ctx, name, append(opts, trace.WithSpanKind(trace.SpanKindInternal))...)
}

// Untraced returns ctx without its span so high-frequency polls (abort control every second, shard updates
// every tick) under a long run span produce no Postgres spans (otelpgx only traces under a recording parent).
func Untraced(ctx context.Context) context.Context {
	return trace.ContextWithSpanContext(ctx, trace.SpanContext{})
}

// StartSpanAt starts an internal child span with an explicit start time (reconstructed stage spans).
func StartSpanAt(ctx context.Context, name string, at time.Time, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer().Start(ctx, name, trace.WithTimestamp(at), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
}
