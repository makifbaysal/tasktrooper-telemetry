package io.tasktrooper.telemetry.quarkus;

import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.sdk.trace.SpanProcessor;
import io.tasktrooper.telemetry.core.SdkErrors;
import jakarta.enterprise.event.Observes;
import jakarta.enterprise.inject.Instance;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import io.tasktrooper.telemetry.core.TrooperConfig;
import io.tasktrooper.telemetry.core.TrooperSampler;
import io.tasktrooper.telemetry.core.TrooperSpanProcessor;
import jakarta.enterprise.inject.Produces;
import jakarta.inject.Singleton;
import org.eclipse.microprofile.config.Config;

/** CDI beans picked up by quarkus-opentelemetry: the trooper span processor and sampler. */
@Singleton
public class TrooperOtelProducers {

    @Produces
    @Singleton
    public SpanProcessor trooperSpanProcessor() {
        return new TrooperSpanProcessor();
    }

    @Produces
    @Singleton
    public Sampler trooperSampler(Config config) {
        double rate = config.getOptionalValue(TrooperConfig.SAMPLE_RATE, Double.class)
                .or(() -> config.getOptionalValue("quarkus.otel.traces.sampler.arg", Double.class))
                .orElse(1.0);
        return TrooperSampler.create(rate);
    }

    /** Registers {@code trooper.sdk.errors} on Quarkus' OpenTelemetry at startup. */
    void bindSdkErrors(@Observes io.quarkus.runtime.StartupEvent event, Instance<OpenTelemetry> openTelemetry) {
        try {
            if (openTelemetry.isResolvable()) {
                SdkErrors.bind(openTelemetry.get());
            }
        } catch (Throwable t) {
            SdkErrors.record("quarkus_init", t);
        }
    }
}
