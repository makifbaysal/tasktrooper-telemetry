package com.tasktrooper.android.replay.internal

import com.tasktrooper.android.replay.ReplayOptions
import com.tasktrooper.android.spi.TrooperSpi
import java.io.ByteArrayOutputStream
import java.util.Random
import java.util.zip.GZIPOutputStream

internal interface ReplayStore {
    fun get(key: String): String?
    /** [sync]: write through before returning (crash path). */
    fun put(values: Map<String, String>, sync: Boolean)
}

internal fun interface SegmentSink {
    /** Persists one gzipped segment. [sync] = called from a crashing thread: write only, no upload. */
    fun write(gzipped: ByteArray, sync: Boolean)
}

/**
 * Sampling, on-error ring buffer and segmenting for wireframe replay (contracts.md §23). Pure JVM; every
 * method is synchronized because errors (and crashes) arrive on arbitrary threads.
 *
 * - Per RUM session: `sampleRate` → [Mode.RECORD]; else `onErrorSampleRate` → [Mode.BUFFER] (last 60 s in
 *   memory; on ERROR the buffer is flushed and recording continues); else [Mode.OFF]. Fixed for the session.
 * - A segment is sealed when it is ≥ 5 s old or ≥ 256 KB (uncompressed estimate), on [flush] (app in
 *   background), on session change and on crash. Every segment starts with a `screen` event.
 */
