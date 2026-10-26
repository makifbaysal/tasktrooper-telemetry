plugins {
    java
}

evaluationDependsOn(":trooper-otel-extension")

description = "trooper-javaagent.jar: upstream opentelemetry-javaagent with the TaskTrooper extension embedded"

val upstreamAgent: Configuration by configurations.creating {
    isTransitive = false
}

val testApp: SourceSet = sourceSets.create("testApp")

dependencies {
    upstreamAgent(libs.otel.javaagent)

    testImplementation(platform(libs.otel.bom))
    testImplementation(libs.otel.proto)
}

val extensionJar = project(":trooper-otel-extension").tasks.named<Jar>("jar")

// "Distro" pattern from opentelemetry-java-instrumentation/examples/extension: the agent loads every jar found under
// extensions/ inside its own jar.
val trooperJavaagentJar by tasks.registering(Jar::class) {
    group = "build"
    description = "Builds trooper-javaagent.jar"
    archiveFileName.set("trooper-javaagent.jar")
    destinationDirectory.set(layout.buildDirectory.dir("libs"))
    dependsOn(extensionJar)
    from(provider { zipTree(upstreamAgent.singleFile) }) {
        exclude("META-INF/MANIFEST.MF")
    }
    from(extensionJar) {
        into("extensions")
        rename { "trooper-otel-extension.jar" }
    }
    manifest {
        attributes(
            "Main-Class" to "io.opentelemetry.javaagent.OpenTelemetryAgent",
            "Agent-Class" to "io.opentelemetry.javaagent.OpenTelemetryAgent",
            "Premain-Class" to "io.opentelemetry.javaagent.OpenTelemetryAgent",
            "Can-Redefine-Classes" to "true",
            "Can-Retransform-Classes" to "true",
            "Implementation-Title" to "trooper-javaagent",
            "Implementation-Vendor" to "TaskTrooper",
            "Implementation-Version" to project.version.toString(),
            "Automatic-Module-Name" to "io.opentelemetry.javaagent",
        )
    }
    duplicatesStrategy = DuplicatesStrategy.EXCLUDE
}

tasks.assemble { dependsOn(trooperJavaagentJar) }

tasks.test {
    dependsOn(trooperJavaagentJar, tasks.named("testAppClasses"))
    systemProperty("trooper.agentJar", trooperJavaagentJar.get().archiveFile.get().asFile.absolutePath)
    systemProperty("trooper.testAppClasspath", testApp.runtimeClasspath.asPath)
    inputs.file(trooperJavaagentJar.flatMap { it.archiveFile })
}
