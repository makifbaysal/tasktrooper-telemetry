package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.metrics.ObservableLongGauge;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Presence signal (contracts.md §4 "SDK heartbeat"): the gauge {@code trooper.sdk.heartbeat} = 1 on meter
 * {@code TaskTrooper.Telemetry}, no attributes (identity comes from the resource), so a service shows up in the
 * Services list even without traffic.
 *
 * <p>Registered from {@link SdkErrors#bind}, i.e. by every flavour (javaagent, {@link TrooperSdk}, Spring Boot, Quarkus,
 * Ktor). It is exported by the configured metric reader at its interval (OTel default 60 s); to get the first
 * heartbeat out within 10 s, one {@code forceFlush} of the meter provider is scheduled ~5 s after binding on the JDK's
 * shared delayed executor (no thread of our own, never blocks startup). With metrics disabled
 * ({@code otel.metrics.exporter=none}) there is no reader and nothing is sent. Never throws.
 */
public final class SdkHeartbeat {
    public static final String METRIC = "trooper.sdk.heartbeat";

    private static final Logger LOG = Logger.getLogger(SdkHeartbeat.class.getName());
    private static final Object LOCK = new Object();
    /** Delay of the one-off early flush (package-private for tests). */
    static volatile long earlyExportDelayMs = 5_000;
    private static volatile ObservableLongGauge registration;
    private static volatile OpenTelemetry boundTo;

    private SdkHeartbeat() {}

    /** Registers the gauge on {@code openTelemetry} (re-binding moves it) and schedules the early flush. */
    static void bind(OpenTelemetry openTelemetry) {
        if (openTelemetry == null) {
            return;
        }
        try {
            synchronized (LOCK) {
                if (boundTo == openTelemetry && registration != null) {
                    return;
                }
                ObservableLongGauge previous = registration;
                registration = openTelemetry.getMeter(SdkErrors.METER)
                        .gaugeBuilder(METRIC)
                        .ofLongs()
                        .setDescription("Presence signal of a running TaskTrooper SDK")
                        .setUnit("1")
                        .buildWithCallback(m -> m.record(1));
                boundTo = openTelemetry;
                if (previous != null) {
                    previous.close();
                }
            }
            if (openTelemetry instanceof OpenTelemetrySdk sdk) {
                CompletableFuture.runAsync(() -> flush(sdk),
                        CompletableFuture.delayedExecutor(earlyExportDelayMs, TimeUnit.MILLISECONDS));
            }
        } catch (Throwable t) {
            SdkErrors.record("heartbeat", t);
            LOG.log(Level.FINE, "TaskTrooper: cannot register " + METRIC, t);
        }
    }

    private static void flush(OpenTelemetrySdk sdk) {
        try {
            sdk.getSdkMeterProvider().forceFlush();
        } catch (Throwable t) {
            SdkErrors.record("heartbeat", t);
        }
    }

    /** Clears the registration (tests only). */
    static void resetForTest() {
        synchronized (LOCK) {
            if (registration != null) {
                registration.close();
            }
            registration = null;
            boundTo = null;
        }
    }
}
