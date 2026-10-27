package io.tasktrooper.telemetry.core.profiling;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatCode;

import com.sun.net.httpserver.HttpServer;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import io.tasktrooper.telemetry.core.TrooperConfig;
import java.io.ByteArrayInputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.EnumSet;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.function.BooleanSupplier;
import java.util.zip.GZIPInputStream;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

class TrooperProfilerTest {
    private final BooleanSupplier originalCheck = TrooperProfiler.jfrCheck;

    @AfterEach
    void reset() {
        TrooperProfiler.stop();
        TrooperProfiler.jfrCheck = originalCheck;
    }

    private static ProfilerSettings settings(String endpoint) {
        return new ProfilerSettings(endpoint, Map.of("x-trooper-key", "k"), "svc", "test", "1", "h",
                EnumSet.allOf(ProfileType.class), Duration.ofSeconds(1), Duration.ofMillis(10), null, Duration.ZERO);
    }

    @Test
    void missingJfrIsReportedNotThrown() {
        TrooperProfiler.jfrCheck = () -> false;
        assertThat(TrooperProfiler.start(settings("http://127.0.0.1:1"))).isFalse();
        assertThat(TrooperProfiler.isRunning()).isFalse();
        assertThat(ProfileRoutes.isActive()).isFalse();

        TrooperProfiler.jfrCheck = () -> { throw new NoClassDefFoundError("jdk/jfr/FlightRecorder"); };
        assertThatCode(() -> assertThat(TrooperProfiler.start(settings("http://127.0.0.1:1"))).isFalse()).doesNotThrowAnyException();
        assertThat(TrooperProfiler.jfrAvailable()).isFalse();
        assertThatCode(TrooperProfiler::stop).doesNotThrowAnyException();
    }

    @Test
    void noEndpointMeansNoProfiler() {
        assertThat(TrooperProfiler.start(settings(null))).isFalse();
        assertThat(TrooperProfiler.start(null)).isFalse();
    }

    @Test
    void settingsComeFromDsnAndResource() {
        TrooperConfig cfg = TrooperConfig.fromLookup(Map.of(
                "trooper.dsn", "https://tt_key@ingest.example.com/7",
                "trooper.profiling", "true",
                "trooper.profiling.types", "cpu,inuse_space",
                "trooper.profiling.period", "30")::get);
        Resource resource = Resource.create(Attributes.builder()
                .put("service.name", "orders-api").put("deployment.environment.name", "prod")
                .put("service.version", "2.0.0").put("host.name", "web-1").build());
        ProfilerSettings s = ProfilerBootstrap.settings(cfg, resource, DefaultConfigProperties.createFromMap(Map.of()));
        assertThat(s).isNotNull();
        assertThat(s.endpoint()).isEqualTo("https://ingest.example.com");
        assertThat(s.headers()).containsEntry("x-trooper-key", "tt_key");
        assertThat(s.service()).isEqualTo("orders-api");
        assertThat(s.environment()).isEqualTo("prod");
        assertThat(s.version()).isEqualTo("2.0.0");
        assertThat(s.host()).isEqualTo("web-1");
        assertThat(s.types()).containsExactly(ProfileType.CPU);
        assertThat(s.period()).isEqualTo(Duration.ofSeconds(30));
        assertThat(s.toString()).doesNotContain("tt_key");

        // no DSN: OTLP/HTTP endpoint + headers
        TrooperConfig noDsn = TrooperConfig.fromLookup(Map.of("trooper.profiling.enabled", "true")::get);
        ProfilerSettings fromOtel = ProfilerBootstrap.settings(noDsn, Resource.empty(), DefaultConfigProperties.createFromMap(Map.of(
                "otel.exporter.otlp.endpoint", "http://collector:4318/",
                "otel.exporter.otlp.headers", "x-trooper-key=abc")));
        assertThat(fromOtel.endpoint()).isEqualTo("http://collector:4318");
        assertThat(fromOtel.headers()).containsEntry("x-trooper-key", "abc");
        assertThat(ProfilerBootstrap.settings(noDsn, Resource.empty(), DefaultConfigProperties.createFromMap(Map.of(
                "otel.exporter.otlp.endpoint", "http://collector:4317", "otel.exporter.otlp.protocol", "grpc")))).isNull();
    }

    /** Real JFR: a busy request thread shows up in the uploaded cpu profile under its route. */
    @Test
    void uploadsJfrProfilesWithRouteSections() throws Exception {
        List<String[]> uploads = new CopyOnWriteArrayList<>();
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/profiles", ex -> {
            byte[] body = new GZIPInputStream(new ByteArrayInputStream(ex.getRequestBody().readAllBytes())).readAllBytes();
            uploads.add(new String[] {ex.getRequestURI().getRawQuery(), new String(body, StandardCharsets.UTF_8)});
            ex.sendResponseHeaders(200, -1);
            ex.close();
        });
        server.start();
        Thread burner = null;
        try {
            assertThat(TrooperProfiler.start(settings("http://127.0.0.1:" + server.getAddress().getPort()))).isTrue();
            long deadline = System.currentTimeMillis() + 10_000;
            while (!ProfileRoutes.isActive() && System.currentTimeMillis() < deadline) Thread.sleep(10);
            assertThat(ProfileRoutes.isActive()).isTrue();

            burner = new Thread(() -> {
                ProfileRoutes.entryStarted("burn-span", () -> new String[] {"/burn", "POST"});
                long until = System.currentTimeMillis() + 6_000;
                double x = 0;
                while (System.currentTimeMillis() < until && !Thread.currentThread().isInterrupted()) {
                    x += BusyWork.spin(x);
                }
                ProfileRoutes.spanEnded("burn-span");
            }, "burner");
            burner.start();

            deadline = System.currentTimeMillis() + 20_000;
            String cpu = null;
            while (cpu == null && System.currentTimeMillis() < deadline) {
                for (String[] u : uploads) {
                    if (u[0].contains("type=cpu") && u[1].contains("# trooper.route=/burn trooper.method=POST")) cpu = u[1];
                }
                Thread.sleep(100);
            }
            assertThat(cpu).as("cpu profile with /burn section; uploads: %s", uploads.size()).isNotNull();
            String section = cpu.substring(cpu.indexOf("# trooper.route=/burn"));
            assertThat(section).contains(BusyWork.class.getName() + ".spin");
            String query = uploads.stream().filter(u -> u[0].contains("type=cpu")).findFirst().orElseThrow()[0];
            assertThat(query).contains("format=folded", "unit=nanoseconds", "service=svc", "runtime=jvm", "start=", "end=");
        } finally {
            if (burner != null) burner.interrupt();
            TrooperProfiler.stop();
            server.stop(0);
        }
        assertThat(TrooperProfiler.isRunning()).isFalse();
    }

    static final class BusyWork {
        static double spin(double seed) {
            double r = seed;
            for (int i = 0; i < 10_000; i++) r = Math.sin(r + i) * 0.5;
            return r;
        }
    }
}
