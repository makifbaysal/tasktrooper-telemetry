package io.tasktrooper.telemetry.javaagent;

import static org.assertj.core.api.Assertions.assertThat;

import com.sun.net.httpserver.HttpServer;
import io.opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest;
import io.opentelemetry.proto.common.v1.KeyValue;
import io.opentelemetry.proto.trace.v1.Span;
import java.io.File;
import java.net.InetSocketAddress;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Optional;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;
import java.util.jar.JarFile;
import java.util.stream.Collectors;
import java.util.zip.GZIPInputStream;
import org.junit.jupiter.api.Test;

/** Runs {@code java -javaagent:trooper-javaagent.jar TinyApp} against a fake ingest-gw and checks the OTLP payload. */
class JavaagentIntegrationTest {
    private static final Path AGENT = Path.of(System.getProperty("trooper.agentJar"));

    @Test
    void agentJarEmbedsExtension() throws Exception {
        try (JarFile jar = new JarFile(AGENT.toFile())) {
            assertThat(jar.getEntry("extensions/trooper-otel-extension.jar")).isNotNull();
            assertThat(jar.getManifest().getMainAttributes().getValue("Premain-Class"))
                    .isEqualTo("io.opentelemetry.javaagent.OpenTelemetryAgent");
        }
    }

    @Test
    void exportsTrooperAttributesToDsn() throws Exception {
        List<ExportTraceServiceRequest> traces = new CopyOnWriteArrayList<>();
        List<String> keys = new CopyOnWriteArrayList<>();
        HttpServer ingest = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        ingest.createContext("/", ex -> {
            byte[] body = ex.getRequestBody().readAllBytes();
            if ("gzip".equalsIgnoreCase(ex.getRequestHeaders().getFirst("Content-Encoding"))) {
                body = new GZIPInputStream(new java.io.ByteArrayInputStream(body)).readAllBytes();
            }
            if (ex.getRequestURI().getPath().equals("/v1/traces")) {
                keys.add(ex.getRequestHeaders().getFirst("x-trooper-key"));
                traces.add(ExportTraceServiceRequest.parseFrom(body));
            }
            ex.sendResponseHeaders(200, -1);
            ex.close();
        });
        ingest.start();
        try {
            String java = Path.of(System.getProperty("java.home"), "bin", "java").toString();
            Path log = Files.createTempFile("tinyapp", ".log");
            ProcessBuilder pb = new ProcessBuilder(java, "-javaagent:" + AGENT, "-cp",
                    System.getProperty("trooper.testAppClasspath"), "io.tasktrooper.telemetry.testapp.TinyApp")
                    .redirectErrorStream(true)
                    .redirectOutput(log.toFile());
            pb.environment().put("TROOPER_DSN", "http://agent_test_key@127.0.0.1:" + ingest.getAddress().getPort() + "/1");
            pb.environment().put("TROOPER_SERVICE", "agent-test-app");
            pb.environment().put("TROOPER_ENVIRONMENT", "test");
            pb.environment().put("OTEL_METRICS_EXPORTER", "none");
            pb.environment().put("OTEL_LOGS_EXPORTER", "none");
            pb.environment().put("OTEL_BSP_SCHEDULE_DELAY", "100");
            Process p = pb.start();
            assertThat(p.waitFor(120, TimeUnit.SECONDS)).as("app finished").isTrue();
            String output = Files.readString(log);
            assertThat(output).contains("status=200");

            List<Span> spans = traces.stream()
                    .flatMap(r -> r.getResourceSpansList().stream())
                    .peek(rs -> assertThat(attr(rs.getResource().getAttributesList(), "service.name")).contains("agent-test-app"))
                    .peek(rs -> assertThat(attr(rs.getResource().getAttributesList(), "telemetry.distro.name")).contains("tasktrooper-javaagent"))
                    .flatMap(rs -> rs.getScopeSpansList().stream())
                    .flatMap(ss -> ss.getSpansList().stream())
                    .collect(Collectors.toList());
            assertThat(spans).as("spans exported; app output:\n" + output).isNotEmpty();
            assertThat(keys).containsOnly("agent_test_key");

            Span server = spans.stream().filter(s -> s.getKind() == Span.SpanKind.SPAN_KIND_SERVER).findFirst()
                    .orElseThrow(() -> new AssertionError("no server span in " + spans));
            assertThat(attr(server.getAttributesList(), "trooper.caller")).contains("agent-test-app");
            assertThat(server.getAttributesList().stream().filter(kv -> kv.getKey().equals("trooper.sample_rate"))
                    .map(kv -> kv.getValue().getDoubleValue()).findFirst()).contains(1.0);
            // the client root span advertised its rate in tracestate; the server (remote parent) inherits it
            assertThat(server.getTraceState()).contains("tt=r:1");
            assertThat(attr(server.getAttributesList(), "trooper.entry.method")).contains("GET");
        } finally {
            ingest.stop(0);
        }
    }

