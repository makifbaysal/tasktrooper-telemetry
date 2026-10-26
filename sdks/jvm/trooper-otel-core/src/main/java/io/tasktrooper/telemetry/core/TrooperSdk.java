package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Manual SDK initialization for apps without the javaagent or a framework integration (plain Java, Ktor). Requires
 * {@code opentelemetry-sdk-extension-autoconfigure} and {@code opentelemetry-exporter-otlp} on the classpath
 * ({@code trooper-kotlin} brings both).
 */
public final class TrooperSdk {
    private static final Logger LOG = Logger.getLogger(TrooperSdk.class.getName());

    private TrooperSdk() {}

    /** Builds and registers the global SDK. Never throws: on failure returns {@link OpenTelemetry#noop()}. */
    public static OpenTelemetry init(TrooperConfig config) {
        return init(config, true);
    }

    public static OpenTelemetry init(TrooperConfig config, boolean setGlobal) {
        try {
            io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdkBuilder builder = AutoConfiguredOpenTelemetrySdk.builder();
            TrooperCustomizations.apply(builder, config);
            if (setGlobal) {
                builder.setResultAsGlobal();
            }
            OpenTelemetrySdk sdk = builder.build().getOpenTelemetrySdk();
            SdkErrors.bind(sdk);
            Runtime.getRuntime().addShutdownHook(new Thread(sdk::close, "trooper-sdk-shutdown"));
            return sdk;
        } catch (Throwable t) {
            LOG.log(Level.WARNING, "TaskTrooper: SDK initialization failed, telemetry disabled", t);
            SdkErrors.record("init", t);
            return OpenTelemetry.noop();
        }
    }
}
