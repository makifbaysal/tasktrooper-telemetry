package trooperzap

import (
	"context"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Field keys used for trace correlation in local output.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
)

// NewCore returns an otelzap core exporting to the global LoggerProvider.
func NewCore(name string, opts ...otelzap.Option) zapcore.Core {
	return otelzap.NewCore(name, opts...)
}

// Tee combines a local core (stdout, file, ...) with the OpenTelemetry core. In the local core,
// context fields (see Context) are replaced with trace_id/span_id.
func Tee(local zapcore.Core, name string, opts ...otelzap.Option) zapcore.Core {
	if local == nil {
		return NewCore(name, opts...)
	}
	return zapcore.NewTee(localCore{local}, NewCore(name, opts...))
}

// Context returns a field carrying ctx; otelzap uses it as the log record context.
func Context(ctx context.Context) zap.Field { return zap.Any("context", ctx) }

// TraceFields returns trace_id and span_id fields for the span in ctx (nil if none).
func TraceFields(ctx context.Context) []zap.Field {
	if ctx == nil {
		return nil
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []zap.Field{zap.String(TraceIDKey, sc.TraceID().String()), zap.String(SpanIDKey, sc.SpanID().String())}
}

type localCore struct{ zapcore.Core }

func (c localCore) With(fields []zapcore.Field) zapcore.Core {
	return localCore{c.Core.With(rewrite(fields))}
}

func (c localCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c localCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	return c.Core.Write(ent, rewrite(fields))
}

// rewrite replaces context.Context fields by trace_id/span_id.
func rewrite(fields []zapcore.Field) []zapcore.Field {
	idx := -1
	for i, f := range fields {
		if _, ok := f.Interface.(context.Context); ok {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fields
	}
	out := make([]zapcore.Field, 0, len(fields)+1)
	for _, f := range fields {
		if ctx, ok := f.Interface.(context.Context); ok {
			out = append(out, TraceFields(ctx)...)
			continue
		}
		out = append(out, f)
	}
	return out
}
