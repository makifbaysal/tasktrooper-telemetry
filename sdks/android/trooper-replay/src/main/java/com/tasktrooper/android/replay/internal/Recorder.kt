package com.tasktrooper.android.replay.internal

import android.app.Activity
import android.app.Application
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.os.Trace
import android.view.MotionEvent
import android.view.View
import android.view.ViewTreeObserver
import android.view.Window
import android.view.WindowInsets
import com.tasktrooper.android.spi.TrooperSpi
import java.lang.ref.WeakReference
import java.lang.reflect.Field
import java.util.WeakHashMap

/**
 * Main-thread side of mobile replay: watches window roots for draws/layouts (dirty flag), captures at most
 * every [minIntervalMs] (≥ 500 ms = 2 fps, raised when a capture is expensive), forwards touches and keyboard
 * visibility. Everything else (diffing, buffering, gzip, disk, network) runs on [worker].
 */
internal class Recorder(
    private val app: Application,
    private val engine: ReplayEngine,
    private val worker: ReplayScheduler,
    private val mapper: ViewMapper,
    private val onBackground: () -> Unit,
) : Application.ActivityLifecycleCallbacks {
    private val main = Handler(Looper.getMainLooper())
    private var current: WeakReference<Activity>? = null
    /** Root → the ViewTreeObserver our listeners are on (a re-attached window gets a new one). */
    private val observed = WeakHashMap<View, ViewTreeObserver>()
    private val wrappedWindows = WeakHashMap<Window, TouchCallback>()
    private val startedActivities = WeakHashMap<Activity, Boolean>()
    private var running = false
    private var dirty = true
    private var scheduled = false
    private var lastCaptureUptime = 0L
    private var lastSessionCheck = 0L
    private var lastMoveUptime = 0L
    /** The previous capture ran out of time and was dropped (retried once before a partial frame is kept). */
    private var droppedTimedOut = false

    /** Current throttle; grows when captures are slow so replay stays within a few ms of main thread per second. */
    var minIntervalMs = MIN_INTERVAL_MS
        private set
    var lastCostNs = 0L
        private set

    private val drawListener = ViewTreeObserver.OnDrawListener { markDirty() }
    private val layoutListener = ViewTreeObserver.OnGlobalLayoutListener { markDirty() }
    private val captureRunnable = Runnable { capture() }

    /**
     * Main thread. [resumed]: an activity that is already resumed (late install, e.g. React Native after
     * `MainActivity.onResume`): seeded as started + current and its window is wrapped for touches right away.
     */
    fun start(resumed: Activity? = null) {
        if (running) return
        running = true
        app.registerActivityLifecycleCallbacks(this)
        if (resumed != null && !resumed.isFinishing) {
            startedActivities[resumed] = true
            onActivityResumed(resumed)
        }
        main.post { markDirty() }
    }

    /** Main thread. Detaches every hook (lifecycle, tree observers, window callbacks); idempotent. */
    fun stop() {
        if (!running) return
        running = false
        app.unregisterActivityLifecycleCallbacks(this)
        main.removeCallbacks(captureRunnable)
        scheduled = false
        for ((root, vto) in observed.entries.toList()) Guard.run("replay.unobserve", Unit) {
            val live = if (vto.isAlive) vto else root.viewTreeObserver
            live.removeOnDrawListener(drawListener)
            live.removeOnGlobalLayoutListener(layoutListener)
        }
        observed.clear()
        for ((window, cb) in wrappedWindows.entries.toList()) Guard.run("replay.unwrap", Unit) {
            cb.enabled = false // stays a pass-through when someone wrapped the callback after us
            if (window.callback === cb) window.callback = cb.delegate
        }
        wrappedWindows.clear()
        startedActivities.clear()
        current = null
    }

    fun markDirty() {
        if (!running) return
        dirty = true
        if (scheduled) return
        scheduled = true
        val delay = (lastCaptureUptime + minIntervalMs - SystemClock.uptimeMillis()).coerceAtLeast(0)
        main.postDelayed(captureRunnable, delay)
    }

    private fun capture() = Guard.run("replay.capture", Unit) {
        scheduled = false
        if (!dirty) return@run
        if (!engine.isCapturing) {
            // Not recording this session. Re-check now and then: a new session gets a new sampling decision.
            val now = SystemClock.uptimeMillis()
            if (now - lastSessionCheck >= SESSION_CHECK_MS) {
                lastSessionCheck = now
                worker.execute { if (engine.refreshSession() != ReplayEngine.Mode.OFF) main.post { markDirty() } }
            }
            return@run
        }
        dirty = false
        val activity = current?.get()
        val decor = activity?.window?.peekDecorView()
        val all = RootViews.visible().ifEmpty { listOfNotNull(decor) }
        for (r in all) {
            observe(r)
            // Dialogs, popups and an activity resumed before we started: their Window.Callback sees the touches.
            if (r !== decor) RootViews.windowOf(r)?.let { wrapWindow(it) }
        }
        // Not laid out yet (first frame, rotation): skip; the coming layout marks the tree dirty again.
        val roots = all.filter { it.width > 0 && it.height > 0 }
        if (roots.isEmpty()) return@run
        val density = app.resources.displayMetrics.density
        val ts = System.currentTimeMillis()
        // Visible in Perfetto / systrace as "trooper.replay.capture".
        Trace.beginSection("trooper.replay.capture")
        val result = try { mapper.map(roots, density) } finally { Trace.endSection() }
        lastCaptureUptime = SystemClock.uptimeMillis()
        lastCostNs = result.costNs
        // costNs is main-thread CPU time (CpuClock). Budget: ≈ 1% of main-thread time (cost × 100), never faster than 2 fps, never slower than 0.2 fps.
        val costMs = result.costNs / 1_000_000L
        minIntervalMs = (costMs * 100).coerceIn(MIN_INTERVAL_MS, MAX_INTERVAL_MS).let { if (result.truncated) maxOf(it, 2_000L) else it }
        if (result.timedOut && !droppedTimedOut) {
            // A partial frame would show half the screen until the next capture (time budget hit by JIT warm-up,
            // GC, a new window's first traversal). Drop it and retry once after the backoff; keep the next one.
            droppedTimedOut = true
            if (TrooperSpi.isDebug) TrooperSpi.debug("replay: capture over budget (${costMs} ms), retrying in $minIntervalMs ms")
            markDirty()
            return@run
        }
        droppedTimedOut = false
        if (TrooperSpi.isDebug) {
            TrooperSpi.debug("replay: capture ${result.nodes.size} nodes, ${result.costNs / 1_000} µs, ${roots.size} window(s), next ≥ $minIntervalMs ms" +
                if (result.truncated) " (truncated)" else "")
        }
        val base = decor?.takeIf { it.width > 0 && it.height > 0 } ?: roots.first()
        val widthDp = Math.round(base.width / density)
        val heightDp = Math.round(base.height / density)
        val keyboard = keyboardVisible(base)
        val nodes = result.nodes
        worker.execute { engine.onFrame(ts, widthDp, heightDp, density, nodes, keyboard) }
    }

    private fun observe(root: View) {
        val vto = root.viewTreeObserver
        if (observed[root] === vto || !vto.isAlive) return
        Guard.run("replay.observe", Unit) {
            vto.addOnDrawListener(drawListener)
            vto.addOnGlobalLayoutListener(layoutListener)
            observed[root] = vto
        }
    }

    // ------------------------------------------------------------------ touches & keyboard

    fun onTouch(ev: MotionEvent) {
        if (!engine.isCapturing) return
        val phase = when (ev.actionMasked) {
            MotionEvent.ACTION_DOWN -> "down"
            MotionEvent.ACTION_MOVE -> {
                if (ev.eventTime - lastMoveUptime < MOVE_INTERVAL_MS) return
                lastMoveUptime = ev.eventTime
                "move"
            }
            MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> "up"
            else -> return
        }
        val d = app.resources.displayMetrics.density.takeIf { it > 0f } ?: 1f
        val x = ev.rawX / d; val y = ev.rawY / d
        val ts = System.currentTimeMillis()
        worker.execute { engine.onTouch(ts, x, y, phase) }
    }

    @Suppress("DEPRECATION")
    private fun keyboardVisible(root: View): Boolean? = Guard.run("replay.ime", null) {
        when {
            Build.VERSION.SDK_INT >= 30 -> root.rootWindowInsets?.isVisible(WindowInsets.Type.ime())
            Build.VERSION.SDK_INT >= 23 -> root.rootWindowInsets?.let { it.systemWindowInsetBottom > root.height * 0.15f }
            else -> null
        }
    }

    private fun wrapWindow(window: Window) {
        if (wrappedWindows[window]?.let { it === window.callback } == true) return
        val cb = window.callback ?: return
        if (cb is TouchCallback) return
        val wrapper = TouchCallback(cb) { ev -> Guard.run("replay.touch", Unit) { onTouch(ev) } }
        window.callback = wrapper
        wrappedWindows[window] = wrapper
    }

    // ------------------------------------------------------------------ lifecycle

    override fun onActivityResumed(activity: Activity) = Guard.run("replay.resumed", Unit) {
        current = WeakReference(activity)
        activity.window?.let { wrapWindow(it) }
        activity.window?.peekDecorView()?.let { observe(it) }
        markDirty()
    }

    override fun onActivityStarted(activity: Activity) { startedActivities[activity] = true }

    override fun onActivityStopped(activity: Activity) {
        startedActivities.remove(activity)
        if (startedActivities.isEmpty()) Guard.run("replay.background", Unit) { onBackground() }
    }

    override fun onActivityPaused(activity: Activity) {}
    override fun onActivityCreated(activity: Activity, savedInstanceState: Bundle?) {}
    override fun onActivitySaveInstanceState(activity: Activity, outState: Bundle) {}
    override fun onActivityDestroyed(activity: Activity) {
        if (current?.get() === activity) current = null
    }

    companion object {
        const val MIN_INTERVAL_MS = 500L
        const val MAX_INTERVAL_MS = 5_000L
        const val MOVE_INTERVAL_MS = 100L
        const val SESSION_CHECK_MS = 5_000L
    }
}

