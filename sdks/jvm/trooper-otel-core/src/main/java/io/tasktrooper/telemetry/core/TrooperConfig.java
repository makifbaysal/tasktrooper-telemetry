package io.tasktrooper.telemetry.core;

import java.util.LinkedHashMap;
import java.util.Locale;
import java.util.Map;
import java.util.function.Function;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * TaskTrooper SDK configuration. Read from {@code trooper.*} properties or {@code TROOPER_*} environment variables and
 * translated to standard {@code otel.*} properties so the regular OTel autoconfiguration does the work.
 *
 * <p>Keys: {@code trooper.dsn}, {@code trooper.service}, {@code trooper.env} (alias {@code trooper.environment}), {@code trooper.version},
 * {@code trooper.sample-rate}, {@code trooper.enabled}. Environment equivalents: {@code TROOPER_DSN},
 * {@code TROOPER_SERVICE}, {@code TROOPER_ENV} / {@code TROOPER_ENVIRONMENT}, {@code TROOPER_VERSION}, {@code TROOPER_SAMPLE_RATE},
 * {@code TROOPER_ENABLED}.
 *
 * <p>Profiling (contracts.md §24): {@code trooper.profiling.enabled} (alias {@code trooper.profiling}; env
 * {@code TROOPER_PROFILING} / {@code TROOPER_PROFILING_ENABLED}), {@code trooper.profiling.types}
 * ({@code TROOPER_PROFILING_TYPES}), {@code trooper.profiling.period} (seconds, {@code TROOPER_PROFILING_PERIOD}).
 * LLM (contracts.md §25): {@code trooper.llm.capture-content} ({@code TROOPER_LLM_CAPTURE_CONTENT}).
 */
public final class TrooperConfig {
    private static final Logger LOG = Logger.getLogger(TrooperConfig.class.getName());

    public static final String DSN = "trooper.dsn";
    public static final String SERVICE = "trooper.service";
    /** Common name across SDKs (contracts.md §2): {@code TROOPER_ENV}. */
    public static final String ENV = "trooper.env";
    /** Long form, kept for compatibility: {@code TROOPER_ENVIRONMENT}. */
    public static final String ENVIRONMENT = "trooper.environment";
    public static final String VERSION = "trooper.version";
    public static final String SAMPLE_RATE = "trooper.sample-rate";
    public static final String ENABLED = "trooper.enabled";
    public static final String PROFILING_ENABLED = "trooper.profiling.enabled";
    /** Short form; the env var {@code TROOPER_PROFILING} maps here. */
    public static final String PROFILING = "trooper.profiling";
    public static final String PROFILING_TYPES = "trooper.profiling.types";
    public static final String PROFILING_PERIOD = "trooper.profiling.period";
    public static final String LLM_CAPTURE_CONTENT = "trooper.llm.capture-content";

    /** Default upload period (contracts.md §24). */
    public static final int DEFAULT_PROFILING_PERIOD_SECONDS = 60;

    private final Dsn dsn;
    private final String service;
    private final String environment;
    private final String version;
    private final double sampleRate;
    private final boolean enabled;
    private final boolean profilingEnabled;
    private final java.util.List<String> profilingTypes;
    private final int profilingPeriodSeconds;
    private final Boolean llmCaptureContent;

    private TrooperConfig(Builder b) {
        this.dsn = b.dsn;
        this.service = blankToNull(b.service);
        this.environment = blankToNull(b.environment);
        this.version = blankToNull(b.version);
        this.sampleRate = clamp(b.sampleRate);
        this.enabled = b.enabled;
        this.profilingEnabled = b.profilingEnabled;
        this.profilingTypes = b.profilingTypes == null ? java.util.List.of() : java.util.List.copyOf(b.profilingTypes);
        this.profilingPeriodSeconds = b.profilingPeriodSeconds <= 0 ? DEFAULT_PROFILING_PERIOD_SECONDS
                : Math.max(5, Math.min(3600, b.profilingPeriodSeconds));
        this.llmCaptureContent = b.llmCaptureContent;
    }

    public static Builder builder() {
        return new Builder();
    }

