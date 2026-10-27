package io.tasktrooper.telemetry.core.profiling;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.resources.Resource;
import io.tasktrooper.telemetry.core.TrooperConfig;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.logging.Level;
import java.util.logging.Logger;

/** Builds {@link ProfilerSettings} from the TaskTrooper config and the final OTel resource / exporter settings. */
public final class ProfilerBootstrap {
    private static final Logger LOG = Logger.getLogger(ProfilerBootstrap.class.getName());
    private static final AttributeKey<String> SERVICE_NAME = AttributeKey.stringKey("service.name");
    private static final AttributeKey<String> SERVICE_VERSION = AttributeKey.stringKey("service.version");
    private static final AttributeKey<String> ENVIRONMENT = AttributeKey.stringKey("deployment.environment.name");
    private static final AttributeKey<String> ENVIRONMENT_OLD = AttributeKey.stringKey("deployment.environment");
    private static final AttributeKey<String> HOST_NAME = AttributeKey.stringKey("host.name");

    private ProfilerBootstrap() {}

    /** Starts the profiler if {@code config} enables it. Never throws. */
    public static void startIfEnabled(TrooperConfig config, Resource resource, ConfigProperties props) {
        try {
            if (config == null || !config.enabled() || !config.profilingEnabled()) return;
            ProfilerSettings settings = settings(config, resource, props);
            if (settings != null) TrooperProfiler.start(settings);
        } catch (Throwable t) {
            io.tasktrooper.telemetry.core.SdkErrors.record("profiler", t);
        }
    }

    /** Settings, or null when no ingest endpoint / no supported type is configured. */
    public static ProfilerSettings settings(TrooperConfig config, Resource resource, ConfigProperties props) {
        String endpoint = null;
        Map<String, String> headers = new LinkedHashMap<>();
        if (config.dsn() != null) {
            endpoint = config.dsn().endpoint();
            headers.put("x-trooper-key", config.dsn().key());
        } else if (props != null) {
            // no DSN: reuse the OTLP/HTTP endpoint and headers (profiles have no gRPC transport)
            String protocol = props.getString("otel.exporter.otlp.protocol", "http/protobuf");
            String otlp = props.getString("otel.exporter.otlp.endpoint");
            if (otlp != null && !otlp.isBlank() && protocol.toLowerCase(Locale.ROOT).startsWith("http")) {
                endpoint = otlp.trim();
                headers.putAll(props.getMap("otel.exporter.otlp.headers"));
            }
        }
        if (endpoint == null) {
            LOG.log(Level.INFO, "TaskTrooper: profiling enabled but no DSN / OTLP HTTP endpoint configured; profiler not started");
            return null;
        }
        List<String> unsupported = new ArrayList<>();
        Set<ProfileType> types = ProfileType.parse(config.profilingTypes(), unsupported);
        if (!unsupported.isEmpty()) {
            LOG.log(Level.INFO, "TaskTrooper: profile types not supported on the JVM (ignored): " + unsupported);
        }
        if (types.isEmpty()) return null;
        Resource r = resource == null ? Resource.empty() : resource;
        String service = first(r.getAttribute(SERVICE_NAME), config.service());
        String env = first(first(r.getAttribute(ENVIRONMENT), r.getAttribute(ENVIRONMENT_OLD)), config.environment());
        String version = first(r.getAttribute(SERVICE_VERSION), config.version());
        String host = first(r.getAttribute(HOST_NAME), first(System.getenv("HOSTNAME"), System.getenv("COMPUTERNAME")));
        return new ProfilerSettings(endpoint, headers, service, env, version, host, types,
                Duration.ofSeconds(config.profilingPeriodSeconds()), null, null, ProfilerSettings.DEFAULT_START_DELAY);
    }

    private static String first(String a, String b) {
        return a != null && !a.isBlank() ? a : (b != null && !b.isBlank() ? b : null);
    }
}
