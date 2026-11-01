import com.vanniktech.maven.publish.MavenPublishBaseExtension

plugins {
    alias(libs.plugins.kotlin.jvm) apply false
    alias(libs.plugins.maven.publish) apply false
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

// Maven Central (Sonatype Central Portal) via com.vanniktech.maven.publish: every module except examples/ publishes
// ai.tasktrooper:<module>:<version> with sources + javadoc jars and a signed, complete POM. Credentials and the signing
// key come only from ORG_GRADLE_PROJECT_{mavenCentralUsername,mavenCentralPassword,signingInMemoryKey,
// signingInMemoryKeyPassword}; build / publishToMavenLocal work without them (signing is required for Central only).
// Sign when a key is configured or a Central task is requested (then a missing key fails the build, as it should).
val signPublications: Boolean = providers.gradleProperty("signingInMemoryKey").isPresent ||
    gradle.startParameter.taskNames.any { it.contains("MavenCentral") }

subprojects {
    if (path.startsWith(":examples")) return@subprojects
    apply(plugin = "com.vanniktech.maven.publish")
    extensions.configure<MavenPublishBaseExtension> {
        publishToMavenCentral(automaticRelease = true)
        if (signPublications) signAllPublications()
        pom {
            name.set(project.name)
            description.set(provider { project.description ?: "TaskTrooper Telemetry JVM SDK: ${project.name}" })
            url.set("https://telemetry.tasktrooper.ai")
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
    // Central rejects POM dependencies without a version; BOM-managed ones (platform(...)) get the resolved version.
    extensions.configure<PublishingExtension> {
        publications.withType<MavenPublication>().configureEach {
            versionMapping {
                usage("java-api") { fromResolutionOf("runtimeClasspath") }
                usage("java-runtime") { fromResolutionResult() }
            }
        }
    }
}

subprojects {
    plugins.withType<JavaPlugin> {
        extensions.configure<JavaPluginExtension> {
            withSourcesJar()
        }
        tasks.withType<JavaCompile>().configureEach {
            options.release.set(17)
            options.encoding = "UTF-8"
            options.compilerArgs.addAll(listOf("-Xlint:all,-serial,-processing,-options,-try", "-parameters"))
        }
        tasks.withType<Javadoc>().configureEach {
            (options as StandardJavadocDocletOptions).addStringOption("Xdoclint:all,-missing", "-quiet")
        }
        tasks.withType<Test>().configureEach {
            useJUnitPlatform()
            testLogging {
                events("failed")
                exceptionFormat = org.gradle.api.tasks.testing.logging.TestExceptionFormat.FULL
            }
        }
        val libs = rootProject.extensions.getByType<VersionCatalogsExtension>().named("libs")
        dependencies {
            "testImplementation"(platform(libs.findLibrary("junit-bom").get()))
            "testImplementation"("org.junit.jupiter:junit-jupiter")
            "testRuntimeOnly"("org.junit.platform:junit-platform-launcher")
            "testImplementation"(libs.findLibrary("assertj").get())
        }
    }
    plugins.withId("org.jetbrains.kotlin.jvm") {
        extensions.configure<org.jetbrains.kotlin.gradle.dsl.KotlinJvmProjectExtension> {
            compilerOptions {
                jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
                freeCompilerArgs.add("-Xjdk-release=17")
            }
        }
    }
}
