package io.tasktrooper.telemetry.core;

import java.net.URI;
import java.util.Objects;

/**
 * Parsed TaskTrooper DSN: {@code https://<key>@<host>[:port]/<project_id>} (contracts.md §2).
 *
 * @param endpoint OTLP base endpoint ({@code scheme://host[:port]}); the exporter appends {@code /v1/traces} etc.
 * @param key ingest key, sent as {@code x-trooper-key}
 * @param projectId project id from the path (informational; the key determines the project server side)
 */
public record Dsn(String endpoint, String key, String projectId) {

    public Dsn {
        Objects.requireNonNull(endpoint, "endpoint");
        Objects.requireNonNull(key, "key");
    }

    /** Parses a DSN. @throws IllegalArgumentException if the DSN is malformed. */
    public static Dsn parse(String dsn) {
        if (dsn == null || dsn.isBlank()) {
            throw new IllegalArgumentException("DSN is empty");
        }
        URI uri;
        try {
            uri = URI.create(dsn.trim());
        } catch (RuntimeException e) {
            throw new IllegalArgumentException("DSN is not a valid URI", e);
        }
        String scheme = uri.getScheme();
        if (!"http".equalsIgnoreCase(scheme) && !"https".equalsIgnoreCase(scheme)) {
            throw new IllegalArgumentException("DSN scheme must be http or https");
        }
        String key = uri.getRawUserInfo();
        if (key == null || key.isBlank()) {
            throw new IllegalArgumentException("DSN has no ingest key (expected https://<key>@host/<project>)");
        }
        int colon = key.indexOf(':'); // tolerate key:secret form, the key is the user part
        if (colon >= 0) {
            key = key.substring(0, colon);
        }
        if (uri.getHost() == null) {
            throw new IllegalArgumentException("DSN has no host");
        }
        String endpoint = scheme.toLowerCase() + "://" + uri.getHost() + (uri.getPort() > 0 ? ":" + uri.getPort() : "");
        String path = uri.getPath() == null ? "" : uri.getPath().replaceAll("^/+|/+$", "");
        String project = path.isEmpty() ? null : path.substring(path.lastIndexOf('/') + 1);
        return new Dsn(endpoint, key, project);
    }

    @Override
    public String toString() {
        // never log the key
        return "Dsn[endpoint=" + endpoint + ", projectId=" + projectId + "]";
    }
}
