plugins {
    id("com.android.library") version "8.13.2" apply false
    id("com.android.application") version "8.13.2" apply false
    id("org.jetbrains.kotlin.android") version "2.2.20" apply false
    id("org.jetbrains.kotlin.plugin.compose") version "2.2.20" apply false
}

allprojects {
    group = "com.tasktrooper"
    version = "0.1.0"
}

// The mapping-upload Gradle plugin is an included build; run its tests with the root `build`.
tasks.register("build") {
    group = "build"
    dependsOn(gradle.includedBuild("trooper-gradle-plugin").task(":build"))
}

// Maven publishing (`./gradlew publishToMavenLocal`): every Android library module publishes its release
// variant (+ sources) as com.tasktrooper:<module>:<version>; the Gradle plugin (included build) publishes
// com.tasktrooper:trooper-gradle-plugin plus the `com.tasktrooper.mapping-upload` plugin marker.
subprojects {
    plugins.withId("com.android.library") {
        apply(plugin = "maven-publish")
        extensions.configure<com.android.build.api.dsl.LibraryExtension> {
            publishing { singleVariant("release") { withSourcesJar() } }
        }
        afterEvaluate {
            extensions.configure<PublishingExtension> {
                publications {
                    create<MavenPublication>("release") {
                        from(components["release"])
                        artifactId = project.name
                        pom {
                            name.set(project.name)
                            description.set("TaskTrooper Telemetry Android SDK: ${project.name}")
                            licenses { license { name.set("Apache-2.0") } }
                        }
                    }
                }
            }
        }
    }
}

tasks.register("publishToMavenLocal") {
    group = "publishing"
    description = "Publishes all SDK modules and the mapping-upload Gradle plugin to ~/.m2."
    dependsOn(gradle.includedBuild("trooper-gradle-plugin").task(":publishToMavenLocal"))
}
