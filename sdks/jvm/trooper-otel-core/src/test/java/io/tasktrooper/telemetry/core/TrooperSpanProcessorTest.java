package io.tasktrooper.telemetry.core;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.baggage.Baggage;
import io.opentelemetry.api.baggage.propagation.W3CBaggagePropagator;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.TraceFlags;
import io.opentelemetry.api.trace.TraceState;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import io.opentelemetry.context.propagation.TextMapGetter;
import io.opentelemetry.context.propagation.TextMapPropagator;
import io.opentelemetry.context.propagation.TextMapSetter;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

class TrooperSpanProcessorTest {
    private InMemorySpanExporter exporter;
    private SdkTracerProvider provider;
    private TrooperSpanProcessor processor;
    private Tracer tracer;

    @BeforeEach
    void setUp() {
        exporter = InMemorySpanExporter.create();
        processor = new TrooperSpanProcessor();
        provider = SdkTracerProvider.builder()
                .setSampler(TrooperSampler.create(0.5 + 0.5)) // rate 1.0
                .addSpanProcessor(processor)
                .addSpanProcessor(SimpleSpanProcessor.create(exporter))
                .build();
        tracer = provider.get("test");
    }

    @AfterEach
    void tearDown() {
        provider.close();
    }

    private SpanData span(String name) {
        return exporter.getFinishedSpanItems().stream().filter(s -> s.getName().equals(name)).findFirst().orElseThrow();
    }

