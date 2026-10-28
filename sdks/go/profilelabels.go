package trooper

import (
	"context"
	"runtime/pprof"
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"
)

// pprof label keys set on request goroutines (.ai/contracts.md §24).
const (
	ProfileLabelRoute  = "trooper.route"
	ProfileLabelMethod = "trooper.method"
)

var (
	profilerLabels atomic.Bool // set while the trooper profiler runs
	userLabels     atomic.Bool // EnableProfileLabels
)

// ProfileLabelsEnabled reports whether entry points label their goroutines with pprof labels
// (true while continuous profiling runs, or after EnableProfileLabels(true)).
func ProfileLabelsEnabled() bool { return profilerLabels.Load() || userLabels.Load() }

// EnableProfileLabels turns pprof goroutine labels (trooper.route, trooper.method) on entry points
// on or off independently of the trooper profiler, e.g. when another continuous profiler collects
// the profiles. Labels are off by default so they cost nothing when nobody profiles.
func EnableProfileLabels(on bool) { userLabels.Store(on) }

// labelMethod returns the trooper.method label of an entry: the HTTP method, CONSUME for
// consumers and GRPC for RPC servers (which call BeginEntry without a method).
func labelMethod(span trace.Span, method string) string {
	if method != "" {
		return method
	}
	if k, ok := span.(interface{ SpanKind() trace.SpanKind }); ok {
		switch k.SpanKind() {
		case trace.SpanKindConsumer:
			return "CONSUME"
		case trace.SpanKindServer:
			return "GRPC"
		}
	}
	return ""
}

// applyLabels labels the current goroutine (and returns ctx carrying the labels, so goroutines
// started later with pprof.Do / SetGoroutineLabels(ctx) can reuse them). The labels in effect
// before (those of parent) are restored by Finish.
func (e *Entry) applyLabels(parent, ctx context.Context, route string) context.Context {
	lm := labelMethod(e.span, e.method)
	if route == "" && lm == "" {
		return ctx
	}
	kv := make([]string, 0, 4)
	if route != "" {
		kv = append(kv, ProfileLabelRoute, route)
	}
	if lm != "" {
		kv = append(kv, ProfileLabelMethod, lm)
	}
	ctx = pprof.WithLabels(ctx, pprof.Labels(kv...))
	pprof.SetGoroutineLabels(ctx)
	e.mu.Lock()
	if !e.labeled {
		e.labelParent, e.labeled = parent, true
	}
	e.mu.Unlock()
	return ctx
}

// LabelRoute (re)labels the current goroutine with route once the integration knows it, for
// routers that resolve the route after the middleware ran. It returns ctx with the labels; a no-op
// returning ctx unchanged when profile labels are disabled or e is nil.
func (e *Entry) LabelRoute(ctx context.Context, route string) context.Context {
	if e == nil || ctx == nil || !ProfileLabelsEnabled() {
		return ctx
	}
	route = NormalizeRoute(route)
	if route == "" {
		return ctx
	}
	e.mu.Lock()
	parent, labeled := e.labelParent, e.labeled
	e.mu.Unlock()
	if !labeled {
		parent = ctx
	}
	return e.applyLabels(parent, ctx, route)
}

// restoreLabels resets the goroutine labels to those in effect before BeginEntry.
func (e *Entry) restoreLabels() {
	if e == nil {
		return
	}
	e.mu.Lock()
	parent, labeled := e.labelParent, e.labeled
	e.labeled, e.labelParent = false, nil
	e.mu.Unlock()
	if labeled {
		pprof.SetGoroutineLabels(parent)
	}
}
