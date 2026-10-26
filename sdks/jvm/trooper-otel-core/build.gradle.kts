plugins {
    `java-library`
}

description = "TaskTrooper Telemetry: shared OpenTelemetry customizations (DSN, trooper.* attributes, sampler, propagator)"

dependencies {
    api(platform(libs.otel.bom))
    api(platform(libs.otel.bom.alpha))
    api(libs.otel.api)
    api(libs.otel.sdk)
    api(libs.otel.autoconfigure.spi)
    compileOnly(libs.otel.autoconfigure)
    compileOnly(libs.otel.exporter.otlp)

    testImplementation(libs.otel.sdk.testing)
    testImplementation(libs.otel.autoconfigure)
    testImplementation(libs.otel.exporter.otlp)
}