    @Test
    void routeTemplateAndEntryRouteOnAllSpans() {
        Span server = tracer.spanBuilder("GET").setSpanKind(SpanKind.SERVER).startSpan();
        Span early;
        try (Scope ignored = server.makeCurrent()) {
            early = tracer.spanBuilder("filter").startSpan(); // starts before the route is known
            server.setAttribute("http.route", "/v1/orders/{id}");
            Span child = tracer.spanBuilder("db").setSpanKind(SpanKind.CLIENT).startSpan();
            try (Scope s2 = child.makeCurrent()) {
                tracer.spanBuilder("grandchild").startSpan().end();
            }
            child.end();
            early.end();
        }
        server.end();

        assertThat(span("GET").getAttributes().get(TrooperAttributes.ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(span("GET").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(span("db").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(span("grandchild").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(span("filter").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/orders/:id");
        assertThat(span("GET").getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isEqualTo(1.0);
        assertThat(span("db").getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isNull();
        assertThat(processor.trackedSpans()).isZero();
    }

    @Test
    void callerFromBaggageOnRemoteParentedServerSpan() {
        // Upstream service injects headers with the trooper propagator.
        TextMapPropagator upstream = TrooperBaggagePropagator.create("gateway");
        Map<String, String> headers = new HashMap<>();
        SpanContext remote = SpanContext.createFromRemoteParent("0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331", TraceFlags.getSampled(), TraceState.getDefault());
        upstream.inject(Context.root().with(Span.wrap(remote)), headers, MapSetter.INSTANCE);
        assertThat(headers.get("baggage")).contains("tt.svc=gateway");

        Context extracted = W3CBaggagePropagator.getInstance().extract(Context.root().with(Span.wrap(remote)), headers, MapGetter.INSTANCE);
        Span server = tracer.spanBuilder("POST").setParent(extracted).setSpanKind(SpanKind.SERVER).startSpan();
        server.end();

        SpanData s = span("POST");
        assertThat(s.getAttributes().get(TrooperAttributes.CALLER)).isEqualTo("gateway");
        assertThat(s.getParentSpanId()).isEqualTo("b7ad6b7169203331");
        assertThat(s.getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isEqualTo(1.0);
    }

    @Test
    void outgoingBaggageOverridesIncomingCaller() {
        TextMapPropagator p = TrooperBaggagePropagator.create("orders-api");
        Map<String, String> headers = new HashMap<>();
        Context ctx = Context.root().with(Baggage.builder().put("tt.svc", "gateway").put("tenant", "t1").build());
        p.inject(ctx, headers, MapSetter.INSTANCE);
        assertThat(headers.get("baggage")).contains("tt.svc=orders-api").contains("tenant=t1").doesNotContain("gateway");
    }

    @Test
    void consumerProcessAttemptAndEntryRoute() {
        Span consumer = tracer.spanBuilder("orders.created process")
                .setSpanKind(SpanKind.CONSUMER)
                .setAttribute("messaging.system", "kafka")
                .setAttribute("messaging.destination.name", "orders.created")
                .setAttribute("messaging.consumer.group.name", "order-projector")
                .setAttribute(AttributeKey.stringArrayKey("messaging.header.kafka_deliveryAttempt"), List.of(new String(new byte[] {0, 0, 0, 3})))
                .startSpan();
        try (Scope ignored = consumer.makeCurrent()) {
            tracer.spanBuilder("INSERT orders").setSpanKind(SpanKind.CLIENT).startSpan().end();
        }
        consumer.end();

        SpanData c = span("orders.created process");
        assertThat(c.getAttributes().get(TrooperAttributes.PROCESS)).isEqualTo("order-projector");
        assertThat(c.getAttributes().get(TrooperAttributes.CONSUMER_ATTEMPT)).isEqualTo(3L);
        assertThat(c.getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("order-projector");
        assertThat(span("INSERT orders").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("order-projector");
    }

    @Test
    void explicitProcessNameWins() {
        Span consumer = tracer.spanBuilder("process").setSpanKind(SpanKind.CONSUMER)
                .setAttribute("messaging.destination.name", "q1").startSpan();
        try (Scope ignored = consumer.makeCurrent()) {
            TrooperConsumer.markCurrent("invoice-worker", 2L, false);
        }
        consumer.end();
        SpanData c = span("process");
        assertThat(c.getAttributes().get(TrooperAttributes.PROCESS)).isEqualTo("invoice-worker");
        assertThat(c.getAttributes().get(TrooperAttributes.CONSUMER_ATTEMPT)).isEqualTo(2L);
        assertThat(c.getAttributes().get(TrooperAttributes.CONSUMER_DLQ)).isFalse();
    }

    @Test
    void internalRootHasNoEntryRoute() {
        Span job = tracer.spanBuilder("cron").startSpan();
        job.end();
        assertThat(span("cron").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isNull();
        assertThat(span("cron").getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isNull();
    }

    @Test
    void samplerStampsConfiguredRate() {
        try (SdkTracerProvider p = SdkTracerProvider.builder()
                .setSampler(TrooperSampler.create(0.0))
                .addSpanProcessor(SimpleSpanProcessor.create(exporter)).build()) {
            p.get("t").spanBuilder("dropped").setSpanKind(SpanKind.SERVER).startSpan().end();
        }
        assertThat(exporter.getFinishedSpanItems()).isEmpty();
        assertThat(TrooperSampler.create(0.1).getDescription()).contains("rate=0.1");
    }

    @Test
    void entryMethodOnAllSpans() {
        Span server = tracer.spanBuilder("POST").setSpanKind(SpanKind.SERVER)
                .setAttribute("http.request.method", "post").startSpan();
        try (Scope ignored = server.makeCurrent()) {
            server.setAttribute("http.route", "/v1/orders");
            tracer.spanBuilder("child").startSpan().end();
        }
        server.end();
        assertThat(span("POST").getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("POST");
        assertThat(span("child").getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("POST");
    }

    @Test
    void grpcRouteAndMethod() {
        Span server = tracer.spanBuilder("pkg.OrderService/GetOrder").setSpanKind(SpanKind.SERVER)
                .setAttribute("rpc.system", "grpc").setAttribute("rpc.service", "pkg.OrderService")
                .setAttribute("rpc.method", "GetOrder").startSpan();
        try (Scope ignored = server.makeCurrent()) {
            tracer.spanBuilder("db").setSpanKind(SpanKind.CLIENT).startSpan().end();
        }
        server.end();
        SpanData s = span("pkg.OrderService/GetOrder");
        assertThat(s.getAttributes().get(TrooperAttributes.ROUTE)).isEqualTo("/pkg.OrderService/GetOrder");
        assertThat(s.getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("GRPC");
        assertThat(span("db").getAttributes().get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/pkg.OrderService/GetOrder");
        assertThat(span("db").getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("GRPC");
    }

    @Test
    void consumerAttemptFromCapturedHeaders() {
        Span tt = tracer.spanBuilder("tt").setSpanKind(SpanKind.CONSUMER)
                .setAttribute("messaging.destination.name", "orders.retry")
                .setAttribute(AttributeKey.stringArrayKey("messaging.header.tt_attempt"), List.of("3"))
                .setAttribute(AttributeKey.stringArrayKey("messaging.header.kafka_deliveryAttempt"), List.of(new String(new byte[] {0, 0, 0, 1})))
                .startSpan();
        tt.end();
        Span rabbit = tracer.spanBuilder("rabbit").setSpanKind(SpanKind.CONSUMER)
                .setAttribute("messaging.system", "rabbitmq").setAttribute("messaging.destination.name", "q")
                .setAttribute(AttributeKey.stringArrayKey("messaging.header.x_delivery_count"), List.of("2"))
                .startSpan();
        rabbit.end();
        assertThat(span("tt").getAttributes().get(TrooperAttributes.CONSUMER_ATTEMPT)).isEqualTo(3L);
        assertThat(span("tt").getAttributes().get(TrooperAttributes.ENTRY_METHOD)).isEqualTo("CONSUME");
        assertThat(span("rabbit").getAttributes().get(TrooperAttributes.CONSUMER_ATTEMPT)).isEqualTo(3L);
    }

    @Test
    void rootSpanWritesRateToTraceState() {
        try (SdkTracerProvider p = SdkTracerProvider.builder().setSampler(TrooperSampler.create(0.25))
                .addSpanProcessor(SimpleSpanProcessor.create(exporter)).build()) {
            Span root = p.get("t").spanBuilder("job").startSpan();
            assertThat(root.getSpanContext().getTraceState().get("tt")).isEqualTo("r:0.25");
            // children inherit the trace state
            Span child = p.get("t").spanBuilder("c").setParent(Context.root().with(root)).startSpan();
            assertThat(child.getSpanContext().getTraceState().get("tt")).isEqualTo("r:0.25");
            child.end();
            root.end();
        }
        assertThat(TrooperSampler.formatTraceState(1.0)).isEqualTo("r:1");
        assertThat(TrooperSampler.formatTraceState(0.0001)).isEqualTo("r:0.0001");
        assertThat(TrooperSampler.parseTraceState("r:0.5")).isEqualTo(0.5);
        assertThat(TrooperSampler.parseTraceState("r:x")).isNull();
        assertThat(TrooperSampler.parseTraceState("r:2")).isNull();
    }

    @Test
    void remoteEntryTakesUpstreamRate() {
        SpanContext remote = SpanContext.createFromRemoteParent("0af7651916cd43dd8448eb211c80319d", "b7ad6b7169203331",
                TraceFlags.getSampled(), TraceState.builder().put("tt", "r:0.1").build());
        tracer.spanBuilder("GET up").setParent(Context.root().with(Span.wrap(remote))).setSpanKind(SpanKind.SERVER).startSpan().end();
        SpanData s = span("GET up");
        assertThat(s.getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isEqualTo(0.1);
        assertThat(s.getSpanContext().getTraceState().get("tt")).isEqualTo("r:0.1");

        SpanContext plain = SpanContext.createFromRemoteParent("0af7651916cd43dd8448eb211c80319e", "b7ad6b7169203331",
                TraceFlags.getSampled(), TraceState.getDefault());
        tracer.spanBuilder("GET plain").setParent(Context.root().with(Span.wrap(plain))).setSpanKind(SpanKind.SERVER).startSpan().end();
        assertThat(span("GET plain").getAttributes().get(TrooperAttributes.SAMPLE_RATE)).isEqualTo(1.0);
    }

    @Test
    void sdkErrorsCounter() {
        SdkErrors.resetForTest();
        io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader reader = io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader.create();
        try (io.opentelemetry.sdk.metrics.SdkMeterProvider mp = io.opentelemetry.sdk.metrics.SdkMeterProvider.builder().registerMetricReader(reader).build()) {
            SdkErrors.record("sampler", new IllegalStateException("x"));
            SdkErrors.bind(io.opentelemetry.sdk.OpenTelemetrySdk.builder().setMeterProvider(mp).build());
            SdkErrors.record("sampler", new IllegalStateException("y"));
            var metric = reader.collectAllMetrics().stream().filter(m -> m.getName().equals("trooper.sdk.errors")).findFirst().orElseThrow();
            assertThat(metric.getInstrumentationScopeInfo().getName()).isEqualTo("TaskTrooper.Telemetry");
            var point = metric.getLongSumData().getPoints().iterator().next();
            assertThat(point.getValue()).isEqualTo(2L);
            assertThat(point.getAttributes().get(SdkErrors.COMPONENT)).isEqualTo("sampler");
        } finally {
            SdkErrors.resetForTest();
        }
    }

    @Test
    void attemptHeaderHelpers() {
        Map<String, Object> headers = new HashMap<>();
        TrooperConsumer.writeAttempt(headers, TrooperConsumer.nextAttempt(2L));
        assertThat(headers).containsEntry("tt-attempt", "3");
        assertThat(TrooperConsumer.attemptFromHeaders(headers)).isEqualTo(3L);
        assertThat(TrooperConsumer.attemptFromHeaders(Map.of("x-delivery-count", 1L))).isEqualTo(2L);
        assertThat(TrooperConsumer.attemptFromHeaders(Map.of("x-death", List.of(Map.of("count", 2L), Map.of("count", 1L))))).isEqualTo(4L);
        assertThat(TrooperConsumer.attemptFromHeaders(Map.of("kafka_deliveryAttempt", new byte[] {0, 0, 0, 5}))).isEqualTo(5L);
        assertThat(TrooperConsumer.attemptValue("7".getBytes())).isEqualTo(7L);
        assertThat(TrooperConsumer.nextAttempt((Long) null)).isEqualTo(2L);
    }

    enum MapSetter implements TextMapSetter<Map<String, String>> {
        INSTANCE;
        @Override public void set(Map<String, String> carrier, String key, String value) { carrier.put(key, value); }
    }

    enum MapGetter implements TextMapGetter<Map<String, String>> {
        INSTANCE;
        @Override public Iterable<String> keys(Map<String, String> carrier) { return carrier.keySet(); }
        @Override public String get(Map<String, String> carrier, String key) { return carrier == null ? null : carrier.get(key); }
    }
}
