package com.tasktrooper.android

/**
 * SDK configuration. Only [dsn] and [app] are required.
 *
 * @param dsn `https://<ingest-key>@<host>[:port]/<project_id>` (contracts.md §2).
 * @param tracePropagationTargets URLs matching any of these get W3C `traceparent` and `baggage: tt.svc=<app>`
 *   headers (OkHttp module).
 * @param beforeSend mutate or drop (return null) events before they are queued. Exceptions are swallowed.
 */
data class TrooperConfig @JvmOverloads constructor(
    val dsn: String,
    val app: String,
    val version: String = "",
    val environment: String = "production",
    val sampleRate: Double = 1.0,
    val tracePropagationTargets: List<Regex> = emptyList(),
    val trackActivities: Boolean = true,
    val trackFragments: Boolean = true,
    val trackAppStart: Boolean = true,
    val trackCrashes: Boolean = true,
    val trackAnr: Boolean = true,
    val anrThresholdMs: Long = 5_000,
    val trackFrames: Boolean = true,
    /** Flag taps with no navigation / request / layout change within 1 s as `dead` (ACTION is sent 1 s late). */
    val trackDeadTaps: Boolean = true,
    /** Native (NDK) crashes; effective only when the `trooper-ndk` artifact is on the classpath. */
    val trackNativeCrashes: Boolean = true,
    /** API 30+: report previous-process exits (ANR, native crash, low-memory kill) not caught otherwise. */
    val trackExitReasons: Boolean = true,
    val maxBatchSize: Int = 100,
    val flushIntervalMs: Long = 5_000,
    val maxQueueBytes: Long = 5L * 1024 * 1024,
    val resourceAttributes: Map<String, String> = emptyMap(),
    val beforeSend: ((RumEvent) -> RumEvent?)? = null,
    val debug: Boolean = false,
)

/** Parsed DSN: ingest endpoint + key. */
data class Dsn(val endpoint: String, val key: String, val projectId: String) {
    companion object {
        /** Returns null for malformed DSNs (the SDK then stays disabled instead of throwing). */
        @JvmStatic
        fun parse(dsn: String): Dsn? {
            val m = Regex("^(https?)://([^@/]+)@([^/]+)/([^/?#]+)/?$").find(dsn.trim()) ?: return null
            val (scheme, key, host, project) = m.destructured
            return Dsn("$scheme://$host/v1/rum", key, project)
        }
    }
}
