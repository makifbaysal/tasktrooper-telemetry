package com.tasktrooper.gradle

import com.android.build.api.artifact.SingleArtifact
import com.android.build.api.variant.ApplicationAndroidComponentsExtension
import com.android.build.api.variant.VariantOutputConfiguration
import org.gradle.api.Project

internal object AndroidWiring {
    fun wire(project: Project, ext: TrooperExtension) {
        val components = project.extensions.getByType(ApplicationAndroidComponentsExtension::class.java)
        components.onVariants { variant ->
            if (!variant.isMinifyEnabled) return@onVariants
            val cap = variant.name.replaceFirstChar { it.uppercase() }
            val output = variant.outputs.firstOrNull { it.outputType == VariantOutputConfiguration.OutputType.SINGLE }
                ?: variant.outputs.firstOrNull()
            // Same format as trooper-android's default RUM resource.version: "${versionName ?: ""}+$versionCode".
            val defaultRelease = if (output == null) null else
                output.versionName.orElse("").zip(output.versionCode) { name, code -> "$name+$code" }
            val enabled = ext.enabled // capture the Provider, not the extension (configuration cache)
            val upload = project.tasks.register("uploadTrooperMapping$cap", TrooperMappingUploadTask::class.java) { t ->
                t.description = "Uploads the ${variant.name} R8 mapping file to TaskTrooper."
                t.mappingFile.set(variant.artifacts.get(SingleArtifact.OBFUSCATION_MAPPING_FILE))
                if (defaultRelease != null) t.release.set(ext.release.orElse(defaultRelease)) else t.release.set(ext.release)
                t.onlyIf("trooper.enabled") { enabled.get() }
            }
            val minifyName = "minify${cap}WithR8"
            project.tasks.matching { it.name == minifyName }.configureEach { it.finalizedBy(upload) }
        }
    }
}
