pluginManagement {
    includeBuild("trooper-gradle-plugin")
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}
dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
    }
}
rootProject.name = "tasktrooper-android"
include(":trooper-android", ":trooper-ndk", ":trooper-okhttp", ":trooper-compose", ":trooper-android-sample")
