plugins {
    `java-library`
}

description = "TaskTrooper for Quarkus 3 (configures quarkus-opentelemetry; plain library, no deployment module)"

dependencies {
    api(project(":trooper-otel-core"))
    compileOnly(platform(libs.quarkus.bom))
    compileOnly("io.quarkus:quarkus-opentelemetry")
    compileOnly("io.smallrye.config:smallrye-config")
    compileOnly("jakarta.ws.rs:jakarta.ws.rs-api")
    compileOnly("jakarta.enterprise:jakarta.enterprise.cdi-api")

    testImplementation(platform(libs.quarkus.bom))
    testImplementation("io.smallrye.config:smallrye-config")
    testImplementation("jakarta.ws.rs:jakarta.ws.rs-api")
    testImplementation(libs.otel.sdk.testing)
    testImplementation(libs.otel.autoconfigure.spi)
}
