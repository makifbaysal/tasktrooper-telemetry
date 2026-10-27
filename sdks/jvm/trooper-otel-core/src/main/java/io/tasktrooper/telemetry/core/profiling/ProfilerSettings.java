package io.tasktrooper.telemetry.core.profiling;

import java.time.Duration;
import java.util.Map;
import java.util.Set;

/**
 * Everything the profiler needs: where to upload, resource metadata (query parameters of {@code /v1/profiles}) and
 * what to record.
 *
 * @param endpoint ingest base URL ({@code scheme://host[:port]}, no trailing slash); {@code /v1/profiles} is appended
 * @param headers request headers (normally {@code x-trooper-key})
 * @param period aggregation / upload period (contracts.md §24 {@code TROOPER_PROFILING_PERIOD}, default 60 s)
 * @param cpuInterval {@code jdk.ExecutionSample} period
 * @param lockThreshold minimum monitor-enter / park duration recorded as {@code lock}
 * @param startDelay delay before JFR starts (keeps it out of application startup)
 */
public record ProfilerSettings(
        String endpoint,
        Map<String, String> headers,
        String service,
        String environment,
        String version,
        String host,
        Set<ProfileType> types,
        Duration period,
        Duration cpuInterval,
        Duration lockThreshold,
        Duration startDelay) {

    public static final Duration DEFAULT_CPU_INTERVAL = Duration.ofMillis(20);
    public static final Duration DEFAULT_LOCK_THRESHOLD = Duration.ofMillis(10);
    public static final Duration DEFAULT_START_DELAY = Duration.ofSeconds(5);

    public ProfilerSettings {
        endpoint = endpoint == null ? null : endpoint.replaceAll("/+$", "");
        headers = headers == null ? Map.of() : Map.copyOf(headers);
        service = service == null || service.isBlank() ? "unknown_service:java" : service;
        types = types == null || types.isEmpty() ? ProfileType.DEFAULTS : Set.copyOf(types);
        period = period == null || period.isNegative() || period.isZero() ? Duration.ofSeconds(60) : period;
        cpuInterval = cpuInterval == null || cpuInterval.toMillis() < 1 ? DEFAULT_CPU_INTERVAL : cpuInterval;
        lockThreshold = lockThreshold == null ? DEFAULT_LOCK_THRESHOLD : lockThreshold;
        startDelay = startDelay == null || startDelay.isNegative() ? Duration.ZERO : startDelay;
    }

    public ProfilerSettings withStartDelay(Duration delay) {
        return new ProfilerSettings(endpoint, headers, service, environment, version, host, types, period, cpuInterval, lockThreshold, delay);
    }

    public ProfilerSettings withPeriod(Duration p) {
        return new ProfilerSettings(endpoint, headers, service, environment, version, host, types, p, cpuInterval, lockThreshold, startDelay);
    }

    @Override
    public String toString() {
        // never print header values (ingest key)
        return "ProfilerSettings[endpoint=" + endpoint + ", service=" + service + ", env=" + environment + ", version=" + version
                + ", host=" + host + ", types=" + types + ", period=" + period + "]";
    }
}
