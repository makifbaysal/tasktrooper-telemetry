package com.tasktrooper.gradle

import org.gradle.api.DefaultTask
import org.gradle.api.GradleException
import org.gradle.api.file.RegularFileProperty
import org.gradle.api.provider.Property
import org.gradle.api.tasks.Input
import org.gradle.api.tasks.InputFile
import org.gradle.api.tasks.Internal
import org.gradle.api.tasks.Optional
import org.gradle.api.tasks.PathSensitive
import org.gradle.api.tasks.PathSensitivity
import org.gradle.api.tasks.TaskAction
import org.gradle.work.DisableCachingByDefault

/**
 * Uploads one R8/ProGuard mapping file. Registered automatically per minified Android variant
 * (`uploadTrooperMapping<Variant>`); can also be registered manually for non-AGP setups:
 *
 * ```kotlin
 * tasks.register<TrooperMappingUploadTask>("uploadMapping") {
 *     mappingFile.set(file("build/outputs/mapping/release/mapping.txt"))
 *     apiUrl.set("..."); apiKey.set("..."); app.set("courier-app"); release.set("1.4.0+812")
 * }
 * ```
 */
@DisableCachingByDefault(because = "Uploads to a remote service")
abstract class TrooperMappingUploadTask : DefaultTask() {
    @get:InputFile @get:Optional @get:PathSensitive(PathSensitivity.NONE)
    abstract val mappingFile: RegularFileProperty

    @get:Input @get:Optional abstract val apiUrl: Property<String>
    @get:Internal abstract val apiKey: Property<String>
    @get:Input @get:Optional abstract val app: Property<String>
    @get:Input @get:Optional abstract val release: Property<String>
    @get:Input abstract val kind: Property<String>
    @get:Input abstract val failOnError: Property<Boolean>

    init {
        group = "tasktrooper"
        description = "Uploads the R8 mapping file to the TaskTrooper symbolicator."
        kind.convention("r8")
        failOnError.convention(false)
        outputs.upToDateWhen { false }
    }

    @TaskAction
    fun upload() {
        val url = apiUrl.orNull?.takeIf { it.isNotBlank() }
        val key = apiKey.orNull?.takeIf { it.isNotBlank() }
        if (url == null || key == null) {
            logger.warn("TaskTrooper: trooper.apiUrl / trooper.apiKey (or TROOPER_API_URL / TROOPER_API_KEY) not set, mapping upload skipped")
            return
        }
        val file = mappingFile.orNull?.asFile
        if (file == null || !file.isFile) {
            fail("mapping file not found (${file ?: "unset"})"); return
        }
        val appName = app.orNull?.takeIf { it.isNotBlank() } ?: run { fail("trooper.app is not set"); return }
        val rel = release.orNull?.takeIf { it.isNotBlank() } ?: run { fail("release could not be determined (set trooper.release)"); return }
        val r = MultipartUploader.upload(url, key, kind.get(), appName, rel, file, "mapping.txt")
        if (r.ok) {
            logger.lifecycle("TaskTrooper: uploaded ${kind.get()} mapping for $appName $rel (HTTP ${r.status})")
        } else {
            fail("mapping upload for $appName $rel failed: " + if (r.status < 0) r.body else "HTTP ${r.status} ${r.body}")
        }
    }

    private fun fail(msg: String) {
        if (failOnError.get()) throw GradleException("TaskTrooper: $msg")
        logger.warn("TaskTrooper: $msg")
    }
}
