package trooper

import (
	"context"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
)

// NewPropagator returns the trooper propagator: W3C tracecontext + W3C baggage, where every
// outgoing injection sets baggage member tt.svc=<service> (overriding the value received from
// an upstream caller). The receiving trooper SDK records it as trooper.caller.
func NewPropagator(service string) propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, callerBaggage{service: service})
}

type callerBaggage struct {
	service string
	inner   propagation.Baggage
}

func (p callerBaggage) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	if p.service != "" {
		if m, err := baggage.NewMemberRaw(BaggageServiceKey, p.service); err == nil {
			if b, err := baggage.FromContext(ctx).SetMember(m); err == nil {
				ctx = baggage.ContextWithBaggage(ctx, b)
			}
		}
	}
	p.inner.Inject(ctx, carrier)
}

func (p callerBaggage) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return p.inner.Extract(ctx, carrier)
}

func (p callerBaggage) Fields() []string { return p.inner.Fields() }
