package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.metrics.ObservableLongCounter;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.LongAdder;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Internal SDK error counter (contracts.md §4 "SDK iç metriği"): every swallowed error is counted per component and
 * exported as the cumulative counter {@code trooper.sdk.errors} (meter {@code TaskTrooper.Telemetry}, attribute
 * {@code component}).
 *
 * <p>Errors are counted in memory from the start; the counter is registered once an {@link OpenTelemetry} is known
 * ({@link #bind}: javaagent listener, {@link TrooperSdk}, Spring/Quarkus/Ktor integrations), or lazily from
 * {@link GlobalOpenTelemetry} once that has been set. Never throws.
 */
public final class SdkErrors {
    public static final String METER = "TaskTrooper.Telemetry";
    public static final String METRIC = "trooper.sdk.errors";
    public static final AttributeKey<String> COMPONENT = AttributeKey.stringKey("component");

    private static final Logger LOG = Logger.getLogger(SdkErrors.class.getName());
    private static final Map<String, LongAdder> COUNTS = new ConcurrentHashMap<>();
    private static final Object LOCK = new Object();
    private static volatile ObservableLongCounter registration;
    private static volatile OpenTelemetry boundTo;

    private SdkErrors() {}

    /** Counts one swallowed error for {@code component} (e.g. {@code span_processor}, {@code sampler}). */
    public static void record(String component, Throwable error) {
        try {
            String c = component == null || component.isBlank() ? "unknown" : component;
            COUNTS.computeIfAbsent(c, k -> new LongAdder()).increment();
            if (LOG.isLoggable(Level.FINE)) {
                LOG.log(Level.FINE, "TaskTrooper SDK error in " + c, error);
            }
            if (registration == null && GlobalOpenTelemetry.isSet()) {
                bind(GlobalOpenTelemetry.get());
            }
        } catch (Throwable ignored) {
            // the error counter itself must never fail
        }
    }

    /** Registers the counter on {@code openTelemetry}'s meter provider (re-binding moves it). Never throws. */
    public static void bind(OpenTelemetry openTelemetry) {
        if (openTelemetry == null) {
            return;
        }
        try {
            synchronized (LOCK) {
                if (boundTo == openTelemetry && registration != null) {
                    return;
                }
                ObservableLongCounter previous = registration;
                registration = openTelemetry.getMeter(METER)
                        .counterBuilder(METRIC)
                        .setDescription("Errors swallowed by the TaskTrooper SDK")
                        .setUnit("{error}")
                        .buildWithCallback(m -> COUNTS.forEach((c, n) -> m.record(n.sum(), Attributes.of(COMPONENT, c))));
                boundTo = openTelemetry;
                if (previous != null) {
                    previous.close();
                }
            }
        } catch (Throwable t) {
            LOG.log(Level.FINE, "TaskTrooper: cannot register " + METRIC, t);
        }
    }

    /** Current count for a component (tests / diagnostics). */
    public static long count(String component) {
        LongAdder a = COUNTS.get(component);
        return a == null ? 0 : a.sum();
    }

    /** Clears counts and the registration (tests only). */
    static void resetForTest() {
        synchronized (LOCK) {
            COUNTS.clear();
            if (registration != null) {
                registration.close();
            }
            registration = null;
            boundTo = null;
        }
    }
}
