import com.vanniktech.maven.publish.MavenPublishBaseExtension

plugins {
    id("com.android.library") version "8.13.2" apply false
    id("com.android.application") version "8.13.2" apply false
    id("org.jetbrains.kotlin.android") version "2.2.20" apply false
    id("org.jetbrains.kotlin.plugin.compose") version "2.2.20" apply false
    // 0.35.x: the last line supporting Gradle 8.x (0.36+ needs Gradle 9). Keep in sync with trooper-gradle-plugin.
    id("com.vanniktech.maven.publish") version "0.35.0" apply false
}

// Version: -PVERSION_NAME / VERSION_NAME env (release CI) > -Pversion > gradle.properties.
val sdkVersion: String = providers.gradleProperty("VERSION_NAME")
    .orElse(providers.environmentVariable("VERSION_NAME"))
    .orElse(providers.gradleProperty("version"))
    .get()

allprojects {
    group = "ai.tasktrooper"
    version = sdkVersion
}

// The mapping-upload Gradle plugin is an included build; run its tests with the root `build`.
tasks.register("build") {
    group = "build"
    dependsOn(gradle.includedBuild("trooper-gradle-plugin").task(":build"))
}

// Maven Central (Sonatype Central Portal) via com.vanniktech.maven.publish: every Android library module publishes its
// release variant AAR (+ sources + javadoc jars, signed, full POM) as ai.tasktrooper:<module>:<version>; the Gradle
// plugin (included build) publishes ai.tasktrooper:trooper-gradle-plugin plus the `ai.tasktrooper.mapping-upload`
// plugin marker. The sample app is not published. Credentials / signing key come only from
// ORG_GRADLE_PROJECT_{mavenCentralUsername,mavenCentralPassword,signingInMemoryKey,signingInMemoryKeyPassword};
// build / publishToMavenLocal work without them. Signing happens when a key is configured or a Central task is
// requested (then a missing key fails the build, as it should).
val signPublications: Boolean = providers.gradleProperty("signingInMemoryKey").isPresent ||
    gradle.startParameter.taskNames.any { it.contains("MavenCentral") }

subprojects {
    plugins.withId("com.android.library") {
        apply(plugin = "com.vanniktech.maven.publish")
        extensions.configure<MavenPublishBaseExtension> {
            publishToMavenCentral(automaticRelease = true)
            if (signPublications) signAllPublications()
            pom {
                name.set(project.name)
                description.set(provider { project.description ?: "TaskTrooper Telemetry Android SDK: ${project.name}" })
                url.set("https://tasktrooper.ai")
                inceptionYear.set("2026")
                licenses {
                    license {
                        name.set("Apache-2.0")
                        url.set("https://www.apache.org/licenses/LICENSE-2.0.txt")
                        distribution.set("repo")
                    }
                }
                developers {
                    developer {
                        id.set("makifbaysal")
                        name.set("Mehmet Akif Baysal")
                        url.set("https://github.com/makifbaysal")
                    }
                }
                scm {
                    url.set("https://github.com/makifbaysal/tasktrooper-telemetry")
                    connection.set("scm:git:https://github.com/makifbaysal/tasktrooper-telemetry.git")
                    developerConnection.set("scm:git:ssh://git@github.com/makifbaysal/tasktrooper-telemetry.git")
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

tasks.register("publishToMavenCentral") {
    group = "publishing"
    description = "Publishes all SDK modules and the mapping-upload Gradle plugin to Maven Central."
    dependsOn(gradle.includedBuild("trooper-gradle-plugin").task(":publishToMavenCentral"))
}
