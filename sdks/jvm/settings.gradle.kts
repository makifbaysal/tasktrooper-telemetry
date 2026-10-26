pluginManagement {
    repositories {
        gradlePluginPortal()
        mavenCentral()
    }
}

dependencyResolutionManagement {
    repositories {
        mavenCentral()
    }
}

rootProject.name = "tasktrooper-jvm"

include(
    "trooper-otel-core",
    "trooper-otel-extension",
    "trooper-javaagent",
    "trooper-spring-boot-starter",
    "trooper-quarkus",
    "trooper-kotlin",
    "trooper-ktor",
    "examples:spring-boot-app",
    "examples:quarkus-app",
    "examples:ktor-app",
)
