plugins {
    java
}

description = "Example: Quarkus 3 + quarkus-opentelemetry + trooper-quarkus (compiled only; run it with the Quarkus Gradle/Maven plugin)"

dependencies {
    implementation(project(":trooper-quarkus"))
    implementation(platform(libs.quarkus.bom))
    implementation("io.quarkus:quarkus-rest")
    implementation("io.quarkus:quarkus-opentelemetry")
}
