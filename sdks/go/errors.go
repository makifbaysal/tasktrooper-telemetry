package trooper

import (
	"context"
	"fmt"
	"reflect"
	"runtime/debug"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/trace"
)

// PanicPolicy controls what server/consumer integrations do after recording a panic.
type PanicPolicy int

const (
	// PanicRepanic records the panic on the span and re-panics so the framework's own recovery
	// (gin.Recovery, echo Recover, net/http server, ...) handles it. Default.
	PanicRepanic PanicPolicy = iota
	// PanicRecover records the panic and converts it to a 500 / codes.Internal / handler error.
	PanicRecover
)

// CaptureError records err on the active span in ctx as an exception event with a stack trace and
// marks the span as failed. If there is no recording span, it emits an ERROR log record instead.
// It never panics and is a no-op for a nil error.
func CaptureError(ctx context.Context, err error, attrs ...attribute.KeyValue) {
	if err == nil {
		return
	}
	defer func() {
		if recover() != nil {
			self.countError("capture")
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.RecordError(err, trace.WithStackTrace(true), trace.WithAttributes(attrs...))
		span.SetStatus(codes.Error, err.Error())
		return
	}
	emitErrorLog(ctx, errType(err), err.Error(), string(debug.Stack()), attrs)
}

// RecordPanic records a recovered panic value on the active span (exception event with the
// goroutine stack, status Error). Integrations call it from their deferred recover.
func RecordPanic(ctx context.Context, recovered any) {
	defer func() {
		if recover() != nil {
			self.countError("capture")
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	msg := fmt.Sprint(recovered)
	typ := "panic"
	if err, ok := recovered.(error); ok {
		typ = errType(err)
	}
	stack := string(debug.Stack())
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		emitErrorLog(ctx, typ, msg, stack, nil)
		return
	}
	span.AddEvent("exception", trace.WithAttributes(
		attribute.String("exception.type", typ),
		attribute.String("exception.message", msg),
		attribute.String("exception.stacktrace", stack),
		attribute.Bool("exception.escaped", true),
	))
	span.SetStatus(codes.Error, "panic: "+msg)
}

// MarkDLQ marks the active (consumer) span as sent to a dead letter queue.
func MarkDLQ(ctx context.Context) {
	trace.SpanFromContext(ctx).SetAttributes(AttrConsumerDLQ.Bool(true))
}

func errType(err error) string {
	t := reflect.TypeOf(err)
	if t == nil {
		return "error"
	}
	return t.String()
}

func emitErrorLog(ctx context.Context, typ, msg, stack string, attrs []attribute.KeyValue) {
	l := global.GetLoggerProvider().Logger(ScopeName)
	var r log.Record
	r.SetSeverity(log.SeverityError)
	r.SetSeverityText("ERROR")
	r.SetBody(attribute.StringValue(msg))
	r.AddAttributes(
		attribute.String("exception.type", typ),
		attribute.String("exception.message", msg),
		attribute.String("exception.stacktrace", stack),
	)
	r.AddAttributes(attrs...)
	l.Emit(ctx, r)
}
