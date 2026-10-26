plugins {
    alias(libs.plugins.kotlin.jvm)
    `java-library`
}

description = "TaskTrooper Ktor 3 server/client plugins (built on opentelemetry-ktor-3.0)"

dependencies {
    api(project(":trooper-kotlin"))
    api(platform(libs.otel.instrumentation.bom.alpha))
    api(libs.otel.ktor3)
    implementation("io.opentelemetry.instrumentation:opentelemetry-instrumentation-api")
    api(platform(libs.ktor.bom))
    compileOnly("io.ktor:ktor-server-core")
    compileOnly("io.ktor:ktor-client-core")

    testImplementation("io.ktor:ktor-server-test-host")
    testImplementation("io.ktor:ktor-client-core")
    testImplementation(libs.otel.sdk.testing)
}
