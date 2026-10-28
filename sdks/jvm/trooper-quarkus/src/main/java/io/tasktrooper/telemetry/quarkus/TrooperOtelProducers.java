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
import io.tasktrooper.telemetry.core.profiling.ProfilerBootstrap;
import jakarta.enterprise.inject.Produces;
import jakarta.inject.Singleton;
import org.eclipse.microprofile.config.Config;

/** CDI beans picked up by quarkus-opentelemetry: the trooper span processor and sampler. */
@Singleton
public class TrooperOtelProducers {

    @Produces
    @Singleton
    public SpanProcessor trooperSpanProcessor() {
        // One bean: the trooper conventions + trooper.consumer.attempt for SmallRye Reactive Messaging consumers.
        return SpanProcessor.composite(new TrooperSpanProcessor(), new TrooperAttemptSpanProcessor());
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
    void bindSdkErrors(@Observes io.quarkus.runtime.StartupEvent event, Instance<OpenTelemetry> openTelemetry, Config config) {
        try {
            if (openTelemetry.isResolvable()) {
                SdkErrors.bind(openTelemetry.get());
            }
        } catch (Throwable t) {
            SdkErrors.record("quarkus_init", t);
        }
        startProfiler(config);
    }

    /**
     * {@code trooper.profiling.enabled=true} (or {@code TROOPER_PROFILING=true}): JFR profiler, JVM mode only (native
     * images have no JFR streaming; the profiler then logs and stays off). Uploads go to the DSN.
     */
    static void startProfiler(Config config) {
        try {
            TrooperConfig cfg = TrooperConfig.fromLookup(key -> {
                String v = config.getOptionalValue(key, String.class).orElse(null);
                if (v == null && TrooperConfig.SERVICE.equals(key)) {
                    v = config.getOptionalValue("quarkus.application.name", String.class).orElse(null);
                }
                return v;
            });
            ProfilerBootstrap.startIfEnabled(cfg, io.opentelemetry.sdk.resources.Resource.empty(), null);
        } catch (Throwable t) {
            SdkErrors.record("profiler", t);
        }
    }
}
