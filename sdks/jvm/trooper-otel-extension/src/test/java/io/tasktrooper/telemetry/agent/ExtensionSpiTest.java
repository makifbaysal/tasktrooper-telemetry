package io.tasktrooper.telemetry.agent;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import io.opentelemetry.sdk.resources.Resource;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/** The SPI registrations are picked up by plain OTel autoconfigure, exactly as the javaagent does. */
class ExtensionSpiTest {

    @AfterEach
    void clear() {
        for (String k : new String[] {"trooper.dsn", "trooper.service", "trooper.environment", "trooper.version"}) {
            System.clearProperty(k);
        }
    }

    @Test
    void autoconfigurePicksUpTrooperProviders() {
        System.setProperty("trooper.dsn", "http://tt_dev_ingest_key@localhost:4318/1");
        System.setProperty("trooper.service", "billing");
        System.setProperty("trooper.environment", "staging");
        System.setProperty("trooper.version", "1.2.3");

        java.util.concurrent.atomic.AtomicReference<Resource> captured = new java.util.concurrent.atomic.AtomicReference<>();
        AutoConfiguredOpenTelemetrySdk auto = AutoConfiguredOpenTelemetrySdk.builder()
                .addPropertiesSupplier(() -> Map.of("otel.metrics.exporter", "none", "otel.logs.exporter", "none"))
                .addResourceCustomizer((r, cfg) -> { captured.set(r); return r; })
                .build();
        OpenTelemetrySdk sdk = auto.getOpenTelemetrySdk();
        try {
            Resource resource = captured.get();
            assertThat(resource.getAttribute(TrooperResourceProvider.DISTRO_NAME)).isEqualTo("tasktrooper-javaagent");
            assertThat(resource.getAttribute(TrooperResourceProvider.ENVIRONMENT)).isEqualTo("staging");
            assertThat(resource.getAttribute(TrooperResourceProvider.SERVICE_VERSION)).isEqualTo("1.2.3");
            assertThat(resource.getAttribute(io.opentelemetry.api.common.AttributeKey.stringKey("service.name"))).isEqualTo("billing");
            assertThat(sdk.getSdkTracerProvider().getSampler().getDescription()).startsWith("TrooperSampler");
            assertThat(sdk.getSdkTracerProvider().toString()).contains("TrooperSpanProcessor");
            assertThat(sdk.getSdkTracerProvider().toString()).contains("endpoint=http://localhost:4318/v1/traces");
        } finally {
            sdk.close();
        }
    }
}
