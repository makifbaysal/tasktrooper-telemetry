package io.tasktrooper.telemetry.quarkus;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.propagation.TextMapGetter;
import io.opentelemetry.context.propagation.TextMapPropagator;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

/**
 * Simulates SmallRye Reactive Messaging: the connector extracts the parent context from the message headers with the
 * configured propagator, then starts (and immediately ends) a CONSUMER span from it.
 */
class QuarkusAttemptTest {
    private static final AttributeKey<Long> ATTEMPT = AttributeKey.longKey("trooper.consumer.attempt");

    /** Rabbit-style getter: header values are objects rendered with toString(). */
    private static final TextMapGetter<Map<String, Object>> RABBIT = new TextMapGetter<>() {
        @Override
        public Iterable<String> keys(Map<String, Object> carrier) {
            return carrier.keySet();
        }

        @Override
        public String get(Map<String, Object> carrier, String key) {
            Object v = carrier == null ? null : carrier.get(key);
            return v == null ? null : v.toString();
        }
    };

    /** Kafka-style getter: header bytes decoded as UTF-8. */
    private static final TextMapGetter<Map<String, byte[]>> KAFKA = new TextMapGetter<>() {
        @Override
        public Iterable<String> keys(Map<String, byte[]> carrier) {
            return carrier.keySet();
        }

        @Override
        public String get(Map<String, byte[]> carrier, String key) {
            byte[] v = carrier == null ? null : carrier.get(key);
            return v == null ? null : new String(v, StandardCharsets.UTF_8);
        }
    };

    private static <C> Long consume(C headers, TextMapGetter<C> getter) {
        InMemorySpanExporter exporter = InMemorySpanExporter.create();
        try (SdkTracerProvider tp = SdkTracerProvider.builder()
                .addSpanProcessor(new TrooperOtelProducers().trooperSpanProcessor())
                .addSpanProcessor(SimpleSpanProcessor.create(exporter))
                .build()) {
            TextMapPropagator propagator = new TrooperPropagatorProvider().getPropagator(null);
            Context parent = propagator.extract(Context.root(), headers, getter);
            Tracer tracer = tp.get("io.smallrye.reactive.messaging");
            tracer.spanBuilder("orders receive").setSpanKind(SpanKind.CONSUMER).setParent(parent)
                    .setAttribute("messaging.destination.name", "orders").startSpan().end();
            List<SpanData> spans = exporter.getFinishedSpanItems();
            assertThat(spans).hasSize(1);
            return spans.get(0).getAttributes().get(ATTEMPT);
        }
    }

    private static Map<String, Object> rabbit(Object... kv) {
        Map<String, Object> m = new HashMap<>();
        for (int i = 0; i < kv.length; i += 2) m.put((String) kv[i], kv[i + 1]);
        return m;
    }

    @Test
    void ttAttemptWinsOverEverything() {
        assertThat(consume(rabbit("tt-attempt", "4", "x-delivery-count", 9L), RABBIT)).isEqualTo(4L);
        Map<String, byte[]> kafka = new HashMap<>();
        kafka.put("tt-attempt", "3".getBytes(StandardCharsets.UTF_8));
        kafka.put("kafka_deliveryAttempt", new byte[] {0, 0, 0, 7});
        assertThat(consume(kafka, KAFKA)).isEqualTo(3L);
    }

    @Test
    void kafkaFallbacks() {
        Map<String, byte[]> kafka = new HashMap<>();
        kafka.put("kafka_deliveryAttempt", new byte[] {0, 0, 0, 2});
        assertThat(consume(kafka, KAFKA)).isEqualTo(2L);

        Map<String, byte[]> delayed = new HashMap<>();
        delayed.put("delayed-retry-count", "2".getBytes(StandardCharsets.UTF_8));
        assertThat(consume(delayed, KAFKA)).isEqualTo(3L);
    }

    @Test
    void rabbitFallbacks() {
        assertThat(consume(rabbit("x-delivery-count", 2L), RABBIT)).isEqualTo(3L);
        List<Map<String, Object>> xDeath = List.of(Map.of("count", 2L, "reason", "rejected"), Map.of("count", 1L, "reason", "expired"));
        assertThat(consume(rabbit("x-death", xDeath), RABBIT)).isEqualTo(4L);
    }

    @Test
    void noHeadersNoAttempt() {
        assertThat(consume(rabbit(), RABBIT)).isNull();
        assertThat(consume(rabbit("tt-attempt", "garbage"), RABBIT)).isNull();
    }

    @Test
    void nestedExtractionDoesNotInheritOuterAttempt() {
        TextMapPropagator p = new TrooperPropagatorProvider().getPropagator(null);
        Context outer = p.extract(Context.root(), rabbit("tt-attempt", "5"), RABBIT);
        assertThat(TrooperAttemptPropagator.attemptFrom(outer)).isEqualTo(5L);
        Context inner = p.extract(outer, rabbit(), RABBIT);
        assertThat(TrooperAttemptPropagator.attemptFrom(inner)).isNull();
    }

    @Test
    void neverThrows() {
        TextMapGetter<Object> broken = new TextMapGetter<>() {
            @Override
            public Iterable<String> keys(Object carrier) {
                throw new IllegalStateException("boom");
            }

            @Override
            public String get(Object carrier, String key) {
                if ("tt-attempt".equals(key)) throw new IllegalStateException("boom");
                return null;
            }
        };
        TextMapPropagator p = TrooperAttemptPropagator.wrap(TextMapPropagator.noop());
        assertThat(p.extract(Context.root(), new Object(), broken)).isNotNull();
        assertThat(p.extract(Context.root(), null, broken)).isNotNull();
    }

    @Test
    void injectStillAddsServiceBaggage() {
        Map<String, String> out = new HashMap<>();
        TrooperAttemptPropagator.wrap(io.tasktrooper.telemetry.core.TrooperBaggagePropagator.create("inventory"))
                .inject(Context.root(), out, Map::put);
        assertThat(out).containsEntry("baggage", "tt.svc=inventory");
    }
}
