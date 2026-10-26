package com.tasktrooper.gradle

import org.gradle.api.Plugin
import org.gradle.api.Project

/**
 * `com.tasktrooper.mapping-upload`: after `minify<Variant>WithR8`, uploads `mapping.txt` to
 * `POST <apiUrl>/v1/artifacts` (kind=r8, app, release = versionName+versionCode).
 */
class TrooperMappingUploadPlugin : Plugin<Project> {
    override fun apply(project: Project) {
        val ext = project.extensions.create("trooper", TrooperExtension::class.java)
        val providers = project.providers
        ext.apiUrl.convention(providers.environmentVariable("TROOPER_API_URL"))
        ext.apiKey.convention(providers.environmentVariable("TROOPER_API_KEY"))
        ext.app.convention(project.name)
        ext.enabled.convention(true)
        ext.failOnError.convention(false)

        // Tasks registered by hand get the extension values as conventions.
        project.tasks.withType(TrooperMappingUploadTask::class.java).configureEach { t ->
            t.apiUrl.convention(ext.apiUrl)
            t.apiKey.convention(ext.apiKey)
            t.app.convention(ext.app)
            t.release.convention(ext.release)
            t.failOnError.convention(ext.failOnError)
        }

        // AGP classes are referenced only from AndroidWiring, loaded once AGP is applied.
        project.pluginManager.withPlugin("com.android.application") { AndroidWiring.wire(project, ext) }
    }
}
