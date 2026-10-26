package io.tasktrooper.telemetry.core;

import static org.assertj.core.api.Assertions.assertThat;

import com.sun.net.httpserver.HttpServer;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import java.net.InetSocketAddress;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** End to end: DSN → OTLP/HTTP protobuf POST /v1/traces with x-trooper-key, through OTel autoconfigure. */
class ExporterConfigTest {
    record Request(String path, String key, String contentType, int size) {}

    private HttpServer server;
    private final List<Request> requests = new CopyOnWriteArrayList<>();

    @BeforeEach
    void start() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", ex -> {
            byte[] body = ex.getRequestBody().readAllBytes();
            requests.add(new Request(ex.getRequestURI().getPath(), ex.getRequestHeaders().getFirst("x-trooper-key"),
                    ex.getRequestHeaders().getFirst("Content-Type"), body.length));
            ex.sendResponseHeaders(200, -1);
            ex.close();
        });
        server.start();
        System.setProperty("otel.metrics.exporter", "none");
        System.setProperty("otel.logs.exporter", "none");
    }

    @AfterEach
    void stop() {
        server.stop(0);
        System.clearProperty("otel.metrics.exporter");
        System.clearProperty("otel.logs.exporter");
    }

    @Test
    void exportsToDsnEndpointWithKeyHeader() {
        TrooperConfig cfg = TrooperConfig.builder()
                .dsn("http://tt_test_key@127.0.0.1:" + server.getAddress().getPort() + "/1")
                .service("orders-api")
                .environment("test")
                .build();
        OpenTelemetry otel = TrooperSdk.init(cfg, false);
        assertThat(otel).isInstanceOf(OpenTelemetrySdk.class);
        OpenTelemetrySdk sdk = (OpenTelemetrySdk) otel;

        sdk.getTracer("t").spanBuilder("GET /x").setSpanKind(SpanKind.SERVER).startSpan().end();
        sdk.getSdkTracerProvider().forceFlush().join(10, TimeUnit.SECONDS);

        assertThat(requests).isNotEmpty();
        Request r = requests.get(0);
        assertThat(r.path()).isEqualTo("/v1/traces");
        assertThat(r.key()).isEqualTo("tt_test_key");
        assertThat(r.contentType()).isEqualTo("application/x-protobuf");
        assertThat(r.size()).isPositive();

        Map<String, String> headers = new HashMap<>();
        sdk.getPropagators().getTextMapPropagator().inject(io.opentelemetry.context.Context.root(), headers, Map::put);
        assertThat(headers.get("baggage")).isEqualTo("tt.svc=orders-api");
        assertThat(sdk.getSdkTracerProvider().getSampler().getDescription()).startsWith("TrooperSampler");
        sdk.close();
    }

    @Test
    void initNeverThrows() {
        OpenTelemetry otel = TrooperSdk.init(TrooperConfig.builder().dsn("garbage").build(), false);
        assertThat(otel).isNotNull();
        if (otel instanceof OpenTelemetrySdk sdk) {
            sdk.close();
        }
    }
}
