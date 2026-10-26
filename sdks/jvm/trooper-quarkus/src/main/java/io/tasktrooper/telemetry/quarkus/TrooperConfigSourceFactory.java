package io.tasktrooper.telemetry.quarkus;

import io.smallrye.config.ConfigSourceContext;
import io.smallrye.config.ConfigSourceFactory;
import io.smallrye.config.ConfigValue;
import io.smallrye.config.PropertiesConfigSource;
import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperConfig;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.OptionalInt;
import org.eclipse.microprofile.config.spi.ConfigSource;

/**
 * Maps {@code trooper.*} / {@code TROOPER_*} to {@code quarkus.otel.*}. Ordinal 150: below application.properties (250),
 * system properties and environment variables, so explicit {@code quarkus.otel.*} settings win.
 */
public class TrooperConfigSourceFactory implements ConfigSourceFactory {
    public static final int ORDINAL = 150;
    public static final String NAME = "TaskTrooperConfigSource";

    @Override
    public Iterable<ConfigSource> getConfigSources(ConfigSourceContext context) {
        try {
            TrooperConfig cfg = TrooperConfig.fromLookup(key -> {
                String v = value(context, key);
                if (v == null && TrooperConfig.SERVICE.equals(key)) {
                    v = value(context, "quarkus.application.name");
                }
                return v;
            });
            return List.of(new PropertiesConfigSource(toQuarkus(cfg), NAME, ORDINAL));
        } catch (Throwable t) {
            SdkErrors.record("quarkus_config", t);
            return List.of();
        }
    }

    @Override
    public OptionalInt getPriority() {
        return OptionalInt.of(ORDINAL);
    }

    private static String value(ConfigSourceContext context, String key) {
        ConfigValue v = context.getValue(key);
        return v == null ? null : v.getValue();
    }

    /** {@code otel.x} → {@code quarkus.otel.x}; propagators include the trooper baggage propagator. */
    static Map<String, String> toQuarkus(TrooperConfig cfg) {
        Map<String, String> out = new LinkedHashMap<>();
        cfg.toOtelProperties().forEach((k, v) -> {
            if (k.equals("otel.instrumentation.messaging.experimental.capture-headers")) {
                return; // not a Quarkus property
            }
            out.put("quarkus." + k, v);
        });
        if (cfg.enabled()) {
            out.put("quarkus.otel.propagators", "tracecontext," + TrooperPropagatorProvider.NAME);
        }
        return out;
    }
}
