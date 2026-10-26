package com.tasktrooper.gradle

import org.gradle.testkit.runner.GradleRunner
import org.gradle.testkit.runner.TaskOutcome
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File

/**
 * TestKit: the plugin is resolved from `build/test-repo` (published by the test task), so it lives in the
 * same classloader as AGP inside the fixture, exactly as for users.
 */
class MappingUploadFunctionalTest {
    @get:Rule val tmp = TemporaryFolder()

    private val repo = System.getProperty("trooper.testRepo")
    private val version = System.getProperty("trooper.pluginVersion")

    private fun settings(dir: File) = File(dir, "settings.gradle.kts").writeText("""
        pluginManagement {
            repositories { maven { url = uri("${repo.replace("\\", "/")}") }; google(); mavenCentral(); gradlePluginPortal() }
        }
        dependencyResolutionManagement { repositories { google(); mavenCentral() } }
        rootProject.name = "fixture"
    """.trimIndent())

    private fun runner(dir: File, vararg args: String) = GradleRunner.create()
        .withProjectDir(dir).withArguments(*args, "--stacktrace", "--configuration-cache").forwardOutput()

    @Test fun manualTaskUploadsWithExtensionAndEnvDefaults() = FakeSymbolicator().use { server ->
        val dir = tmp.newFolder("manual")
        settings(dir)
        File(dir, "mapping.txt").writeText("a.B -> c:\n")
        File(dir, "build.gradle.kts").writeText("""
            import com.tasktrooper.gradle.TrooperMappingUploadTask
            plugins { id("com.tasktrooper.mapping-upload") version "$version" }
            trooper { app.set("courier-app") }
            tasks.register<TrooperMappingUploadTask>("uploadMapping") {
                mappingFile.set(file("mapping.txt"))
                release.set("2.0.0+7")
            }
        """.trimIndent())
        val result = runner(dir, "uploadMapping")
            .withEnvironment(System.getenv() + mapOf("TROOPER_API_URL" to server.url, "TROOPER_API_KEY" to "env_key"))
            .build()
        assertEquals(TaskOutcome.SUCCESS, result.task(":uploadMapping")!!.outcome)
        val req = server.requests.single()
        assertEquals("Bearer env_key", req.headers["authorization"])
        val parts = req.parts()
        assertEquals("r8", parts["kind"]!!.second)
        assertEquals("courier-app", parts["app"]!!.second)
        assertEquals("2.0.0+7", parts["release"]!!.second)
        assertEquals("a.B -> c:\n", parts["file"]!!.second)
    }

    @Test fun missingCredentialsSkipWithWarning() {
        val dir = tmp.newFolder("nocreds")
        settings(dir)
        File(dir, "mapping.txt").writeText("x")
        File(dir, "build.gradle.kts").writeText("""
            import com.tasktrooper.gradle.TrooperMappingUploadTask
            plugins { id("com.tasktrooper.mapping-upload") version "$version" }
            tasks.register<TrooperMappingUploadTask>("uploadMapping") { mappingFile.set(file("mapping.txt")); release.set("1+1") }
        """.trimIndent())
        val env = System.getenv().filterKeys { !it.startsWith("TROOPER_") }
        val result = runner(dir, "uploadMapping").withEnvironment(env).build()
        assertTrue(result.output.contains("mapping upload skipped"))
    }

    @Test fun failOnErrorFailsTheBuild() = FakeSymbolicator(status = 500).use { server ->
        val dir = tmp.newFolder("fail")
        settings(dir)
        File(dir, "mapping.txt").writeText("x")
        File(dir, "build.gradle.kts").writeText("""
            import com.tasktrooper.gradle.TrooperMappingUploadTask
            plugins { id("com.tasktrooper.mapping-upload") version "$version" }
            trooper { apiUrl.set("${server.url}"); apiKey.set("k"); app.set("a"); failOnError.set(true) }
            tasks.register<TrooperMappingUploadTask>("uploadMapping") { mappingFile.set(file("mapping.txt")); release.set("1+1") }
        """.trimIndent())
        val result = runner(dir, "uploadMapping").buildAndFail()
        assertTrue(result.output.contains("HTTP 500"))
    }

    /** Full AGP app: minifyReleaseWithR8 is finalized by uploadTrooperMappingRelease. Needs the Android SDK. */
    @Test fun androidAppUploadsMappingAfterR8() = FakeSymbolicator().use { server ->
        val sdk = System.getProperty("trooper.androidSdk")
        assumeTrue("Android SDK not found", sdk != null && File(sdk).isDirectory)
        assumeTrue(System.getProperty("trooper.agpFunctional") != "false")
        val dir = tmp.newFolder("app")
        settings(dir)
        File(dir, "local.properties").writeText("sdk.dir=${sdk!!.replace("\\", "/")}\n")
        File(dir, "gradle.properties").writeText("android.useAndroidX=true\norg.gradle.jvmargs=-Xmx2g\n")
        File(dir, "build.gradle.kts").writeText("""
            plugins {
                id("com.android.application") version "8.13.2"
                id("com.tasktrooper.mapping-upload") version "$version"
            }
            android {
                namespace = "com.example.fixture"
                compileSdk = 36
                defaultConfig { applicationId = "com.example.fixture"; minSdk = 21; versionName = "1.4.0"; versionCode = 812 }
                buildTypes { release { isMinifyEnabled = true; proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt")) } }
            }
            trooper { apiUrl.set("${server.url}"); apiKey.set("tt_key"); app.set("courier-app") }
        """.trimIndent())
        File(dir, "src/main").mkdirs()
        File(dir, "src/main/AndroidManifest.xml").writeText("""
            <manifest xmlns:android="http://schemas.android.com/apk/res/android">
                <application><activity android:name=".Main" android:exported="true">
                    <intent-filter><action android:name="android.intent.action.MAIN"/><category android:name="android.intent.category.LAUNCHER"/></intent-filter>
                </activity></application>
            </manifest>
        """.trimIndent())
        File(dir, "src/main/java/com/example/fixture").mkdirs()
        File(dir, "src/main/java/com/example/fixture/Main.java").writeText("""
            package com.example.fixture;
            public class Main extends android.app.Activity { static String helper(String s) { return s + "!"; } }
        """.trimIndent())
        val result = runner(dir, "minifyReleaseWithR8").build()
        assertEquals(TaskOutcome.SUCCESS, result.task(":uploadTrooperMappingRelease")!!.outcome)
        val parts = server.requests.single().parts()
        assertEquals("r8", parts["kind"]!!.second)
        assertEquals("courier-app", parts["app"]!!.second)
        assertEquals("1.4.0+812", parts["release"]!!.second)
        assertTrue(parts["file"]!!.second.contains("com.example.fixture.Main"))
        // Debug (not minified) gets no upload task.
        assertTrue(!result.output.contains("uploadTrooperMappingDebug"))
    }
}
