package io.tasktrooper.telemetry.core;

import io.opentelemetry.context.propagation.TextMapPropagator;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizer;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Registers every TaskTrooper customization on an OTel {@link AutoConfigurationCustomizer}. Shared by the javaagent
 * extension, the Spring Boot starter and {@link TrooperSdk}.
 */
public final class TrooperCustomizations {
    private static final Logger LOG = Logger.getLogger(TrooperCustomizations.class.getName());

    private TrooperCustomizations() {}

    public static void apply(AutoConfigurationCustomizer customizer, TrooperConfig config) {
        customizer.addPropertiesSupplier(config::toOtelProperties);
        if (!config.enabled()) {
            return;
        }
        customizer.addTracerProviderCustomizer((builder, props) -> builder.addSpanProcessor(new TrooperSpanProcessor()));
        customizer.addSamplerCustomizer((sampler, props) -> TrooperSampler.wrap(sampler, effectiveRate(props)));
        customizer.addPropagatorCustomizer((propagator, props) -> withCallerBaggage(propagator, serviceName(config, props)));
        customizer.addSpanExporterCustomizer((exporter, props) -> OtlpRetry.span(exporter));
        customizer.addMetricExporterCustomizer((exporter, props) -> OtlpRetry.metric(exporter));
        customizer.addLogRecordExporterCustomizer((exporter, props) -> OtlpRetry.log(exporter));
        if (config.dsn() == null) {
            LOG.log(Level.INFO, "TaskTrooper: no DSN configured (TROOPER_DSN / trooper.dsn); using the OTel exporter settings as-is");
        }
    }

    static TextMapPropagator withCallerBaggage(TextMapPropagator propagator, String service) {
        if (service != null && TrooperBaggagePropagator.handlesBaggage(propagator)) {
            return TrooperBaggagePropagator.wrap(propagator, service);
        }
        return propagator;
    }

    static String serviceName(TrooperConfig config, ConfigProperties props) {
        String fromProps = props.getString("otel.service.name");
        if (fromProps != null && !fromProps.isBlank()) {
            return fromProps;
        }
        String fromResource = props.getMap("otel.resource.attributes").get("service.name");
        if (fromResource != null && !fromResource.isBlank()) {
            return fromResource;
        }
        return config.service();
    }

    /** Effective head-sampling ratio from the resolved OTel sampler settings. */
    static double effectiveRate(ConfigProperties props) {
        String sampler = props.getString("otel.traces.sampler", "parentbased_always_on");
        return switch (sampler) {
            case "always_off", "parentbased_always_off" -> 0.0;
            case "traceidratio", "parentbased_traceidratio" -> props.getDouble("otel.traces.sampler.arg", 1.0);
            default -> 1.0;
        };
    }

    /** Builds the default trooper sampler for a fixed rate (used where autoconfigure isn't available, e.g. Quarkus). */
    public static Sampler sampler(TrooperConfig config) {
        return TrooperSampler.create(config.sampleRate());
    }
}