    /** Reads system properties first, then environment variables. */
    public static TrooperConfig fromSystem() {
        return fromLookup(TrooperConfig::systemLookup);
    }

    /**
     * Builds a config from a property lookup keyed by the dotted names ({@code trooper.dsn}, ...). Invalid values are
     * logged and ignored — never thrown — so a bad DSN can't crash the host application.
     */
    public static TrooperConfig fromLookup(Function<String, String> lookup) {
        Builder b = builder();
        String rawDsn = lookup.apply(DSN);
        if (rawDsn != null && !rawDsn.isBlank()) {
            try {
                b.dsn(Dsn.parse(rawDsn));
            } catch (IllegalArgumentException e) {
                LOG.log(Level.WARNING, "TaskTrooper: ignoring invalid DSN: " + e.getMessage());
                SdkErrors.record("config", e);
            }
        }
        b.service(lookup.apply(SERVICE));
        String env = lookup.apply(ENV);
        b.environment(env != null && !env.isBlank() ? env : lookup.apply(ENVIRONMENT));
        b.version(lookup.apply(VERSION));
        String rate = lookup.apply(SAMPLE_RATE);
        if (rate != null && !rate.isBlank()) {
            try {
                b.sampleRate(Double.parseDouble(rate.trim()));
            } catch (NumberFormatException e) {
                LOG.log(Level.WARNING, "TaskTrooper: ignoring invalid sample rate '" + rate + "'");
            }
        }
        String enabled = lookup.apply(ENABLED);
        if (enabled != null && !enabled.isBlank()) {
            b.enabled(Boolean.parseBoolean(enabled.trim()));
        }
        String profiling = lookup.apply(PROFILING_ENABLED);
        if (profiling == null || profiling.isBlank()) {
            profiling = lookup.apply(PROFILING);
        }
        if (profiling != null && !profiling.isBlank()) {
            b.profilingEnabled(Boolean.parseBoolean(profiling.trim()));
        }
        String types = lookup.apply(PROFILING_TYPES);
        if (types != null && !types.isBlank()) {
            java.util.List<String> list = new java.util.ArrayList<>();
            for (String t : types.split(",")) {
                if (!t.isBlank()) list.add(t.trim().toLowerCase(Locale.ROOT));
            }
            b.profilingTypes(list);
        }
        String period = lookup.apply(PROFILING_PERIOD);
        if (period != null && !period.isBlank()) {
            try {
                b.profilingPeriodSeconds(Integer.parseInt(period.trim()));
            } catch (NumberFormatException e) {
                LOG.log(Level.WARNING, "TaskTrooper: ignoring invalid profiling period '" + period + "'");
            }
        }
        String capture = lookup.apply(LLM_CAPTURE_CONTENT);
        if (capture != null && !capture.isBlank()) {
            b.llmCaptureContent(Boolean.parseBoolean(capture.trim()));
        }
        return b.build();
    }

    static String systemLookup(String key) {
        String v = System.getProperty(key);
        if (v == null) {
            v = System.getenv(envName(key));
        }
        return v;
    }

    /** {@code trooper.sample-rate} → {@code TROOPER_SAMPLE_RATE}. */
    public static String envName(String key) {
        return key.toUpperCase(Locale.ROOT).replace('.', '_').replace('-', '_');
    }

    /**
     * Standard OTel properties derived from this config. They are meant to be <em>defaults</em>: explicit
     * {@code otel.*} settings from the user win.
     */
    public Map<String, String> toOtelProperties() {
        Map<String, String> p = new LinkedHashMap<>();
        if (!enabled) {
            p.put("otel.sdk.disabled", "true");
            return p;
        }
        if (dsn != null) {
            p.put("otel.exporter.otlp.endpoint", dsn.endpoint());
            p.put("otel.exporter.otlp.protocol", "http/protobuf");
            p.put("otel.exporter.otlp.headers", "x-trooper-key=" + dsn.key());
        }
        if (service != null) {
            p.put("otel.service.name", service);
        }
        String resource = resourceAttributes();
        if (!resource.isEmpty()) {
            p.put("otel.resource.attributes", resource);
        }
        p.put("otel.traces.sampler", "parentbased_traceidratio");
        p.put("otel.traces.sampler.arg", Double.toString(sampleRate));
        // Lets the span processor read tt-attempt / kafka_deliveryAttempt / x-delivery-count (trooper.consumer.attempt).
        p.put("otel.instrumentation.messaging.experimental.capture-headers", TrooperAttributes.CAPTURED_HEADERS);
        if (Boolean.TRUE.equals(llmCaptureContent)) {
            // OTel GenAI instrumentations (OpenAI Java SDK, AWS Bedrock ...) record prompts/completions only when asked.
            p.put("otel.instrumentation.genai.capture-message-content", "true");
        }
        return p;
    }

