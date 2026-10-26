plugins {
    `java-library`
}

description = "TaskTrooper OpenTelemetry javaagent extension (use with -Dotel.javaagent.extensions or via trooper-javaagent.jar)"

dependencies {
    // Bundled into the extension jar; the OTel API/SDK classes are provided by the agent at runtime.
    implementation(project(":trooper-otel-core"))
    compileOnly(libs.otel.autoconfigure.spi)
    compileOnly(libs.otel.autoconfigure)
    compileOnly(libs.otel.javaagent.extension.api)

    testImplementation(libs.otel.autoconfigure)
    testImplementation(libs.otel.exporter.otlp)
    testImplementation(libs.otel.sdk.testing)
}

// The extension jar must contain only our classes (core + extension): never OTel itself.
tasks.jar {
    archiveBaseName.set("trooper-otel-extension")
    dependsOn(":trooper-otel-core:jar")
    from(provider { zipTree(project(":trooper-otel-core").tasks.named<Jar>("jar").get().archiveFile) }) {
        exclude("META-INF/MANIFEST.MF")
    }
    duplicatesStrategy = DuplicatesStrategy.EXCLUDE
}