    /** {@code TROOPER_PROFILING=true}: JFR runs inside the agent and uploads folded profiles with route sections. */
    @Test
    void uploadsProfilesWithRouteSections() throws Exception {
        List<String[]> profiles = new CopyOnWriteArrayList<>();
        List<ExportTraceServiceRequest> traces = new CopyOnWriteArrayList<>();
        HttpServer ingest = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        ingest.createContext("/", ex -> {
            byte[] body = ex.getRequestBody().readAllBytes();
            if ("gzip".equalsIgnoreCase(ex.getRequestHeaders().getFirst("Content-Encoding"))) {
                body = new GZIPInputStream(new java.io.ByteArrayInputStream(body)).readAllBytes();
            }
            String path = ex.getRequestURI().getPath();
            if (path.equals("/v1/profiles")) {
                profiles.add(new String[] {ex.getRequestURI().getRawQuery(), new String(body, java.nio.charset.StandardCharsets.UTF_8),
                        ex.getRequestHeaders().getFirst("x-trooper-key")});
            } else if (path.equals("/v1/traces")) {
                traces.add(ExportTraceServiceRequest.parseFrom(body));
            }
            ex.sendResponseHeaders(200, -1);
            ex.close();
        });
        ingest.start();
        try {
            String java = Path.of(System.getProperty("java.home"), "bin", "java").toString();
            Path log = Files.createTempFile("profiledapp", ".log");
            ProcessBuilder pb = new ProcessBuilder(java, "-javaagent:" + AGENT, "-cp",
                    System.getProperty("trooper.testAppClasspath"), "io.tasktrooper.telemetry.testapp.ProfiledApp", "17")
                    .redirectErrorStream(true)
                    .redirectOutput(log.toFile());
            pb.environment().put("TROOPER_DSN", "http://agent_test_key@127.0.0.1:" + ingest.getAddress().getPort() + "/1");
            pb.environment().put("TROOPER_SERVICE", "profiled-app");
            pb.environment().put("TROOPER_ENV", "test");
            pb.environment().put("TROOPER_PROFILING", "true");
            pb.environment().put("TROOPER_PROFILING_PERIOD", "5");
            pb.environment().put("OTEL_METRICS_EXPORTER", "none");
            pb.environment().put("OTEL_LOGS_EXPORTER", "none");
            pb.environment().put("OTEL_BSP_SCHEDULE_DELAY", "100");
            Process p = pb.start();
            assertThat(p.waitFor(120, TimeUnit.SECONDS)).as("app finished").isTrue();
            String output = Files.readString(log);
            assertThat(output).contains("requests=");

            assertThat(profiles).as("profiles uploaded; app output:\n" + output).isNotEmpty();
            // JFR's execution sampler density varies by platform (sparse on macOS), so any profile type will do here;
            // cpu attribution itself is covered by TrooperProfilerTest in trooper-otel-core
            assertThat(profiles).allSatisfy(u -> assertThat(u[0]).containsAnyOf("type=cpu&unit=nanoseconds",
                    "type=alloc_space&unit=bytes", "type=lock&unit=nanoseconds"));
            String[] labelled = profiles.stream().filter(u -> u[1].contains("# trooper.route=/burn"))
                    .findFirst().orElseThrow(() -> new AssertionError("no profile with a /burn section in "
                            + profiles.stream().map(u -> u[0]).toList()));
            assertThat(labelled[0]).contains("format=folded", "service=profiled-app", "env=test", "runtime=jvm", "start=", "end=");
            assertThat(labelled[2]).isEqualTo("agent_test_key");
            String section = labelled[1].substring(labelled[1].indexOf("# trooper.route=/burn"));
            assertThat(section).startsWith("# trooper.route=/burn trooper.method=GET\n");
            assertThat(section).contains("io.tasktrooper.telemetry.testapp.ProfiledApp.lambda$main$0");

            // the profile uploads themselves are not traced as application HTTP calls
            List<Span> spans = traces.stream().flatMap(r -> r.getResourceSpansList().stream())
                    .flatMap(rs -> rs.getScopeSpansList().stream()).flatMap(ss -> ss.getSpansList().stream())
                    .collect(Collectors.toList());
            assertThat(spans).isNotEmpty();
            assertThat(spans).noneMatch(s -> s.getAttributesList().stream()
                    .anyMatch(kv -> kv.getValue().getStringValue().contains("/v1/profiles")));
        } finally {
            ingest.stop(0);
        }
    }

    private static Optional<String> attr(List<KeyValue> attrs, String key) {
        return attrs.stream().filter(kv -> kv.getKey().equals(key)).map(kv -> kv.getValue().getStringValue()).findFirst();
    }

}