    /** {@code deployment.environment.name=...,service.version=...} (empty when neither is set). */
    public String resourceAttributes() {
        StringBuilder sb = new StringBuilder();
        if (environment != null) {
            sb.append("deployment.environment.name=").append(environment);
        }
        if (version != null) {
            if (sb.length() > 0) sb.append(',');
            sb.append("service.version=").append(version);
        }
        return sb.toString();
    }

    public Dsn dsn() { return dsn; }
    public String service() { return service; }
    public String environment() { return environment; }
    public String version() { return version; }
    public double sampleRate() { return sampleRate; }
    public boolean enabled() { return enabled; }
    public boolean profilingEnabled() { return profilingEnabled; }
    /** Requested profile types (lower case); empty = runtime default. */
    public java.util.List<String> profilingTypes() { return profilingTypes; }
    public int profilingPeriodSeconds() { return profilingPeriodSeconds; }
    /**
     * {@code TRUE}: record LLM prompts/completions; {@code FALSE}: strip them even if a library recorded them;
     * {@code null} (unset): leave each library's own opt-in (all default off) alone.
     */
    public Boolean llmCaptureContent() { return llmCaptureContent; }

    private static double clamp(double r) {
        if (Double.isNaN(r)) return 1.0;
        return Math.max(0.0, Math.min(1.0, r));
    }

    private static String blankToNull(String s) {
        return s == null || s.isBlank() ? null : s.trim();
    }

    @Override
    public String toString() {
        return "TrooperConfig[dsn=" + dsn + ", service=" + service + ", environment=" + environment
                + ", version=" + version + ", sampleRate=" + sampleRate + ", enabled=" + enabled
                + ", profiling=" + profilingEnabled + ", llmCaptureContent=" + llmCaptureContent + "]";
    }

    public static final class Builder {
        private Dsn dsn;
        private String service;
        private String environment;
        private String version;
        private double sampleRate = 1.0;
        private boolean enabled = true;
        private boolean profilingEnabled;
        private java.util.List<String> profilingTypes;
        private int profilingPeriodSeconds = DEFAULT_PROFILING_PERIOD_SECONDS;
        private Boolean llmCaptureContent;

        public Builder dsn(Dsn dsn) { this.dsn = dsn; return this; }
        /** Parses the DSN; invalid DSNs are logged and ignored. */
        public Builder dsn(String dsn) {
            if (dsn == null || dsn.isBlank()) { this.dsn = null; return this; }
            try {
                this.dsn = Dsn.parse(dsn);
            } catch (IllegalArgumentException e) {
                LOG.log(Level.WARNING, "TaskTrooper: ignoring invalid DSN: " + e.getMessage());
            }
            return this;
        }
        public Builder service(String service) { this.service = service; return this; }
        public Builder environment(String environment) { this.environment = environment; return this; }
        public Builder version(String version) { this.version = version; return this; }
        public Builder sampleRate(double sampleRate) { this.sampleRate = sampleRate; return this; }
        public Builder enabled(boolean enabled) { this.enabled = enabled; return this; }
        public Builder profilingEnabled(boolean profilingEnabled) { this.profilingEnabled = profilingEnabled; return this; }
        public Builder profilingTypes(java.util.List<String> types) { this.profilingTypes = types; return this; }
        public Builder profilingPeriodSeconds(int seconds) { this.profilingPeriodSeconds = seconds; return this; }
        public Builder llmCaptureContent(Boolean capture) { this.llmCaptureContent = capture; return this; }
        public TrooperConfig build() { return new TrooperConfig(this); }
    }
}
