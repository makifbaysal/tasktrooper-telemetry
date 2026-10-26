package io.tasktrooper.telemetry.agent;

import io.tasktrooper.telemetry.core.TrooperAutoConfigurationCustomizerProvider;
import io.tasktrooper.telemetry.core.TrooperConfig;

/**
 * Javaagent entry point (SPI {@code AutoConfigurationCustomizerProvider}). Reads {@code TROOPER_DSN} /
 * {@code -Dtrooper.dsn} etc. and registers the TaskTrooper defaults, span processor, sampler and propagator.
 */
public final class TrooperAgentCustomizerProvider extends TrooperAutoConfigurationCustomizerProvider {
    public TrooperAgentCustomizerProvider() {
        super(TrooperConfig::fromSystem);
    }
}