/** All window roots of the process (activities, dialogs, popups) via `WindowManagerGlobal.mViews`; empty if unavailable. */
internal object RootViews {
    private var windowField: Field? = null
    @Volatile private var windowFailed = false

    /** The [Window] of a `DecorView` root (`DecorView.mWindow`, API 24+); null for popups (no Window) or if blocked. */
    fun windowOf(root: View): Window? {
        if (windowFailed || Build.VERSION.SDK_INT < 24) return null
        return try {
            val f = windowField ?: run {
                if (root.javaClass.name != DECOR_VIEW) return null
                root.javaClass.getDeclaredField("mWindow").apply { isAccessible = true }.also { windowField = it }
            }
            if (!f.declaringClass.isInstance(root)) null else f.get(root) as? Window
        } catch (t: Throwable) {
            windowFailed = true
            null
        }
    }

    private const val DECOR_VIEW = "com.android.internal.policy.DecorView"

    private var global: Any? = null
    private var field: Field? = null
    @Volatile private var failed = false

    fun visible(): List<View> {
        if (failed) return emptyList()
        return try {
            if (field == null) {
                val c = Class.forName("android.view.WindowManagerGlobal")
                global = c.getMethod("getInstance").invoke(null)
                field = c.getDeclaredField("mViews").apply { isAccessible = true }
            }
            val raw = field!!.get(global) as? List<*> ?: return emptyList()
            ArrayList(raw).mapNotNull { it as? View }.filter { it.windowVisibility == View.VISIBLE && it.isShown && it.width > 0 }
        } catch (t: Throwable) {
            failed = true // hidden API blocked: fall back to the resumed activity's window
            emptyList()
        }
    }
}

/** Forwards every Window.Callback call; observes touches first. Never alters dispatch. */
internal class TouchCallback(val delegate: Window.Callback, private val onTouch: (MotionEvent) -> Unit) :
    Window.Callback by delegate {
    @Volatile var enabled = true

    override fun dispatchTouchEvent(event: MotionEvent?): Boolean {
        if (event != null && enabled) try { onTouch(event) } catch (_: Throwable) {}
        return delegate.dispatchTouchEvent(event)
    }
}

/**
 * Main-thread CPU time for the capture budget: wall time also counts preemption (busy device, emulator), which
 * would truncate frames (half the UI missing in the replay) for work we did not do.
 */
internal object CpuClock {
    @Volatile private var unsupported = false

    fun nanos(): Long {
        if (!unsupported) {
            val t = try { android.os.Debug.threadCpuTimeNanos() } catch (_: Throwable) { -1L }
            if (t >= 0) return t
            unsupported = true
        }
        return System.nanoTime()
    }
}
