package trooperslog

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys injected into records passed to the local handler.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
)

type config struct {
	level slog.Leveler
	otel  []otelslog.Option
}

// Option configures NewHandler.
type Option func(*config)

// WithLevel sets the minimum level exported to OpenTelemetry (default slog.LevelInfo).
// The local handler keeps its own level.
func WithLevel(l slog.Leveler) Option { return func(c *config) { c.level = l } }

// WithOtelOptions passes options to the otelslog bridge (e.g. otelslog.WithSource(true)).
func WithOtelOptions(opts ...otelslog.Option) Option {
	return func(c *config) { c.otel = append(c.otel, opts...) }
}

type handler struct {
	level slog.Leveler
	otel  slog.Handler
	next  slog.Handler // may be nil
}

// NewHandler returns a handler that sends records to OpenTelemetry (instrumentation scope name)
// and to next (may be nil) with trace_id/span_id attributes added when ctx carries a span.
func NewHandler(name string, next slog.Handler, opts ...Option) slog.Handler {
	c := &config{level: slog.LevelInfo}
	for _, o := range opts {
		o(c)
	}
	return &handler{level: c.level, otel: otelslog.NewHandler(name, c.otel...), next: next}
}

// New returns a logger exporting to OpenTelemetry and writing text to stderr.
func New(name string, opts ...Option) *slog.Logger {
	return slog.New(NewHandler(name, slog.NewTextHandler(os.Stderr, nil), opts...))
}

func (h *handler) otelEnabled(ctx context.Context, l slog.Level) bool {
	return l >= h.level.Level() && h.otel.Enabled(ctx, l)
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.otelEnabled(ctx, l) || (h.next != nil && h.next.Enabled(ctx, l))
}

func (h *handler) Handle(ctx context.Context, r slog.Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = nil // never crash the host because of telemetry
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	var errs []error
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		nr := r.Clone()
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			nr.AddAttrs(slog.String(TraceIDKey, sc.TraceID().String()), slog.String(SpanIDKey, sc.SpanID().String()))
		}
		errs = append(errs, h.next.Handle(ctx, nr))
	}
	if h.otelEnabled(ctx, r.Level) {
		_ = h.otel.Handle(ctx, r) // export errors are handled by the SDK
	}
	return errors.Join(errs...)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := &handler{level: h.level, otel: h.otel.WithAttrs(attrs)}
	if h.next != nil {
		n.next = h.next.WithAttrs(attrs)
	}
	return n
}

func (h *handler) WithGroup(name string) slog.Handler {
	n := &handler{level: h.level, otel: h.otel.WithGroup(name)}
	if h.next != nil {
		n.next = h.next.WithGroup(name)
	}
	return n
}
