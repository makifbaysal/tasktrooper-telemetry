package io.tasktrooper.telemetry.core.profiling;

import io.tasktrooper.telemetry.core.SdkErrors;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URI;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.zip.GZIPOutputStream;

/**
 * {@code POST /v1/profiles?format=folded&type=…&unit=…&service=…&env=…&version=…&host=…&runtime=jvm&start=…&end=…}
 * (contracts.md §24), gzip body, {@code x-trooper-key} header.
 *
 * <p>A failed profile is dropped (no queue). After 429 / 5xx / network errors further uploads are skipped for an
 * exponential, jittered backoff 1 s → 60 s ({@code Retry-After} honoured); other 4xx drop only that profile
 * (contracts.md §2). Never throws.
 */
public final class ProfileUploader {
    private static final Logger LOG = Logger.getLogger(ProfileUploader.class.getName());
    static final long BASE_BACKOFF_MS = 1_000;
    static final long MAX_BACKOFF_MS = 60_000;

    private final ProfilerSettings settings;
    private int failures;
    private long blockedUntilMs;

    public ProfileUploader(ProfilerSettings settings) {
        this.settings = settings;
    }

    /** Outcome of one upload attempt (tests / diagnostics). */
    public enum Result { SENT, DROPPED, SKIPPED_BACKOFF, EMPTY }

    public synchronized Result upload(FoldedProfile profile, long startMs, long endMs) {
        if (profile == null || profile.isEmpty()) return Result.EMPTY;
        long now = System.currentTimeMillis();
        if (now < blockedUntilMs) return Result.SKIPPED_BACKOFF;
        try {
            byte[] body = gzip(profile.render());
            int[] status = {0};
            String[] retryAfter = {null};
            IOException[] error = {null};
            Runnable send = () -> {
                try {
                    HttpURLConnection c = (HttpURLConnection) URI.create(url(settings, profile.type(), startMs, endMs)).toURL().openConnection();
                    try {
                        c.setRequestMethod("POST");
                        c.setDoOutput(true);
                        c.setConnectTimeout(10_000);
                        c.setReadTimeout(30_000);
                        c.setRequestProperty("Content-Type", "text/plain; charset=utf-8");
                        c.setRequestProperty("Content-Encoding", "gzip");
                        settings.headers().forEach(c::setRequestProperty);
                        c.setFixedLengthStreamingMode(body.length);
                        try (OutputStream out = c.getOutputStream()) {
                            out.write(body);
                        }
                        status[0] = c.getResponseCode();
                        retryAfter[0] = c.getHeaderField("Retry-After");
                        drain(status[0] >= 400 ? c.getErrorStream() : c.getInputStream());
                    } finally {
                        c.disconnect();
                    }
                } catch (IOException e) {
                    error[0] = e;
                }
            };
            suppressInstrumentation(send);
            if (error[0] != null) {
                backoff(null);
                LOG.log(Level.FINE, "TaskTrooper: profile upload failed", error[0]);
                SdkErrors.record("profiler.upload", error[0]);
                return Result.DROPPED;
            }
            int s = status[0];
            if (s >= 200 && s < 300) {
                failures = 0;
                return Result.SENT;
            }
            if (s == 429 || s >= 500) {
                backoff(retryAfter[0]);
            }
            LOG.log(Level.FINE, "TaskTrooper: profile upload rejected with HTTP " + s);
            SdkErrors.record("profiler.upload", null);
            return Result.DROPPED;
        } catch (Throwable t) {
            SdkErrors.record("profiler.upload", t);
            return Result.DROPPED;
        }
    }

    private void backoff(String retryAfter) {
        failures++;
        long delay = Math.min(MAX_BACKOFF_MS, BASE_BACKOFF_MS << Math.min(16, failures - 1));
        delay = delay / 2 + ThreadLocalRandom.current().nextLong(delay / 2 + 1); // jitter
        if (retryAfter != null) {
            try {
                delay = Math.max(delay, Math.min(MAX_BACKOFF_MS, Long.parseLong(retryAfter.trim()) * 1000));
            } catch (NumberFormatException ignored) {
                // HTTP-date form: keep the computed delay
            }
        }
        blockedUntilMs = System.currentTimeMillis() + delay;
    }

    /** Remaining backoff in ms (tests). */
    synchronized long backoffRemainingMs() {
        return Math.max(0, blockedUntilMs - System.currentTimeMillis());
    }

    /** Full upload URL with metadata query parameters; empty values are omitted. */
    public static String url(ProfilerSettings s, ProfileType type, long startMs, long endMs) {
        Map<String, String> q = new LinkedHashMap<>();
        q.put("format", "folded");
        q.put("type", type.wireName());
        q.put("unit", type.unit());
        q.put("service", s.service());
        q.put("env", s.environment());
        q.put("version", s.version());
        q.put("host", s.host());
        q.put("runtime", "jvm");
        q.put("start", Long.toString(startMs));
        q.put("end", Long.toString(endMs));
        StringBuilder sb = new StringBuilder(s.endpoint()).append("/v1/profiles");
        char sep = '?';
        for (Map.Entry<String, String> e : q.entrySet()) {
            if (e.getValue() == null || e.getValue().isBlank()) continue;
            sb.append(sep).append(e.getKey()).append('=').append(URLEncoder.encode(e.getValue(), StandardCharsets.UTF_8));
            sep = '&';
        }
        return sb.toString();
    }

    static byte[] gzip(String text) throws IOException {
        ByteArrayOutputStream bos = new ByteArrayOutputStream(Math.max(64, text.length() / 4));
        try (GZIPOutputStream gz = new GZIPOutputStream(bos)) {
            gz.write(text.getBytes(StandardCharsets.UTF_8));
        }
        return bos.toByteArray();
    }

    private static void drain(InputStream in) {
        if (in == null) return;
        try (in) {
            byte[] buf = new byte[1024];
            while (in.read(buf) >= 0) {
                // discard
            }
        } catch (IOException ignored) {
            // response body is irrelevant
        }
    }

    /**
     * Under the javaagent, HTTP client instrumentation would otherwise turn every upload into a span of the host
     * application; the exporter suppression key is honoured by all OTel instrumentations.
     */
    private static void suppressInstrumentation(Runnable r) {
        try {
            io.opentelemetry.api.impl.InstrumentationUtil.suppressInstrumentation(r);
        } catch (LinkageError e) {
            r.run(); // older API without the helper
        }
    }
}
