plugins {
    `java-gradle-plugin`
    id("org.jetbrains.kotlin.jvm") version "2.2.20"
}

group = "com.tasktrooper"
version = "0.1.0"

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
    plugins {
        create("mappingUpload") {
            id = "com.tasktrooper.mapping-upload"
            implementationClass = "com.tasktrooper.gradle.TrooperMappingUploadPlugin"
            displayName = "TaskTrooper R8 mapping upload"
            description = "Uploads R8 mapping.txt to the TaskTrooper symbolicator after minify<Variant>WithR8."
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
apply(plugin = "maven-publish")
val testRepo = layout.buildDirectory.dir("test-repo")
extensions.configure<PublishingExtension> {
    repositories { maven { name = "test"; url = uri(testRepo) } }
}
tasks.test {
    dependsOn("publishAllPublicationsToTestRepository")
    systemProperty("trooper.testRepo", testRepo.get().asFile.absolutePath)
    systemProperty("trooper.pluginVersion", version.toString())
}
