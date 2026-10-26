import java.util.Properties

plugins {
    id("com.android.library")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.tasktrooper.android.ndk"
    compileSdk = 36
    ndkVersion = "27.0.12077973"
    defaultConfig {
        minSdk = 21
        consumerProguardFiles("consumer-rules.pro")
        externalNativeBuild { cmake { arguments += listOf("-DANDROID_STL=none") } }
    }
    externalNativeBuild {
        cmake {
            path = file("src/main/cpp/CMakeLists.txt")
            version = "3.22.1"
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_11
        targetCompatibility = JavaVersion.VERSION_11
    }
}

kotlin { jvmToolchain(21); compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_11) } }

dependencies {
    // `api`: apps add only trooper-ndk and get the core SDK too.
    api(project(":trooper-android"))
}

// Host harness: builds trooper_crash.c for the build machine (macOS/Linux) and runs it. Each case forks a
// child that crashes for real (SIGSEGV on a worker thread, SIGABRT, trap, chained previous handler) and
// checks the report. Skipped on Windows or when no CMake is found.
val hostBuildDir = layout.buildDirectory.dir("host-native")
val sdkCmake: File? = run {
    val props = Properties()
    val lp = rootProject.file("local.properties")
    if (lp.exists()) lp.inputStream().use { props.load(it) }
    val sdk = props.getProperty("sdk.dir") ?: System.getenv("ANDROID_HOME")
    sdk?.let { File(it, "cmake/3.22.1/bin") }?.takeIf { File(it, "cmake").exists() }
}
val hostNativeTest by tasks.registering(Exec::class) {
    group = "verification"
    description = "Builds and runs the native crash handler harness on the host."
    val src = file("src/hostTest/cpp")
    inputs.dir(src)
    inputs.dir(file("src/main/cpp"))
    val out = hostBuildDir
    outputs.dir(out)
    onlyIf { !org.gradle.internal.os.OperatingSystem.current().isWindows }
    val cmakeDir = sdkCmake
    val cmake = cmakeDir?.let { File(it, "cmake").path } ?: "cmake"
    val ninja = cmakeDir?.let { File(it, "ninja").path }
    doFirst { out.get().asFile.mkdirs() }
    val b = out.get().asFile.path
    val configure = buildString {
        append("'$cmake' -S '${src.path}' -B '$b'")
        if (ninja != null) append(" -G Ninja -DCMAKE_MAKE_PROGRAM='$ninja'")
    }
    commandLine("sh", "-c", "$configure >/dev/null && '$cmake' --build '$b' && '$b/crash_harness' '$b/reports'")
}
tasks.named("check") { dependsOn(hostNativeTest) }