internal class ReplayEngine(
    private val options: ReplayOptions,
    private val clock: () -> Long,
    private val store: ReplayStore,
    private val sink: SegmentSink,
    private val context: () -> TrooperSpi.Context?,
    private val resource: () -> ResourceInfo?,
    private val random: Random = Random(),
) {
    enum class Mode { OFF, BUFFER, RECORD }

    class ResourceInfo(val service: String, val env: String, val version: String, val sdkVersion: String)

    private class Ev(val ts: Long, val json: String, val type: Int)

    private data class Screen(val widthDp: Int, val heightDp: Int, val density: Float, val name: String?)

    @Volatile var mode: Mode = Mode.OFF
        private set
    @Volatile var sessionId: String? = null
        private set
    var segment = 0
        private set

    private val events = ArrayList<Ev>()
    private var bytes = 0
    private var segmentContext: TrooperSpi.Context? = null
    private var lastContext: TrooperSpi.Context? = null
    private var clicks = 0
    private var errors = 0
    private var lastScreen: Screen? = null
    private var lastFrameKey: String? = null
    private var lastKeyboard: Boolean? = null

    @Volatile var stopped = false
        private set

    /** True when the UI should be captured at all (recording or buffering for errors). */
    val isCapturing: Boolean get() = mode != Mode.OFF && !stopped

    // ------------------------------------------------------------------ session

    /** Re-reads the RUM context; starts a new replay session (new sampling decision) when the id changed. */
    @Synchronized
    fun refreshSession(): Mode {
        if (stopped) return Mode.OFF
        val ctx = Guard.run("replay.context", null) { context() }
        if (ctx == null) {
            // SDK not initialized (yet): nothing to attribute segments to.
            if (sessionId == null) mode = Mode.OFF
            return mode
        }
        lastContext = ctx
        if (ctx.sessionId != sessionId) startSession(ctx)
        return mode
    }

    private fun startSession(ctx: TrooperSpi.Context) {
        if (mode == Mode.RECORD) seal(sync = false)
        resetBuffer()
        lastScreen = null; lastFrameKey = null; lastKeyboard = null
        sessionId = ctx.sessionId
        if (store.get(KEY_SESSION) == ctx.sessionId) {
            // Same session in a new process (or replay re-installed): continue numbering, keep the decision.
            segment = store.get(KEY_SEGMENT)?.toIntOrNull() ?: 0
            mode = store.get(KEY_MODE)?.let { m -> Mode.values().firstOrNull { it.name == m } } ?: decide(ctx)
        } else {
            segment = 0
            mode = decide(ctx)
        }
        persistState(sync = false)
    }

    private fun decide(ctx: TrooperSpi.Context): Mode = when {
        !options.enabled || !ctx.sampled -> Mode.OFF
        hit(options.sampleRate) -> Mode.RECORD
        hit(options.onErrorSampleRate) -> Mode.BUFFER
        else -> Mode.OFF
    }

    private fun hit(rate: Double) = rate >= 1.0 || (rate > 0.0 && random.nextDouble() < rate)

    private fun persistState(sync: Boolean) {
        val sid = sessionId ?: return
        Guard.run("replay.persist", Unit) {
            store.put(mapOf(KEY_SESSION to sid, KEY_SEGMENT to segment.toString(), KEY_MODE to mode.name), sync)
        }
    }

    // ------------------------------------------------------------------ input

    /**
     * A captured frame. [nodes] is a full wireframe; it is only recorded when it differs from the previous one.
     * [keyboardVisible] null = unknown.
     */
    @Synchronized
    fun onFrame(ts: Long, widthDp: Int, heightDp: Int, density: Float, nodes: List<WireNode>, keyboardVisible: Boolean?) {
        if (refreshSession() == Mode.OFF) return
        val screen = Screen(widthDp, heightDp, density, lastContext?.viewName)
        if (screen != lastScreen) {
            lastScreen = screen
            lastFrameKey = null // a new screen always gets a frame
            add(Ev(ts, WireEvents.screen(ts, widthDp, heightDp, density, screen.name), SCREEN))
        }
        if (keyboardVisible != null && keyboardVisible != lastKeyboard) {
            lastKeyboard = keyboardVisible
            add(Ev(ts, WireEvents.keyboard(ts, keyboardVisible), KEYBOARD))
        }
        val key = WireEvents.nodesKey(nodes)
        if (key != lastFrameKey) {
            lastFrameKey = key
            add(Ev(ts, "{\"type\":\"frame\",\"ts\":$ts,\"nodes\":$key}", FRAME))
        }
        maybeSeal(ts)
    }

    @Synchronized
    fun onTouch(ts: Long, xDp: Float, yDp: Float, phase: String) {
        if (mode == Mode.OFF || stopped) return
        if (phase == "down") clicks++
        add(Ev(ts, WireEvents.touch(ts, xDp, yDp, phase), TOUCH))
        maybeSeal(ts)
    }

    /** RUM ERROR of the current session. On crash ([isCrash]) everything is written synchronously. */
    @Synchronized
    fun onError(ts: Long, isCrash: Boolean) {
        if (stopped) return
        when (mode) {
            Mode.OFF -> return
            Mode.BUFFER -> {
                errors++
                mode = Mode.RECORD
                persistState(sync = isCrash)
                sealChunks(sync = isCrash)
            }
            Mode.RECORD -> {
                errors++
                if (isCrash) seal(sync = true)
            }
        }
    }

    /** Timer (≈1 s): seals segments that are due, trims the ring buffer. */
    @Synchronized
    fun tick() {
        if (stopped) return
        val now = clock()
        when (mode) {
            Mode.RECORD -> maybeSeal(now)
            Mode.BUFFER -> trim(now)
            Mode.OFF -> {}
        }
    }

    /**
     * Replay stopped: seals and sends the open segment (on-error buffer is dropped) and ignores all later input.
     * The session's decision and segment counter stay persisted, so a later install continues the numbering.
     */
    @Synchronized
    fun stop() {
        if (stopped) return
        if (mode == Mode.RECORD) seal(sync = false)
        resetBuffer()
        stopped = true
    }

    /** App went to background: send what is recorded (buffered on-error data stays in memory). */
    @Synchronized
    fun flush() {
        if (mode == Mode.RECORD) seal(sync = false)
    }

    // ------------------------------------------------------------------ buffer & segments

    private fun add(ev: Ev) {
        if (events.isEmpty()) {
            segmentContext = lastContext
            val s = lastScreen
            // Each segment starts with the screen so it can be laid out on its own.
            if (ev.type != SCREEN && s != null && mode == Mode.RECORD) {
                val json = WireEvents.screen(ev.ts, s.widthDp, s.heightDp, s.density, s.name)
                events.add(Ev(ev.ts, json, SCREEN)); bytes += json.length
            }
        }
        events.add(ev)
        bytes += ev.json.length
        if (mode == Mode.BUFFER) trim(ev.ts)
    }

    private fun maybeSeal(now: Long) {
        if (mode != Mode.RECORD || events.isEmpty()) return
        if (now - events[0].ts >= SEGMENT_MAX_MS || bytes >= SEGMENT_MAX_BYTES) seal(sync = false)
    }

    /**
     * Ring buffer: keeps the last [BUFFER_MS], plus the latest frame (and screen) older than that so the
     * buffer always starts with a full wireframe. Hard cap [BUFFER_MAX_BYTES].
     */
    private fun trim(now: Long) {
        val cutoff = now - BUFFER_MS
        var lastOldFrame = -1
        for (i in events.indices) {
            val e = events[i]
            if (e.ts > cutoff) break
            if (e.type == FRAME) lastOldFrame = i
        }
        if (lastOldFrame > 0) {
            val head = events.subList(0, lastOldFrame)
            val screen = head.lastOrNull { it.type == SCREEN }
            for (e in head) bytes -= e.json.length
            head.clear()
            if (screen != null) { events.add(0, screen); bytes += screen.json.length }
        }
        while (bytes > BUFFER_MAX_BYTES && events.size > 1) bytes -= events.removeAt(0).json.length
    }

    /** Flushes the whole buffer as segments of ≤ 256 KB. */
    private fun sealChunks(sync: Boolean) {
        while (events.isNotEmpty()) {
            if (bytes <= SEGMENT_MAX_BYTES) { seal(sync); return }
            var n = 0; var size = 0
            while (n < events.size && (n == 0 || size + events[n].json.length <= SEGMENT_MAX_BYTES)) { size += events[n].json.length; n++ }
            val chunk = ArrayList(events.subList(0, n))
            events.subList(0, n).clear()
            bytes -= size
            write(chunk, sync, clicks, errors)
            clicks = 0; errors = 0
            segmentContext = lastContext
        }
    }

    private fun seal(sync: Boolean) {
        if (events.isEmpty()) return
        val chunk = ArrayList(events)
        val c = clicks; val e = errors
        resetBuffer()
        write(chunk, sync, c, e)
    }

    private fun resetBuffer() {
        events.clear(); bytes = 0; clicks = 0; errors = 0
    }

    private fun write(chunk: List<Ev>, sync: Boolean, clicks: Int, errors: Int) = Guard.run("replay.seal", Unit) {
        val sid = sessionId ?: return@run
        val res = resource() ?: return@run
        val json = segmentJson(sid, segment, res, segmentContext ?: lastContext, chunk, clicks, errors)
        sink.write(gzip(json.toByteArray(Charsets.UTF_8)), sync)
        segment++
        persistState(sync)
    }

    companion object {
        const val SEGMENT_MAX_MS = 5_000L
        const val SEGMENT_MAX_BYTES = 256 * 1024
        const val BUFFER_MS = 60_000L
        const val BUFFER_MAX_BYTES = 4 * 1024 * 1024
        const val KEY_SESSION = "session_id"
        const val KEY_SEGMENT = "next_segment"
        const val KEY_MODE = "mode"
        private const val SCREEN = 1
        private const val FRAME = 2
        private const val TOUCH = 3
        private const val KEYBOARD = 4

        private fun segmentJson(
            sessionId: String, segment: Int, res: ResourceInfo, ctx: TrooperSpi.Context?, events: List<Ev>,
            clicks: Int, errors: Int,
        ): String {
            val w = Json(StringBuilder(512 + events.sumOf { it.json.length + 1 }))
            w.beginObject()
            w.name("resource").beginObject()
                .name("service").value(res.service).name("env").value(res.env).name("version").value(res.version)
                .name("platform").value("android").name("sdk_name").value(SDK_NAME).name("sdk_version").value(res.sdkVersion)
                .endObject()
            w.name("session_id").value(sessionId)
            w.name("segment").value(segment.toLong())
            w.name("start_ts").value(events.first().ts)
            w.name("end_ts").value(events.maxOf { it.ts })
            w.name("format").value("wireframe")
            w.name("has_full_snapshot").value(events.any { it.type == FRAME })
            if (ctx != null) {
                ctx.viewId?.let { w.name("view_id").value(it) }
                ctx.viewName?.let { w.name("view_name").value(it) }
                ctx.userId?.let { w.name("user_id").value(it) }
                if (ctx.anonymousId.isNotEmpty()) w.name("anonymous_id").value(ctx.anonymousId)
            }
            // Rage clicks / inputs are not detected on mobile wireframes (optional fields, omitted).
            w.name("stats").beginObject().name("clicks").value(clicks.toLong()).name("errors").value(errors.toLong()).endObject()
            w.name("events").beginArray()
            for (e in events) w.raw(e.json)
            w.endArray().endObject()
            return w.toString()
        }

        const val SDK_NAME = "trooper-android-replay"

        fun gzip(data: ByteArray): ByteArray {
            val bos = ByteArrayOutputStream(data.size / 4 + 64)
            GZIPOutputStream(bos).use { it.write(data) }
            return bos.toByteArray()
        }
    }
}

/** Never let a replay failure reach the host app (AGENTS.md rule 5). */
internal object Guard {
    inline fun <T> run(where: String, fallback: T, block: () -> T): T =
        try { block() } catch (t: Throwable) { report(where, t); fallback }

    fun report(where: String, t: Throwable) {
        try { TrooperSpi.internalError(where, t) } catch (_: Throwable) {}
    }
}
