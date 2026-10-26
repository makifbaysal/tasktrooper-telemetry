package io.tasktrooper.telemetry.spring;

import static io.tasktrooper.telemetry.spring.TestTelemetry.span;
import static org.assertj.core.api.Assertions.assertThat;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.tasktrooper.telemetry.core.TrooperAttributes;
import java.nio.ByteBuffer;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.web.servlet.AutoConfigureMockMvc;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Import;
import org.springframework.core.env.Environment;
import org.springframework.test.web.servlet.MockMvc;

@SpringBootTest(classes = TestApp.class, properties = {
        "trooper.dsn=http://tt_dev_ingest_key@localhost:4318/1",
        "trooper.service=orders-api",
        "trooper.environment=test",
        "otel.traces.exporter=none",
        "otel.metrics.exporter=none",
        "otel.logs.exporter=none",
        "spring.kafka.bootstrap-servers=localhost:1",
})
@AutoConfigureMockMvc
@Import(TestTelemetry.class)
class SpringMvcTest {
    @Autowired MockMvc mvc;
    @Autowired Environment environment;
    @Autowired OpenTelemetry otel;
    @Autowired TestApp.OrderProjector projector;

    @BeforeEach
    void reset() {
        TestTelemetry.EXPORTER.reset();
    }

    @Test
    void mapsTrooperPropertiesToOtel() {
        assertThat(environment.getProperty("otel.exporter.otlp.endpoint")).isEqualTo("http://localhost:4318");
        assertThat(environment.getProperty("otel.exporter.otlp.headers")).isEqualTo("x-trooper-key=tt_dev_ingest_key");
        assertThat(environment.getProperty("otel.exporter.otlp.protocol")).isEqualTo("http/protobuf");
        assertThat(environment.getProperty("otel.service.name")).isEqualTo("orders-api");
        // explicit otel.* wins over the trooper defaults
        assertThat(environment.getProperty("otel.traces.exporter")).isEqualTo("none");
    }

    @Test
    void routeTemplateEntryRouteAndCaller() throws Exception {
        mvc.perform(get("/v1/orders/42")
                        .header("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
                        .header("baggage", "tt.svc=gateway"))
                .andExpect(status().isOk());

        SpanData server = TestTelemetry.EXPORTER.getFinishedSpanItems().stream()
                .filter(s -> s.getKind() == SpanKind.SERVER).findFirst().orElseThrow();
        assertThat(server.getAttributes().get(TrooperAttributes.ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(server.getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(server.getAttributes().get(TrooperAttributes.CALLER)).isEqualTo("gateway");
        assertThat(server.getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isEqualTo(1.0);
        assertThat(span("load-order").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(server.getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("GET");
        assertThat(span("load-order").getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("GET");
    }

    @Test
    void sampleRateFromUpstreamTraceState() throws Exception {
        mvc.perform(get("/v1/orders/7")
                        .header("traceparent", "00-1af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
                        .header("tracestate", "tt=r:0.25,other=x"))
                .andExpect(status().isOk());
        SpanData server = TestTelemetry.EXPORTER.getFinishedSpanItems().stream()
                .filter(s -> s.getKind() == SpanKind.SERVER).findFirst().orElseThrow();
        assertThat(server.getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isEqualTo(0.25);
        assertThat(server.getSpanContext().getTraceState().get("tt")).isEqualTo("r:0.25");
    }

    @Test
    void rootSpanWritesTraceState() {
        Span root = otel.getTracer("test").spanBuilder("job").startSpan();
        assertThat(root.getSpanContext().getTraceState().get("tt")).isEqualTo("r:1");
        root.end();
    }

    @Test
    void outgoingBaggageCarriesServiceName() {
        java.util.Map<String, String> headers = new java.util.HashMap<>();
        otel.getPropagators().getTextMapPropagator().inject(io.opentelemetry.context.Context.root(), headers, java.util.Map::put);
        assertThat(headers.get("baggage")).contains("tt.svc=orders-api");
    }

    @Test
    void kafkaListenerProcessNameAndAttempt() {
        ConsumerRecord<String, String> record = new ConsumerRecord<>("orders.created", 0, 0L, "k", "v");
        record.headers().add("kafka_deliveryAttempt", ByteBuffer.allocate(4).putInt(2).array());
        Span consumer = otel.getTracer("test").spanBuilder("orders.created process").setSpanKind(SpanKind.CONSUMER).startSpan();
        try (Scope ignored = consumer.makeCurrent()) {
            projector.onOrder(record);
        }
        consumer.end();
        SpanData s = span("orders.created process");
        assertThat(s.getAttributes().get(TrooperAttributes.PROCESS)).isEqualTo("order-projector");
        assertThat(s.getAttributes().get(TrooperAttributes.CONSUMER_ATTEMPT)).isEqualTo(2L);
        assertThat(s.getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("order-projector");
        assertThat(s.getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("CONSUME");
        assertThat(new String(TestApp.OrderProjector.lastRetryHeaders.lastHeader("tt-attempt").value())).isEqualTo("3");
    }

    @Test
    void ttAttemptHeaderWins() {
        ConsumerRecord<String, String> record = new ConsumerRecord<>("orders.created.retry", 0, 0L, "k", "v");
        record.headers().add("kafka_deliveryAttempt", ByteBuffer.allocate(4).putInt(1).array());
        record.headers().add("tt-attempt", "4".getBytes());
        Span consumer = otel.getTracer("test").spanBuilder("retry process").setSpanKind(SpanKind.CONSUMER).startSpan();
        try (Scope ignored = consumer.makeCurrent()) {
            projector.onOrder(record);
        }
        consumer.end();
        assertThat(span("retry process").getAttributes().get(TrooperAttributes.CONSUMER_ATTEMPT)).isEqualTo(4L);
        assertThat(new String(TestApp.OrderProjector.lastRetryHeaders.lastHeader("tt-attempt").value())).isEqualTo("5");
    }
}
