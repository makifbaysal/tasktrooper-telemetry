package trooper

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// JobMethod is trooper.entry.method of background job entry spans (.ai/contracts.md §4).
const JobMethod = "JOB"

// StartJob starts the entry span of one run of a background job (a scheduler tick, a poll that found
// work, a batch): span kind consumer, name = trooper.process = trooper.entry.route = name and
// trooper.entry.method = JOB, so the run is counted like a consumer process (throughput, errors,
// duration) and every span started from the returned context belongs to it. name must be low
// cardinality (e.g. "billing.sync", never per-tenant or per-item); put ids in attributes.
//
// A job entry never has a parent in the same service: a local span in ctx is not used as the parent
// (the run starts a new trace and links to it); a remote parent (e.g. a TRACEPARENT handed over by
// another service) is kept. Start it only when the run does work — an idle poll should produce no span,
// otherwise the job's throughput is just the poll rate. End the span with EndSpan; RunJob does both.
//
// Without a configured provider the returned span is a no-op. StartJob never panics.
func StartJob(ctx context.Context, name string, opts ...trace.SpanStartOption) (_ context.Context, span trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if recover() != nil {
			self.countError("job")
			span = trace.SpanFromContext(ctx)
		}
	}()
	so := make([]trace.SpanStartOption, 0, 4+len(opts))
	so = append(so, trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(AttrProcess.String(name)))
	if parent := trace.SpanContextFromContext(ctx); parent.IsValid() && !parent.IsRemote() {
		so = append(so, trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: parent}))
	}
	ctx, span = tracer().Start(ctx, name, append(so, opts...)...)
	ctx, _ = BeginEntry(ctx, JobMethod, name, nil)
	return ctx, span
}

// RunJob runs fn inside a job entry span (see StartJob) and ends it with fn's error. A panic in fn is
// recorded and, with PanicRepanic, re-raised after the span is ended; with PanicRecover it is returned
// as an error.
func RunJob(ctx context.Context, name string, policy PanicPolicy, fn func(context.Context) error, opts ...trace.SpanStartOption) (err error) {
	ctx, span := StartJob(ctx, name, opts...)
	defer func() {
		EntryFromContext(ctx).restoreLabels()
		if r := recover(); r != nil {
			RecordPanic(ctx, r)
			span.End()
			if policy == PanicRepanic {
				panic(r)
			}
			err = &PanicError{Value: r}
			return
		}
		EndSpan(span, err)
	}()
	return fn(ctx)
}
