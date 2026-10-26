package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.baggage.Baggage;
import io.opentelemetry.api.baggage.propagation.W3CBaggagePropagator;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.propagation.TextMapGetter;
import io.opentelemetry.context.propagation.TextMapPropagator;
import io.opentelemetry.context.propagation.TextMapSetter;
import java.util.Collection;

/**
 * Decorates a baggage-capable propagator: on inject, sets the W3C baggage member {@code tt.svc=<service>} so the
 * downstream service can record {@code trooper.caller}. Extraction is delegated unchanged.
 */
public final class TrooperBaggagePropagator implements TextMapPropagator {
    private final TextMapPropagator delegate;
    private final String service;

    private TrooperBaggagePropagator(TextMapPropagator delegate, String service) {
        this.delegate = delegate;
        this.service = service;
    }

    /** Wraps {@code delegate} (which must emit the {@code baggage} header). */
    public static TextMapPropagator wrap(TextMapPropagator delegate, String service) {
        if (service == null || service.isBlank() || delegate instanceof TrooperBaggagePropagator) {
            return delegate;
        }
        return new TrooperBaggagePropagator(delegate, service);
    }

    /** W3C baggage propagator that adds {@code tt.svc}. */
    public static TextMapPropagator create(String service) {
        return wrap(W3CBaggagePropagator.getInstance(), service);
    }

    /** True when the propagator writes the W3C {@code baggage} header. */
    public static boolean handlesBaggage(TextMapPropagator p) {
        return p.fields().contains("baggage");
    }

    @Override
    public Collection<String> fields() {
        return delegate.fields();
    }

    @Override
    public <C> void inject(Context context, C carrier, TextMapSetter<C> setter) {
        Context ctx = context;
        try {
            Baggage baggage = Baggage.fromContext(context).toBuilder()
                    .put(TrooperAttributes.BAGGAGE_SERVICE, service)
                    .build();
            ctx = context.with(baggage);
        } catch (Throwable t) {
            SdkErrors.record("propagator", t); // fall back to the unmodified context
        }
        delegate.inject(ctx, carrier, setter);
    }

    @Override
    public <C> Context extract(Context context, C carrier, TextMapGetter<C> getter) {
        return delegate.extract(context, carrier, getter);
    }

    @Override
    public String toString() {
        return "TrooperBaggagePropagator{" + delegate + "}";
    }
}
