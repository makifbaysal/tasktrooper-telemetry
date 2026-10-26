plugins {
    alias(libs.plugins.kotlin.jvm)
    `java-library`
}

description = "TaskTrooper Kotlin helpers: Trooper.init, withSpan, captureError, coroutine context propagation"

dependencies {
    api(project(":trooper-otel-core"))
    api(libs.otel.autoconfigure)
    api(libs.otel.exporter.otlp)
    api(libs.otel.extension.kotlin)
    api(platform(libs.coroutines.bom))
    api("org.jetbrains.kotlinx:kotlinx-coroutines-core")

    testImplementation(libs.otel.sdk.testing)
    testImplementation("org.jetbrains.kotlinx:kotlinx-coroutines-test")
}
