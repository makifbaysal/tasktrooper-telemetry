package com.tasktrooper.android.internal

import java.util.Random
import java.util.UUID

/**
 * Session rotation per rum.proto: new session after 30 min of inactivity or 4 h total.
 * Sampling is decided once per session. A session adopted from a hybrid layer ([adopt]) is EXTERNAL: that layer
 * owns its lifetime, so it is never rotated here (contracts.md §9).
 */
internal class SessionManager(
    private val clock: Clock,
    private val store: KeyValueStore,
    private val sampleRate: Double,
    private val random: Random = Random(),
    private val inactivityMs: Long = 30 * 60_000L,
    private val maxDurationMs: Long = 4 * 60 * 60_000L,
) {
    data class Session(val id: String, val startedMs: Long, val sampled: Boolean, val external: Boolean = false)

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
        val expired = s == null ||
            (!s.external && (now - lastActivityMs >= inactivityMs || now - s.startedMs >= maxDurationMs))
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

    /**
     * Hybrid bridge: continue a session started by the Flutter/RN layer. It becomes EXTERNAL (never rotated here,
     * also when it is the id we already have). [sampled] = that layer's sampling decision; null keeps ours.
     */
    @Synchronized
    fun adopt(sessionId: String, sampled: Boolean? = null) {
        val s = current
        val now = clock.nowMs()
        lastActivityMs = now
        if (s?.id == sessionId) {
            current = s.copy(sampled = sampled ?: s.sampled, external = true)
            return
        }
        val decided = sampled ?: s?.sampled ?: (sampleRate >= 1.0 || (sampleRate > 0.0 && random.nextDouble() < sampleRate))
        current = Session(sessionId, now, decided, external = true)
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
