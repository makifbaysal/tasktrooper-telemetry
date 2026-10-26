package io.tasktrooper.telemetry.core;

import io.opentelemetry.sdk.common.export.RetryPolicy;
import io.opentelemetry.sdk.logs.export.LogRecordExporter;
import io.opentelemetry.sdk.metrics.export.MetricExporter;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.time.Duration;

/**
 * Applies the contract retry policy (contracts.md §2: exponential backoff 1s → 60s with jitter on 429/5xx) to OTLP/HTTP
 * exporters. Exporter classes are optional at runtime, so any linkage problem leaves the exporter untouched.
 */
public final class OtlpRetry {
    /** OTel caps attempts at 5 (initial + 4 retries); backoff 1s, 2s, 4s, 8s ... capped at 60s, jittered by the SDK. */
    public static final RetryPolicy POLICY = RetryPolicy.builder()
            .setMaxAttempts(5)
            .setInitialBackoff(Duration.ofSeconds(1))
            .setMaxBackoff(Duration.ofSeconds(60))
            .setBackoffMultiplier(2.0)
            .build();

    private OtlpRetry() {}

    public static SpanExporter span(SpanExporter exporter) {
        try {
            if (exporter instanceof io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter e) {
                return e.toBuilder().setRetryPolicy(POLICY).build();
            }
        } catch (NoClassDefFoundError ignored) {
            // exporter-otlp not on the classpath: nothing to configure
        } catch (Throwable t) {
            SdkErrors.record("exporter", t);
        }
        return exporter;
    }

    public static MetricExporter metric(MetricExporter exporter) {
        try {
            if (exporter instanceof io.opentelemetry.exporter.otlp.http.metrics.OtlpHttpMetricExporter e) {
                return e.toBuilder().setRetryPolicy(POLICY).build();
            }
        } catch (NoClassDefFoundError ignored) {
            // see above
        } catch (Throwable t) {
            SdkErrors.record("exporter", t);
        }
        return exporter;
    }

    public static LogRecordExporter log(LogRecordExporter exporter) {
        try {
            if (exporter instanceof io.opentelemetry.exporter.otlp.http.logs.OtlpHttpLogRecordExporter e) {
                return e.toBuilder().setRetryPolicy(POLICY).build();
            }
        } catch (NoClassDefFoundError ignored) {
            // see above
        } catch (Throwable t) {
            SdkErrors.record("exporter", t);
        }
        return exporter;
    }
}
