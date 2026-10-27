package io.tasktrooper.telemetry.spring;

import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperConfig;
import java.util.LinkedHashMap;
import java.util.Map;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.env.EnvironmentPostProcessor;
import org.springframework.core.Ordered;
import org.springframework.core.env.ConfigurableEnvironment;
import org.springframework.core.env.MapPropertySource;

/**
 * Translates {@code trooper.*} (application.yml, or {@code TROOPER_*} env vars via relaxed binding) into {@code otel.*}
 * defaults for the OpenTelemetry Spring Boot starter. Added with the lowest precedence: explicit {@code otel.*} wins.
 * {@code trooper.profiling.*} starts the JFR profiler once the SDK resource is built; {@code trooper.llm.capture-content}
 * also configures {@code TrooperLlm}.
 */
public class TrooperEnvironmentPostProcessor implements EnvironmentPostProcessor, Ordered {
    public static final String PROPERTY_SOURCE = "trooperOtelDefaults";

    @Override
    public void postProcessEnvironment(ConfigurableEnvironment environment, SpringApplication application) {
        try {
            TrooperConfig config = config(environment);
            Map<String, Object> props = new LinkedHashMap<>(config.toOtelProperties());
            environment.getPropertySources().remove(PROPERTY_SOURCE);
            environment.getPropertySources().addLast(new MapPropertySource(PROPERTY_SOURCE, props));
            // TrooperLlm / TrooperChatModelListener read this; null keeps the -D / TROOPER_* lookup
            io.tasktrooper.telemetry.llm.TrooperLlm.setCaptureContent(config.llmCaptureContent());
        } catch (Throwable t) {
            SdkErrors.record("spring_config", t); // never break application startup
        }
    }

    static TrooperConfig config(org.springframework.core.env.Environment environment) {
        return TrooperConfig.fromLookup(key -> {
            String v = environment.getProperty(key);
            if (v == null && TrooperConfig.SERVICE.equals(key)) {
                v = environment.getProperty("spring.application.name");
            }
            if (v == null && TrooperConfig.ENVIRONMENT.equals(key)) {
                String[] profiles = environment.getActiveProfiles();
                v = profiles.length == 1 ? profiles[0] : null;
            }
            return v;
        });
    }

    @Override
    public int getOrder() {
        return Ordered.LOWEST_PRECEDENCE;
    }
}
