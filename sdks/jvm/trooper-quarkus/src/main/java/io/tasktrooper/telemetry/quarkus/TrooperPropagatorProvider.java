package io.tasktrooper.telemetry.quarkus;

import io.opentelemetry.context.propagation.TextMapPropagator;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigurablePropagatorProvider;
import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperBaggagePropagator;
import org.eclipse.microprofile.config.Config;
import org.eclipse.microprofile.config.ConfigProvider;

/**
 * {@code quarkus.otel.propagators=tracecontext,trooper}: W3C baggage that also writes {@code tt.svc=<service>} on
 * outgoing calls; extraction also reads the message attempt ({@code tt-attempt}, {@code x-delivery-count} ...) for
 * {@link TrooperAttemptSpanProcessor}.
 */
public class TrooperPropagatorProvider implements ConfigurablePropagatorProvider {
    public static final String NAME = "trooper";

    @Override
    public TextMapPropagator getPropagator(ConfigProperties config) {
        return TrooperAttemptPropagator.wrap(TrooperBaggagePropagator.create(serviceName()));
    }

    static String serviceName() {
        try {
            Config c = ConfigProvider.getConfig();
            return c.getOptionalValue("quarkus.otel.service.name", String.class)
                    .or(() -> c.getOptionalValue("trooper.service", String.class))
                    .or(() -> c.getOptionalValue("quarkus.application.name", String.class))
                    .orElse(null);
        } catch (Throwable t) {
            SdkErrors.record("quarkus_propagator", t);
            return null;
        }
    }

    @Override
    public String getName() {
        return NAME;
    }
}
