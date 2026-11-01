import com.vanniktech.maven.publish.MavenPublishBaseExtension
import java.util.Properties

plugins {
    `java-gradle-plugin`
    id("org.jetbrains.kotlin.jvm") version "2.2.20"
    // Same version as the SDK root build (sdks/android/build.gradle.kts).
    id("com.vanniktech.maven.publish") version "0.35.0"
}

description = "TaskTrooper Gradle plugin: uploads R8 mapping.txt to the TaskTrooper symbolicator"

// Same version as the SDK modules: -PVERSION_NAME / VERSION_NAME env > -Pversion > ../gradle.properties (this
// included build does not read the parent build's gradle.properties by itself).
val parentProperties = Properties().apply {
    rootDir.parentFile.resolve("gradle.properties").takeIf { it.isFile }?.inputStream()?.use { load(it) }
}
group = "ai.tasktrooper"
version = providers.gradleProperty("VERSION_NAME")
    .orElse(providers.environmentVariable("VERSION_NAME"))
    .orElse(providers.gradleProperty("version"))
    .getOrElse(parentProperties.getProperty("version") ?: error("version missing in ../gradle.properties"))

// JDK 21 (same as the SDK modules), bytecode 17 = the minimum JDK for AGP 8.
kotlin {
    jvmToolchain(21)
    compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) }
}
tasks.withType<JavaCompile>().configureEach { options.release.set(17) }

dependencies {
    compileOnly("com.android.tools.build:gradle-api:8.13.2")
    testImplementation(gradleTestKit())
    testImplementation("junit:junit:4.13.2")
}

gradlePlugin {
    website.set("https://tasktrooper.ai")
    vcsUrl.set("https://github.com/makifbaysal/tasktrooper-telemetry")
    plugins {
        create("mappingUpload") {
            id = "ai.tasktrooper.mapping-upload"
            implementationClass = "com.tasktrooper.gradle.TrooperMappingUploadPlugin"
            displayName = "TaskTrooper R8 mapping upload"
            description = "Uploads R8 mapping.txt to the TaskTrooper symbolicator after minify<Variant>WithR8."
            tags.set(listOf("android", "r8", "proguard", "mapping", "tasktrooper"))
        }
    }
}

// Maven Central: ai.tasktrooper:trooper-gradle-plugin + the ai.tasktrooper.mapping-upload plugin marker (so
// `plugins { id("ai.tasktrooper.mapping-upload") version "…" }` resolves from mavenCentral()). Credentials and signing
// as in the SDK root build: ORG_GRADLE_PROJECT_* env only; signing when a key is set or a Central task is requested.
val requestedTasks = (gradle.startParameter.taskNames + (gradle.parent?.startParameter?.taskNames ?: emptyList()))
val signPublications: Boolean = providers.gradleProperty("signingInMemoryKey").isPresent ||
    requestedTasks.any { it.contains("MavenCentral") }
extensions.configure<MavenPublishBaseExtension> {
    publishToMavenCentral(automaticRelease = true)
    if (signPublications) signAllPublications()
    pom {
        name.set(project.name)
        description.set(project.description)
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

tasks.test {
    // The AGP functional test needs the Android SDK; it is skipped when none is found.
    val sdk = System.getenv("ANDROID_HOME") ?: rootDir.parentFile.resolve("local.properties").takeIf { it.isFile }
        ?.readLines()?.firstOrNull { it.startsWith("sdk.dir=") }?.substringAfter("sdk.dir=")
    if (sdk != null) systemProperty("trooper.androidSdk", sdk)
    systemProperty("trooper.agpFunctional", System.getProperty("trooper.agpFunctional") ?: "true")
}

// Functional tests resolve the plugin (with its marker) from a throwaway local repo, so that it shares a
// classloader with AGP inside the fixture build (withPluginClasspath() would isolate it from AGP's classes).
val testRepo = layout.buildDirectory.dir("test-repo")
extensions.configure<PublishingExtension> {
    repositories { maven { name = "test"; url = uri(testRepo) } }
}
tasks.test {
    dependsOn("publishAllPublicationsToTestRepository")
    systemProperty("trooper.testRepo", testRepo.get().asFile.absolutePath)
    systemProperty("trooper.pluginVersion", version.toString())
}
