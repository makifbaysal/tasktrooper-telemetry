package io.tasktrooper.telemetry.core;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.util.Map;
import org.junit.jupiter.api.Test;

class DsnAndConfigTest {

    @Test
    void parsesLocalDsn() {
        Dsn dsn = Dsn.parse("http://tt_dev_ingest_key@localhost:4318/1");
        assertThat(dsn.endpoint()).isEqualTo("http://localhost:4318");
        assertThat(dsn.key()).isEqualTo("tt_dev_ingest_key");
        assertThat(dsn.projectId()).isEqualTo("1");
        assertThat(dsn.toString()).doesNotContain("tt_dev_ingest_key");
    }

    @Test
    void parsesHttpsDsnWithoutPort() {
        Dsn dsn = Dsn.parse("https://abc123@ingest.trooper.dev/42/");
        assertThat(dsn.endpoint()).isEqualTo("https://ingest.trooper.dev");
        assertThat(dsn.key()).isEqualTo("abc123");
        assertThat(dsn.projectId()).isEqualTo("42");
    }

    @Test
    void rejectsMalformedDsn() {
        assertThatThrownBy(() -> Dsn.parse("https://ingest.trooper.dev/1")).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> Dsn.parse("ftp://k@host/1")).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> Dsn.parse("")).isInstanceOf(IllegalArgumentException.class);
    }

    @Test
    void invalidDsnIsIgnoredNotThrown() {
        TrooperConfig cfg = TrooperConfig.fromLookup(Map.of("trooper.dsn", "not a dsn", "trooper.sample-rate", "abc")::get);
        assertThat(cfg.dsn()).isNull();
        assertThat(cfg.sampleRate()).isEqualTo(1.0);
    }

    @Test
    void mapsToOtelProperties() {
        TrooperConfig cfg = TrooperConfig.fromLookup(Map.of(
                "trooper.dsn", "http://key1@localhost:4318/1",
                "trooper.service", "orders-api",
                "trooper.environment", "production",
                "trooper.version", "abc123",
                "trooper.sample-rate", "0.25")::get);
        Map<String, String> p = cfg.toOtelProperties();
        assertThat(p)
                .containsEntry("otel.exporter.otlp.endpoint", "http://localhost:4318")
                .containsEntry("otel.exporter.otlp.protocol", "http/protobuf")
                .containsEntry("otel.exporter.otlp.headers", "x-trooper-key=key1")
                .containsEntry("otel.service.name", "orders-api")
                .containsEntry("otel.resource.attributes", "deployment.environment.name=production,service.version=abc123")
                .containsEntry("otel.traces.sampler", "parentbased_traceidratio")
                .containsEntry("otel.traces.sampler.arg", "0.25");
    }

    @Test
    void disabledTurnsSdkOff() {
        TrooperConfig cfg = TrooperConfig.builder().enabled(false).build();
        assertThat(cfg.toOtelProperties()).containsExactly(Map.entry("otel.sdk.disabled", "true"));
    }

    @Test
    void clampsSampleRateAndBuildsEnvNames() {
        assertThat(TrooperConfig.builder().sampleRate(3).build().sampleRate()).isEqualTo(1.0);
        assertThat(TrooperConfig.builder().sampleRate(-1).build().sampleRate()).isEqualTo(0.0);
        assertThat(TrooperConfig.envName("trooper.sample-rate")).isEqualTo("TROOPER_SAMPLE_RATE");
    }

    @Test
    void normalizesRouteTemplates() {
        assertThat(RouteTemplates.normalize("/users/{id}")).isEqualTo("/users/:id");
        assertThat(RouteTemplates.normalize("/users/{id:[0-9]{1,3}}/orders/{orderId}")).isEqualTo("/users/:id/orders/:orderId");
        assertThat(RouteTemplates.normalize("/files/{path...}")).isEqualTo("/files/*path");
        assertThat(RouteTemplates.normalize("/v1/orders/:id")).isEqualTo("/v1/orders/:id");
        assertThat(RouteTemplates.normalize("items/")).isEqualTo("/items");
        assertThat(RouteTemplates.normalize("/")).isEqualTo("/");
        assertThat(RouteTemplates.join("/api/", "{id}")).isEqualTo("/api/:id");
        assertThat(RouteTemplates.join("api", null)).isEqualTo("/api");
    }

    @Test
    void decodesKafkaDeliveryAttempt() {
        assertThat(TrooperConsumer.kafkaDeliveryAttempt(new byte[] {0, 0, 0, 3})).isEqualTo(3L);
        assertThat(TrooperConsumer.kafkaDeliveryAttempt("7".getBytes())).isEqualTo(7L);
        assertThat(TrooperSpanProcessor.deliveryAttempt(java.util.List.of(new String(new byte[] {0, 0, 0, 2})))).isEqualTo(2L);
        assertThat(TrooperSpanProcessor.deliveryAttempt(java.util.List.of("5"))).isEqualTo(5L);
        assertThat(TrooperSpanProcessor.deliveryAttempt(null)).isNull();
    }

    @Test
    void readsProfilingAndLlmSettings() {
        TrooperConfig off = TrooperConfig.fromLookup(Map.<String, String>of()::get);
        assertThat(off.profilingEnabled()).isFalse();
        assertThat(off.profilingPeriodSeconds()).isEqualTo(60);
        assertThat(off.llmCaptureContent()).isNull();
        assertThat(off.toOtelProperties()).doesNotContainKey("otel.instrumentation.genai.capture-message-content");

        // TROOPER_PROFILING maps to the short key, -Dtrooper.profiling.enabled to the long one
        assertThat(TrooperConfig.envName(TrooperConfig.PROFILING)).isEqualTo("TROOPER_PROFILING");
        assertThat(TrooperConfig.envName(TrooperConfig.LLM_CAPTURE_CONTENT)).isEqualTo("TROOPER_LLM_CAPTURE_CONTENT");
        TrooperConfig on = TrooperConfig.fromLookup(Map.of(
                "trooper.profiling", "true",
                "trooper.profiling.types", " CPU, alloc_space ,",
                "trooper.profiling.period", "1",
                "trooper.llm.capture-content", "true")::get);
        assertThat(on.profilingEnabled()).isTrue();
        assertThat(on.profilingTypes()).containsExactly("cpu", "alloc_space");
        assertThat(on.profilingPeriodSeconds()).isEqualTo(5); // clamped
        assertThat(on.llmCaptureContent()).isTrue();
        assertThat(on.toOtelProperties()).containsEntry("otel.instrumentation.genai.capture-message-content", "true");

        TrooperConfig explicitOff = TrooperConfig.fromLookup(Map.of(
                "trooper.profiling.enabled", "false", "trooper.profiling", "true",
                "trooper.profiling.period", "abc", "trooper.llm.capture-content", "false")::get);
        assertThat(explicitOff.profilingEnabled()).isFalse(); // the long key wins
        assertThat(explicitOff.profilingPeriodSeconds()).isEqualTo(60);
        assertThat(explicitOff.llmCaptureContent()).isFalse();
    }
}
