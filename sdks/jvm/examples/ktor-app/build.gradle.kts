plugins {
    alias(libs.plugins.kotlin.jvm)
}

description = "Example: Ktor 3 + trooper-ktor"

dependencies {
    implementation(project(":trooper-ktor"))
    implementation(platform(libs.ktor.bom))
    implementation("io.ktor:ktor-server-netty")
    implementation("io.ktor:ktor-client-cio")
}

tasks.register<JavaExec>("run") {
    group = "application"
    classpath = sourceSets.main.get().runtimeClasspath
    mainClass.set("io.tasktrooper.examples.ktor.MainKt")
}
