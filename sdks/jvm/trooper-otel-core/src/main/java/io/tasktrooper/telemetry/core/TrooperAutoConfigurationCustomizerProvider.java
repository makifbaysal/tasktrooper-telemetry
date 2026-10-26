package io.tasktrooper.telemetry.core;

import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizer;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider;
import java.util.function.Supplier;

/**
 * OTel autoconfigure SPI entry point. Registered as a service by the javaagent extension and exposed as a bean by the
 * Spring Boot starter.
 */
public class TrooperAutoConfigurationCustomizerProvider implements AutoConfigurationCustomizerProvider {
    private final Supplier<TrooperConfig> config;

    /** SPI constructor: configuration from {@code -Dtrooper.*} / {@code TROOPER_*}. */
    public TrooperAutoConfigurationCustomizerProvider() {
        this(TrooperConfig::fromSystem);
    }

    public TrooperAutoConfigurationCustomizerProvider(Supplier<TrooperConfig> config) {
        this.config = config;
    }

    @Override
    public void customize(AutoConfigurationCustomizer customizer) {
        TrooperCustomizations.apply(customizer, config.get());
    }

    @Override
    public int order() {
        return 0;
    }
}
