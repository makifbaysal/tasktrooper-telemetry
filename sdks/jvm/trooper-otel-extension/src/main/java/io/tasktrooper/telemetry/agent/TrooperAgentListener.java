package io.tasktrooper.telemetry.agent;

import io.opentelemetry.javaagent.extension.AgentListener;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import io.tasktrooper.telemetry.core.SdkErrors;

/** Registers the {@code trooper.sdk.errors} counter on the agent's SDK once it is built. */
public final class TrooperAgentListener implements AgentListener {
    @Override
    public void afterAgent(AutoConfiguredOpenTelemetrySdk sdk) {
        try {
            SdkErrors.bind(sdk.getOpenTelemetrySdk());
        } catch (Throwable ignored) {
            // never break agent startup
        }
    }
}
