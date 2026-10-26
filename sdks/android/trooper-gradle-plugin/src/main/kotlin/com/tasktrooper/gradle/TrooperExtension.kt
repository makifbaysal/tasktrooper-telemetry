package com.tasktrooper.gradle

import org.gradle.api.provider.Property

/**
 * `trooper { }` configuration.
 *
 * ```kotlin
 * trooper {
 *     apiUrl.set("https://symbolicator.example.com")   // or env TROOPER_API_URL
 *     apiKey.set(providers.environmentVariable("TROOPER_API_KEY"))
 *     app.set("courier-app")                            // = TrooperConfig.app
 * }
 * ```
 */
abstract class TrooperExtension {
    /** Symbolicator base URL; `POST <apiUrl>/v1/artifacts`. Defaults to env `TROOPER_API_URL`. */
    abstract val apiUrl: Property<String>

    /** API key sent as `Authorization: Bearer`. Defaults to env `TROOPER_API_KEY`. */
    abstract val apiKey: Property<String>

    /** App name, must equal `TrooperConfig.app` (RUM `resource.app`). Defaults to the project name. */
    abstract val app: Property<String>

    /** Overrides the release; default `versionName+versionCode` (= RUM `resource.version` default). */
    abstract val release: Property<String>

    /** Default true. */
    abstract val enabled: Property<Boolean>

    /** Default false: upload failures only log a warning and never break the build. */
    abstract val failOnError: Property<Boolean>
}
