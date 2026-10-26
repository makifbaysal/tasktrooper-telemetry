package com.tasktrooper.android.internal

import com.tasktrooper.android.EventType
import com.tasktrooper.android.RumEvent
import com.tasktrooper.android.RumPayload
import com.tasktrooper.android.RumUser
import com.tasktrooper.android.TrooperConfig
import java.io.PrintWriter
import java.io.StringWriter
import java.util.UUID

/**
 * Event assembly: enriches every event with session/view/user/connection, applies sampling and
 * `beforeSend`, and hands JSON to the [Pipeline]. Contains no Android framework calls so it is
 * unit-testable on the JVM.
 */
internal class RumCore(
    val config: TrooperConfig,
    val clock: Clock,
    val sessions: SessionManager,
    val pipeline: Pipeline,
    /** Enables dead-tap detection (ACTION is emitted 1 s after the tap); null = emit immediately. */
    private val scheduler: Scheduler? = null,
    private val connection: () -> String? = { null },
) {
    private class ViewState(
        val id: String,
        val name: String,
        val key: Any?,
        val startUptime: Long,
        val startWallMs: Long,
        val isInitial: Boolean,
        /** false while VIEW_START is deferred until the first frame (contracts.md §9: loading_time_ms on VIEW_START). */
        var startEmitted: Boolean,
    )

    @Volatile var user: RumUser? = null
        set(value) { field = value; publishContext() }
    private var view: ViewState? = null
    private var viewsInSession = 0
    private var viewSessionId: String? = null
    private val lock = Any()
    val frames = FrameStats()
    private val rage = RageTapDetector()

    /** Receives the session/view/user whenever it changes (native crash handler, per-process records). */
    @Volatile var contextListener: ((CrashContext) -> Unit)? = null
    @Volatile private var lastContext: CrashContext? = null

    /** Dead taps: last UI/network/navigation activity (uptime ms). */
    @Volatile private var lastActivityUptime = Long.MIN_VALUE
    private val pendingActions = ArrayList<PendingAction>()
    private val fullyDrawnViews = HashSet<String>()

    private class PendingAction(val payload: RumPayload.Action, val view: ViewState?, val tsMs: Long, val tapUptime: Long) {
        var done = false
    }

    val currentViewName: String? get() = synchronized(lock) { view?.name }
    val currentViewKey: Any? get() = synchronized(lock) { view?.key }
    val currentViewId: String? get() = synchronized(lock) { view?.id }

    // ---------------------------------------------------------------- views

    /**
     * Starts a view (ending the previous one). With [deferStart], VIEW_START is emitted by
     * [onViewFirstFrame] so it can carry `loading_time_ms`; if the view ends first it is emitted then.
     */
    fun startView(name: String, key: Any? = null, deferStart: Boolean = false) = Internal.guard("startView", Unit) {
        synchronized(lock) { view?.let { view = null; endViewLocked(it) } }
        frames.reset()
        // Determine is_initial against the session the VIEW_START will belong to.
        val (session, _) = touchSession(true)
        val v: ViewState
        synchronized(lock) {
            if (viewSessionId != session.id) { viewSessionId = session.id; viewsInSession = 0 }
            v = ViewState(UUID.randomUUID().toString(), name, key, clock.uptimeMs(), clock.nowMs(),
                isInitial = viewsInSession == 0, startEmitted = !deferStart)
            viewsInSession++
            view = v
        }
        noteActivity()
        publishContext()
        if (!deferStart) emitViewStart(v, 0.0)
    }

    /** First frame of a deferred view: emits VIEW_START with [loadingTimeMs]. */
    fun onViewFirstFrame(key: Any?, loadingTimeMs: Double) = Internal.guard("firstFrame", Unit) {
        val v = synchronized(lock) {
            val v = view
            if (v == null || v.startEmitted || (key != null && v.key != key)) return@guard
            v.startEmitted = true
            v
        }
        emitViewStart(v, loadingTimeMs)
    }

    private fun emitViewStart(v: ViewState, loadingTimeMs: Double) {
        emit(EventType.VIEW_START, RumPayload.View(loadingTimeMs = loadingTimeMs, isInitial = v.isInitial),
            emptyMap(), true, v, v.startWallMs)
    }

    /** Stops the current view if [key] matches (null matches any). */
    fun stopView(key: Any? = null) = Internal.guard("stopView", Unit) {
        synchronized(lock) {
            val v = view ?: return@guard
            if (key != null && v.key != key && v.name != key) return@guard
            view = null
            endViewLocked(v)
        }
        publishContext()
    }

    /** Hybrid bridge: adopt a view owned by the Flutter/RN layer (no events emitted here). */
    fun adoptView(id: String, name: String) {
        synchronized(lock) {
            view = ViewState(id, name, HYBRID_KEY, clock.uptimeMs(), clock.nowMs(), isInitial = false, startEmitted = true)
        }
        publishContext()
    }

    private fun endViewLocked(v: ViewState) {
        if (v.key === HYBRID_KEY) return
        if (!v.startEmitted) { v.startEmitted = true; emitViewStart(v, 0.0) }
        val spent = (clock.uptimeMs() - v.startUptime).toDouble()
        if (frames.total > 0) {
            val attrs = mapOf("total_frames" to frames.total.toString())
            emit(EventType.VITAL, RumPayload.Vital("SLOW_FRAMES", frames.slow.toDouble(),
                rateFrames(frames.slow, frames.total, 0.1, 0.25)), attrs, true, v, null)
            emit(EventType.VITAL, RumPayload.Vital("FROZEN_FRAMES", frames.frozen.toDouble(),
                if (frames.frozen == 0) "good" else if (frames.frozen < 3) "needs-improvement" else "poor"), attrs, true, v, null)
            frames.reset()
        }
        emit(EventType.VIEW_END, RumPayload.View(timeSpentMs = spent), emptyMap(), true, v, null)
    }

    private fun rateFrames(n: Int, total: Int, good: Double, poor: Double): String {
        val r = n.toDouble() / total
        return if (r <= good) "good" else if (r <= poor) "needs-improvement" else "poor"
    }

    // ---------------------------------------------------------------- events

    fun track(name: String, props: Map<String, String>, value: Double) =
        emit(EventType.CUSTOM, RumPayload.Custom(name, props, value))

    fun action(name: String, target: String?) {
        val t = target ?: name
        val now = clock.uptimeMs()
        val isRage = rage.onTap("${currentViewName}|$t", now)
        val payload = RumPayload.Action(name, target, rage = isRage)
        val sched = scheduler
        if (sched == null || !config.trackDeadTaps) {
            emit(EventType.ACTION, payload)
            return
        }
        // dead = nothing happened within DEAD_TAP_WINDOW_MS: no navigation, request or layout change.
        val p = PendingAction(payload, synchronized(lock) { view }, clock.nowMs(), now)
        synchronized(pendingActions) { pendingActions.add(p) }
        sched.schedule(DEAD_TAP_WINDOW_MS) { resolveAction(p, final = true) }
    }

    private fun resolveAction(p: PendingAction, final: Boolean) {
        synchronized(pendingActions) {
            if (p.done) return
            p.done = true
            pendingActions.remove(p)
        }
        val alive = lastActivityUptime >= p.tapUptime || !final
        emit(EventType.ACTION, p.payload.copy(dead = !alive), emptyMap(), true, p.view, p.tsMs)
    }

    /** Emits taps still inside their dead-tap window as not dead (crash / shutdown). */
    fun flushPendingActions() {
        val all = synchronized(pendingActions) { ArrayList(pendingActions) }
        for (p in all) resolveAction(p, final = false)
    }

    /** UI / network / navigation activity: taps followed by activity within 1 s are not dead. */
    fun noteActivity() { lastActivityUptime = clock.uptimeMs() }

    fun resource(p: RumPayload.Resource) {
        noteActivity()
        emit(EventType.RESOURCE, p, extend = false)
    }

    /**
     * Time to full display (`VITAL` `TTFD`, ms) for the current view, once per view. [fromUptime] is the
     * screen's start (activity onCreate); defaults to when the view started. [key] must match the view if set.
     */
    fun fullyDrawn(key: Any? = null, fromUptime: Long? = null) = Internal.guard("fullyDrawn", Unit) {
        val v = synchronized(lock) {
            val v = view ?: return@guard
            if (key != null && v.key != key) return@guard
            if (!fullyDrawnViews.add(v.id)) return@guard
            if (fullyDrawnViews.size > 64) { fullyDrawnViews.clear(); fullyDrawnViews.add(v.id) }
            v
        }
        val ms = (clock.uptimeMs() - (fromUptime ?: v.startUptime)).coerceAtLeast(0).toDouble()
        val rating = if (ms <= 2_000) "good" else if (ms <= 4_000) "needs-improvement" else "poor"
        emit(EventType.VITAL, RumPayload.Vital("TTFD", ms, rating), emptyMap(), true, v, null)
    }

    /** [startReference]: "process" when measured from OS process start (contracts.md §9). */
    fun appStart(kind: String, durationMs: Double, startReference: String? = null) =
        emit(EventType.APP_START, RumPayload.AppStart(kind, durationMs),
            if (startReference != null) mapOf("start_reference" to startReference) else emptyMap())

    fun error(t: Throwable, source: String, attributes: Map<String, String>, thread: String? = Thread.currentThread().name) =
        emit(EventType.ERROR, errorPayload(t, source, false, thread), attributes)

    fun anr(mainStack: Array<StackTraceElement>) {
        val stack = mainStack.joinToString("\n") { "\tat $it" }
        emit(EventType.ERROR, RumPayload.Error(
            type = "ANR",
            message = "Application Not Responding: main thread blocked for ≥ ${config.anrThresholdMs} ms",
            stack = "ANR\n$stack", source = "anr", isCrash = false, thread = "main"))
    }

    /**
     * Uncaught exception: build the crash event and write it (with everything buffered) to disk
     * synchronously on the crashing thread. Uploaded on next launch.
     */
    fun crash(thread: Thread, t: Throwable) = Internal.guard("crash", Unit) {
        flushPendingActions()
        val json = build(EventType.ERROR, errorPayload(t, "crash", true, thread.name), emptyMap(), true, null) ?: run {
            pipeline.persist(); return@guard
        }
        pipeline.persist(json)
    }

    private fun errorPayload(t: Throwable, source: String, isCrash: Boolean, thread: String?): RumPayload.Error {
        val sw = StringWriter()
        t.printStackTrace(PrintWriter(sw))
        var stack = sw.toString()
        if (stack.length > MAX_STACK) stack = stack.substring(0, MAX_STACK)
        return RumPayload.Error(type = t.javaClass.name, message = t.message?.take(MAX_MESSAGE), stack = stack,
            source = source, isCrash = isCrash, thread = thread)
    }

    fun emit(type: EventType, payload: RumPayload?, attributes: Map<String, String> = emptyMap(), extend: Boolean = true) {
        emit(type, payload, attributes, extend, null)
    }

    private fun emit(type: EventType, payload: RumPayload?, attributes: Map<String, String>, extend: Boolean, viewOverride: ViewState?, tsMs: Long? = null) =
        Internal.guard("emit", Unit) {
            val json = build(type, payload, attributes, extend, viewOverride, tsMs) ?: return@guard
            pipeline.add(json)
        }

    /**
     * An error that ended a previous process (native crash report, ApplicationExitInfo), attributed to the
     * session/view in [ctx] (null: current session, no view). Dropped when that session was not sampled.
     */
    fun previousProcessError(ctx: CrashContext?, tsMs: Long, payload: RumPayload.Error, attributes: Map<String, String>) =
        Internal.guard("previousProcessError", Unit) {
            if (ctx == null) {
                emit(EventType.ERROR, payload, attributes, false, NO_VIEW, tsMs)
                return@guard
            }
            if (!ctx.sampled) return@guard
            var event = RumEvent(EventType.ERROR, tsMs, ctx.sessionId, ctx.viewId, ctx.viewName,
                ctx.anonymousId.ifEmpty { sessions.anonymousId }, ctx.userId?.let { RumUser(it) }, null, payload, attributes)
            val hook = config.beforeSend
            if (hook != null) {
                event = try { hook(event) } catch (t: Throwable) { Internal.error("beforeSend", t); event } ?: return@guard
            }
            pipeline.add(event.toJson())
        }

    /** Current context for crash attribution (does not start or extend a session). */
    fun crashContext(): CrashContext? {
        val s = sessions.peek() ?: return null
        val v = synchronized(lock) { view }
        return CrashContext(s.id, sessions.anonymousId, v?.id, v?.name, user?.id, s.sampled)
    }

    private fun publishContext() {
        val l = contextListener ?: return
        Internal.guard("context", Unit) {
            val c = crashContext() ?: return@guard
            if (c == lastContext) return@guard
            lastContext = c
            l(c)
        }
    }

    private fun build(type: EventType, payload: RumPayload?, attributes: Map<String, String>, extend: Boolean, viewOverride: ViewState?, tsMs: Long? = null): String? {
        val (session, isNew) = touchSession(extend)
        if (isNew) publishContext()
        if (!session.sampled) return null
        val v = if (viewOverride === NO_VIEW) null else viewOverride ?: synchronized(lock) { view }
        var event = RumEvent(
            type = type,
            tsMs = tsMs ?: clock.nowMs(),
            sessionId = session.id,
            viewId = v?.id,
            viewName = v?.name,
            anonymousId = sessions.anonymousId,
            user = user,
            connection = Internal.guard("connection", null) { connection() },
            payload = payload,
            attributes = attributes,
        )
        val hook = config.beforeSend
        if (hook != null) {
            event = try { hook(event) } catch (t: Throwable) { Internal.error("beforeSend", t); event } ?: return null
        }
        return event.toJson()
    }

    private fun touchSession(extend: Boolean): Pair<SessionManager.Session, Boolean> {
        val r = sessions.touch(extend)
        if (r.second && r.first.sampled) {
            val ev = RumEvent(EventType.SESSION_START, clock.nowMs(), r.first.id, null, null,
                sessions.anonymousId, user, Internal.guard("connection", null) { connection() }, null)
            Internal.guard("session_start", Unit) { pipeline.add(ev.toJson()) }
        }
        return r
    }

    companion object {
        const val MAX_STACK = 64 * 1024
        const val MAX_MESSAGE = 4 * 1024
        const val DEAD_TAP_WINDOW_MS = 1_000L
        private val HYBRID_KEY = Any()
        private val NO_VIEW = ViewState("", "", null, 0, 0, isInitial = false, startEmitted = true)
    }
}
