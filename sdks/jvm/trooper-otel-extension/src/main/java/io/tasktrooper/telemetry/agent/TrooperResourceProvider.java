package io.tasktrooper.telemetry.agent;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.autoconfigure.spi.ResourceProvider;
import io.opentelemetry.sdk.resources.Resource;
import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperConfig;

/** Identifies the distro and adds environment/version from {@code trooper.*} when OTel config doesn't set them. */
public final class TrooperResourceProvider implements ResourceProvider {
    static final AttributeKey<String> DISTRO_NAME = AttributeKey.stringKey("telemetry.distro.name");
    static final AttributeKey<String> DISTRO_VERSION = AttributeKey.stringKey("telemetry.distro.version");
    static final AttributeKey<String> ENVIRONMENT = AttributeKey.stringKey("deployment.environment.name");
    static final AttributeKey<String> SERVICE_VERSION = AttributeKey.stringKey("service.version");

    @Override
    public Resource createResource(ConfigProperties config) {
        AttributesBuilder b = Attributes.builder().put(DISTRO_NAME, "tasktrooper-javaagent");
        String version = TrooperResourceProvider.class.getPackage().getImplementationVersion();
        if (version != null) {
            b.put(DISTRO_VERSION, version);
        }
        try {
            TrooperConfig cfg = TrooperConfig.fromSystem();
            if (cfg.environment() != null) b.put(ENVIRONMENT, cfg.environment());
            if (cfg.version() != null) b.put(SERVICE_VERSION, cfg.version());
        } catch (Throwable t) {
            SdkErrors.record("resource", t); // never break startup
        }
        return Resource.create(b.build());
    }

    @Override
    public int order() {
        return 1000; // after the upstream distro provider, so telemetry.distro.* identifies TaskTrooper
    }
}
