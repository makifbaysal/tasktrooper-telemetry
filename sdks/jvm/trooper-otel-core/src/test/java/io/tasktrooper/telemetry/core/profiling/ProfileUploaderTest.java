package io.tasktrooper.telemetry.core.profiling;

import static org.assertj.core.api.Assertions.assertThat;

import com.sun.net.httpserver.HttpServer;
import java.io.ByteArrayInputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.EnumSet;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.zip.GZIPInputStream;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

class ProfileUploaderTest {
    record Upload(String path, String query, String key, String contentType, String encoding, String body) {}

    private HttpServer server;
    private final List<Upload> uploads = new CopyOnWriteArrayList<>();
    private final AtomicInteger status = new AtomicInteger(200);

    @BeforeEach
    void start() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", ex -> {
            byte[] body = ex.getRequestBody().readAllBytes();
            if ("gzip".equals(ex.getRequestHeaders().getFirst("Content-Encoding"))) {
                body = new GZIPInputStream(new ByteArrayInputStream(body)).readAllBytes();
            }
            uploads.add(new Upload(ex.getRequestURI().getPath(), ex.getRequestURI().getRawQuery(),
                    ex.getRequestHeaders().getFirst("x-trooper-key"), ex.getRequestHeaders().getFirst("Content-Type"),
                    ex.getRequestHeaders().getFirst("Content-Encoding"), new String(body, StandardCharsets.UTF_8)));
            ex.sendResponseHeaders(status.get(), -1);
            ex.close();
        });
        server.start();
    }

    @AfterEach
    void stop() {
        server.stop(0);
    }

    private ProfilerSettings settings(String endpoint) {
        return new ProfilerSettings(endpoint, Map.of("x-trooper-key", "tt_key"), "orders-api", "prod", "1.2.3+sha 9", "host-1",
                EnumSet.allOf(ProfileType.class), Duration.ofSeconds(60), null, null, Duration.ZERO);
    }

    private static FoldedProfile cpu() {
        FoldedProfile p = new FoldedProfile(ProfileType.CPU);
        p.add(null, null, "java.lang.Thread.run;com.acme.Worker.loop", 20_000_000);
        p.add("/v1/orders/:id", "GET", "java.lang.Thread.run;com.acme.Orders.get", 40_000_000);
        return p;
    }

    @Test
    void urlCarriesMetadataQueryParameters() {
        String url = ProfileUploader.url(settings("https://ingest.example.com/"), ProfileType.ALLOC_SPACE, 1000, 61000);
        assertThat(url).isEqualTo("https://ingest.example.com/v1/profiles?format=folded&type=alloc_space&unit=bytes"
                + "&service=orders-api&env=prod&version=1.2.3%2Bsha+9&host=host-1&runtime=jvm&start=1000&end=61000");

        ProfilerSettings minimal = new ProfilerSettings("http://h:4318", null, null, null, null, null, null, null, null, null, null);
        assertThat(ProfileUploader.url(minimal, ProfileType.LOCK, 1, 2)).isEqualTo(
                "http://h:4318/v1/profiles?format=folded&type=lock&unit=nanoseconds&service=unknown_service%3Ajava&runtime=jvm&start=1&end=2");
    }

    @Test
    void postsGzippedFoldedProfileWithKey() {
        ProfileUploader uploader = new ProfileUploader(settings("http://127.0.0.1:" + server.getAddress().getPort()));
        assertThat(uploader.upload(cpu(), 1_700_000_000_000L, 1_700_000_060_000L)).isEqualTo(ProfileUploader.Result.SENT);

        assertThat(uploads).hasSize(1);
        Upload u = uploads.get(0);
        assertThat(u.path()).isEqualTo("/v1/profiles");
        assertThat(u.query()).contains("format=folded", "type=cpu", "unit=nanoseconds", "service=orders-api", "env=prod",
                "host=host-1", "runtime=jvm", "start=1700000000000", "end=1700000060000");
        assertThat(u.key()).isEqualTo("tt_key");
        assertThat(u.contentType()).startsWith("text/plain");
        assertThat(u.encoding()).isEqualTo("gzip");
        assertThat(u.body()).isEqualTo("""
                java.lang.Thread.run;com.acme.Worker.loop 20000000
                # trooper.route=/v1/orders/:id trooper.method=GET
                java.lang.Thread.run;com.acme.Orders.get 40000000
                """);
    }

    @Test
    void emptyProfilesAreNotSent() {
        ProfileUploader uploader = new ProfileUploader(settings("http://127.0.0.1:" + server.getAddress().getPort()));
        assertThat(uploader.upload(new FoldedProfile(ProfileType.CPU), 0, 1)).isEqualTo(ProfileUploader.Result.EMPTY);
        assertThat(uploads).isEmpty();
    }

    @Test
    void serverErrorsBackOffAndDropTheProfile() {
        status.set(503);
        ProfileUploader uploader = new ProfileUploader(settings("http://127.0.0.1:" + server.getAddress().getPort()));
        assertThat(uploader.upload(cpu(), 0, 1)).isEqualTo(ProfileUploader.Result.DROPPED);
        assertThat(uploader.backoffRemainingMs()).isPositive().isLessThanOrEqualTo(ProfileUploader.BASE_BACKOFF_MS);
        status.set(200);
        assertThat(uploader.upload(cpu(), 0, 1)).isEqualTo(ProfileUploader.Result.SKIPPED_BACKOFF);
        assertThat(uploads).hasSize(1);
    }

    @Test
    void clientErrorsDropWithoutBackoff() {
        status.set(400);
        ProfileUploader uploader = new ProfileUploader(settings("http://127.0.0.1:" + server.getAddress().getPort()));
        assertThat(uploader.upload(cpu(), 0, 1)).isEqualTo(ProfileUploader.Result.DROPPED);
        assertThat(uploader.backoffRemainingMs()).isZero();
        status.set(200);
        assertThat(uploader.upload(cpu(), 0, 1)).isEqualTo(ProfileUploader.Result.SENT);
    }

    @Test
    void networkErrorsNeverThrow() throws Exception {
        int port;
        try (java.net.ServerSocket s = new java.net.ServerSocket(0)) {
            port = s.getLocalPort(); // closed again: connection refused
        }
        ProfileUploader uploader = new ProfileUploader(settings("http://127.0.0.1:" + port));
        assertThat(uploader.upload(cpu(), 0, 1)).isEqualTo(ProfileUploader.Result.DROPPED);
        assertThat(uploader.backoffRemainingMs()).isPositive();
    }
}
