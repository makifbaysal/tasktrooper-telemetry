package com.tasktrooper.android.spi

import com.tasktrooper.android.RumResource
import com.tasktrooper.android.TaskTrooper
import com.tasktrooper.android.internal.Internal

/**
 * SDK-internal SPI used by optional modules (`trooper-replay`) to read the RUM session / view / user and
 * observe errors without depending on internals. Not a public API: may change without notice.
 * Every member is safe before `TaskTrooper.init` (returns null / no-op) and never throws.
 */
object TrooperSpi {
    /** Snapshot of the RUM context a replay segment is attributed to (contracts.md §23: `session_id` = RUM session). */
    class Context(
        val sessionId: String,
        /** False when the RUM session is not sampled: replay must not record either. */
        val sampled: Boolean,
        val anonymousId: String,
        val userId: String?,
        val viewId: String?,
        val viewName: String?,
    )

    /** Ingest endpoint derived from the DSN: [baseUrl] is `scheme://host[:port]` (no trailing slash). */
    class Ingest(val baseUrl: String, val key: String)

    fun interface ErrorListener {
        /**
         * An ERROR event of a sampled session was recorded. For uncaught exceptions ([isCrash]) this runs on
         * the crashing thread right before the crash event is persisted: write synchronously, do not post.
         */
        fun onError(tsMs: Long, isCrash: Boolean)
    }

    @JvmStatic val isInitialized: Boolean get() = TaskTrooper.core != null

    /** Current session (rotated if expired, not extended) + view + user; null before init. */
    @JvmStatic
    fun context(): Context? = Internal.guard("spi.context", null) {
        val core = TaskTrooper.core ?: return@guard null
        val s = core.replaySession()
        Context(s.id, s.sampled, core.sessions.anonymousId, core.user?.id, core.currentViewId, core.currentViewName)
    }

    @JvmStatic
    fun ingest(): Ingest? = Internal.guard("spi.ingest", null) {
        val d = TaskTrooper.dsn ?: return@guard null
        Ingest(d.endpoint.removeSuffix("/v1/rum"), d.key)
    }

    /** App / version / environment as sent on RUM batches. */
    @JvmStatic fun resource(): RumResource? = TaskTrooper.resource

    @JvmStatic
    fun addErrorListener(listener: ErrorListener) {
        TaskTrooper.core?.errorListeners?.addIfAbsent(listener)
    }

    @JvmStatic
    fun removeErrorListener(listener: ErrorListener) {
        TaskTrooper.core?.errorListeners?.remove(listener)
    }

    /** Counts and (with `debug = true`) logs an SDK-internal failure of an optional module. */
    @JvmStatic fun internalError(where: String, t: Throwable?) = Internal.error(where, t)

    @JvmStatic fun debug(message: String) = Internal.debug(message)

    @JvmStatic val isDebug: Boolean get() = Internal.debug
}
