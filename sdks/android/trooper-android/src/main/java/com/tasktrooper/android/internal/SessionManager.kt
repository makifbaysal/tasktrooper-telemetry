package com.tasktrooper.android.internal

import java.util.Random
import java.util.UUID

/**
 * Session rotation per rum.proto: new session after 30 min of inactivity or 4 h total.
 * Sampling is decided once per session.
 */
internal class SessionManager(
    private val clock: Clock,
    private val store: KeyValueStore,
    private val sampleRate: Double,
    private val random: Random = Random(),
    private val inactivityMs: Long = 30 * 60_000L,
    private val maxDurationMs: Long = 4 * 60 * 60_000L,
) {
    data class Session(val id: String, val startedMs: Long, val sampled: Boolean)

    @Volatile var anonymousId: String = store.get(KEY_ANON) ?: UUID.randomUUID().toString().also { store.put(KEY_ANON, it) }
        private set

    private var current: Session? = null
    private var lastActivityMs = 0L

    /**
     * Returns the active session, rotating if expired. [isNew] is true when a session started on this call.
     * [extend] = false for passive events (network, vitals) so background traffic does not keep a session alive.
     */
    @Synchronized
    fun touch(extend: Boolean = true): Pair<Session, Boolean> {
        val now = clock.nowMs()
        val s = current
        val expired = s == null || now - lastActivityMs >= inactivityMs || now - s.startedMs >= maxDurationMs
        if (expired) {
            val sampled = sampleRate >= 1.0 || (sampleRate > 0.0 && random.nextDouble() < sampleRate)
            val fresh = Session(UUID.randomUUID().toString(), now, sampled)
            current = fresh
            lastActivityMs = now
            return fresh to true
        }
        if (extend) lastActivityMs = now
        return s!! to false
    }

    @Synchronized
    fun peek(): Session? = current

    /** Hybrid bridge: continue a session started by the Flutter/RN layer (keeps the sampling decision). */
    @Synchronized
    fun adopt(sessionId: String) {
        val s = current
        if (s?.id == sessionId) return
        val now = clock.nowMs()
        val sampled = s?.sampled ?: (sampleRate >= 1.0 || (sampleRate > 0.0 && random.nextDouble() < sampleRate))
        current = Session(sessionId, now, sampled)
        lastActivityMs = now
    }

    @Synchronized
    fun adoptAnonymousId(id: String) {
        if (id.isEmpty() || id == anonymousId) return
        anonymousId = id
        store.put(KEY_ANON, id)
    }

    companion object {
        const val KEY_ANON = "anonymous_id"
    }
}
