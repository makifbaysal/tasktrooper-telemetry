package io.tasktrooper.telemetry.spring;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.tasktrooper.telemetry.core.TrooperAttributes;
import java.time.Duration;
import org.awaitility.Awaitility;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Import;
import org.springframework.test.web.reactive.server.WebTestClient;

@SpringBootTest(classes = TestApp.class, webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
        "spring.main.web-application-type=reactive",
        "trooper.dsn=http://tt_dev_ingest_key@localhost:4318/1",
        "trooper.service=orders-api",
        "otel.traces.exporter=none",
        "otel.metrics.exporter=none",
        "otel.logs.exporter=none",
        "spring.kafka.bootstrap-servers=localhost:1",
})
@Import(TestTelemetry.class)
class SpringWebFluxTest {
    @Autowired WebTestClient client;

    @Test
    void routeTemplateAndCaller() {
        TestTelemetry.EXPORTER.reset();
        client.get().uri("/v1/orders/7").header("baggage", "tt.svc=checkout").exchange().expectStatus().isOk();

        Awaitility.await().atMost(Duration.ofSeconds(5)).until(() ->
                TestTelemetry.EXPORTER.getFinishedSpanItems().stream().anyMatch(s -> s.getKind() == SpanKind.SERVER));
        SpanData server = TestTelemetry.EXPORTER.getFinishedSpanItems().stream()
                .filter(s -> s.getKind() == SpanKind.SERVER).findFirst().orElseThrow();
        assertThat(server.getAttributes().get(TrooperAttributes.ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(server.getAttributes().get(TrooperAttributes.CALLER)).isEqualTo("checkout");
    }
}
