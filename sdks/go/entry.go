package trooper

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type entryKey struct{}

// Entry is the entry point (HTTP route, RPC method or consumer process) of a unit of work.
// It is stored in the context by BeginEntry; the trooper span processor copies its route and
// method onto every span started under that context (trooper.entry.route / trooper.entry.method).
//
// Integrations whose router resolves the route template only after middleware ran (net/http
// ServeMux, chi, fiber) pass a resolver that is consulted lazily when child spans start, and call
// Finish when the request completes. All methods are safe for concurrent use.
type Entry struct {
	span    trace.Span
	mu      sync.Mutex
	route   string
	method  string
	resolve func() string
	done    bool

	// pprof goroutine labels (only while profile labels are enabled)
	labelParent context.Context
	labeled     bool
}

// BeginEntry marks ctx as the entry of a request/message handled by the span in ctx.
// route may be empty if not yet known; resolve (optional) is called lazily to obtain it until
// Finish is called. It also records trooper.caller from baggage (tt.svc) on the span.
//
// While profile labels are enabled (continuous profiling, see WithProfiling), the calling goroutine
// is labelled with pprof labels trooper.route / trooper.method and the returned context carries
// them; Finish restores the previous labels, so Finish must run on the same goroutine.
func BeginEntry(ctx context.Context, method, route string, resolve func() string) (context.Context, *Entry) {
	e := &Entry{span: trace.SpanFromContext(ctx), method: method, route: NormalizeRoute(route), resolve: resolve}
	if caller := CallerFromContext(ctx); caller != "" {
		e.span.SetAttributes(AttrCaller.String(caller))
	}
	if e.route != "" {
		e.span.SetAttributes(e.attrs(e.route)...)
	}
	parent := ctx
	ctx = context.WithValue(ctx, entryKey{}, e)
	if ProfileLabelsEnabled() {
		ctx = e.applyLabels(parent, ctx, e.route)
	}
	return ctx, e
}

// Finish freezes the entry with the final route (if non-empty) and sets the entry attributes on
// the entry span. The resolver is not called after Finish. It also restores the pprof goroutine
// labels set by BeginEntry. Safe to call on nil.
func (e *Entry) Finish(route string) {
	if e == nil {
		return
	}
	e.restoreLabels()
	e.mu.Lock()
	if r := NormalizeRoute(route); r != "" {
		e.route = r
	} else if e.route == "" && e.resolve != nil {
		e.route = NormalizeRoute(safeResolve(e.resolve))
	}
	e.done, e.resolve = true, nil
	r := e.route
	e.mu.Unlock()
	if r != "" {
		e.span.SetAttributes(e.attrs(r)...)
	}
}

// Route returns the current entry route and method.
func (e *Entry) Route() (route, method string) {
	if e == nil {
		return "", ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.done && e.resolve != nil {
		if r := NormalizeRoute(safeResolve(e.resolve)); r != "" {
			e.route = r
		}
	}
	return e.route, e.method
}

func (e *Entry) attrs(route string) []attribute.KeyValue {
	kv := []attribute.KeyValue{AttrEntryRoute.String(route)}
	if e.method != "" {
		kv = append(kv, AttrEntryMethod.String(e.method))
	}
	return kv
}

func safeResolve(f func() string) (s string) {
	defer func() {
		if recover() != nil {
			self.countError("resolver")
			s = ""
		}
	}()
	return f()
}

// EntryFromContext returns the entry stored in ctx, or nil.
func EntryFromContext(ctx context.Context) *Entry {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(entryKey{}).(*Entry)
	return e
}

// CallerFromContext returns the calling service from W3C baggage member tt.svc.
func CallerFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(BaggageServiceKey).Value()
}

// SetServerRoute sets trooper.route (normalized) and http.route (as given) on a server span.
func SetServerRoute(span trace.Span, route string) {
	if route == "" || span == nil {
		return
	}
	span.SetAttributes(AttrRoute.String(NormalizeRoute(route)), attribute.String("http.route", route))
}

// spanProcessor copies the entry route onto every span and marks entry spans with the sample rate.
type spanProcessor struct{ sampleRate float64 }

// NewSpanProcessor returns the trooper span processor. Start registers it automatically; use it
// only when building your own TracerProvider.
func NewSpanProcessor(sampleRate float64) sdktrace.SpanProcessor {
	if sampleRate <= 0 || sampleRate > 1 {
		sampleRate = 1
	}
	return &spanProcessor{sampleRate: sampleRate}
}

func (p *spanProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	defer func() {
		if recover() != nil {
			self.countError("processor")
		}
	}()
	if e := EntryFromContext(parent); e != nil {
		if route, method := e.Route(); route != "" {
			s.SetAttributes(AttrEntryRoute.String(route))
			if method != "" {
				s.SetAttributes(AttrEntryMethod.String(method))
			}
		}
	}
	if k := s.SpanKind(); k == trace.SpanKindServer || k == trace.SpanKindConsumer {
		if parent := s.Parent(); !parent.IsValid() {
			s.SetAttributes(AttrSampleRate.Float64(p.sampleRate))
		} else if parent.IsRemote() {
			// The upstream root decided sampling: use its rate from tracestate tt=r:<rate>.
			rate, ok := rateFromTraceState(parent.TraceState())
			if !ok {
				rate = p.sampleRate
			}
			s.SetAttributes(AttrSampleRate.Float64(rate))
		}
	}
}

func (p *spanProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (p *spanProcessor) Shutdown(context.Context) error   { return nil }
func (p *spanProcessor) ForceFlush(context.Context) error { return nil }
