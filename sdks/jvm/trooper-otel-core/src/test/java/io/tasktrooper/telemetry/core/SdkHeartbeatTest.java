package io.tasktrooper.telemetry.core;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.metrics.export.PeriodicMetricReader;
import io.opentelemetry.sdk.resources.Resource;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricExporter;
import java.time.Duration;
import java.util.List;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/** trooper.sdk.heartbeat (contracts.md §4): exported without traffic, early, with identity from the resource. */
class SdkHeartbeatTest {
    @AfterEach
    void reset() {
        SdkErrors.resetForTest();
        SdkHeartbeat.earlyExportDelayMs = 5_000;
    }

    private static List<MetricData> heartbeats(InMemoryMetricExporter exporter) {
        return exporter.getFinishedMetricItems().stream().filter(m -> m.getName().equals(SdkHeartbeat.METRIC)).toList();
    }

    @Test
    void exportedByEarlyFlushWithResourceServiceName() throws Exception {
        SdkHeartbeat.earlyExportDelayMs = 200;
        InMemoryMetricExporter exporter = InMemoryMetricExporter.create();
        // a 60 s interval (OTel default): only the early flush can deliver the heartbeat within the test
        try (SdkMeterProvider mp = SdkMeterProvider.builder()
                .setResource(Resource.getDefault().merge(Resource.create(Attributes.of(
                        io.opentelemetry.api.common.AttributeKey.stringKey("service.name"), "jvm-heartbeat"))))
                .registerMetricReader(PeriodicMetricReader.builder(exporter).setInterval(Duration.ofSeconds(60)).build())
                .build()) {
            SdkErrors.bind(OpenTelemetrySdk.builder().setMeterProvider(mp).build());
            long deadline = System.currentTimeMillis() + 5_000;
            while (heartbeats(exporter).isEmpty() && System.currentTimeMillis() < deadline) {
                Thread.sleep(25);
            }
            List<MetricData> hb = heartbeats(exporter);
            assertThat(hb).isNotEmpty();
            MetricData m = hb.get(0);
            assertThat(m.getInstrumentationScopeInfo().getName()).isEqualTo(SdkErrors.METER);
            assertThat(m.getResource().getAttribute(io.opentelemetry.api.common.AttributeKey.stringKey("service.name")))
                    .isEqualTo("jvm-heartbeat");
            var point = m.getLongGaugeData().getPoints().iterator().next();
            assertThat(point.getValue()).isEqualTo(1L);
            assertThat(point.getAttributes().isEmpty()).isTrue();
        }
    }

    @Test
    void nothingExportedWithoutMetricReader() throws Exception {
        SdkHeartbeat.earlyExportDelayMs = 50;
        // otel.metrics.exporter=none → meter provider without readers: the gauge and the early flush are no-ops
        try (SdkMeterProvider mp = SdkMeterProvider.builder().build()) {
            SdkErrors.bind(OpenTelemetrySdk.builder().setMeterProvider(mp).build());
            Thread.sleep(300);
        }
        assertThat(SdkErrors.count("heartbeat")).isZero();
    }
}
