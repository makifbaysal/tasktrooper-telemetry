package com.tasktrooper.android.replay.internal

import android.app.Activity
import android.app.Application
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
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
    private val wrappedWindows = WeakHashMap<Window, Boolean>()
    private var dirty = true
    private var scheduled = false
    private var lastCaptureUptime = 0L
    private var lastSessionCheck = 0L
    private var lastMoveUptime = 0L
    private var startedActivities = 0

    /** Current throttle; grows when captures are slow so replay stays within a few ms of main thread per second. */
    var minIntervalMs = MIN_INTERVAL_MS
        private set
    var lastCostNs = 0L
        private set

    private val drawListener = ViewTreeObserver.OnDrawListener { markDirty() }
    private val layoutListener = ViewTreeObserver.OnGlobalLayoutListener { markDirty() }
    private val captureRunnable = Runnable { capture() }

    fun start() {
        app.registerActivityLifecycleCallbacks(this)
        main.post { markDirty() }
    }

    fun markDirty() {
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
        val roots = RootViews.visible().ifEmpty { listOfNotNull(decor) }
        if (roots.isEmpty()) return@run
        for (r in roots) observe(r)
        val density = app.resources.displayMetrics.density
        val ts = System.currentTimeMillis()
        val result = mapper.map(roots, density)
        lastCaptureUptime = SystemClock.uptimeMillis()
        lastCostNs = result.costNs
        // Budget: ≈ 1% of main-thread time (cost × 100), never faster than 2 fps, never slower than 0.2 fps.
        val costMs = result.costNs / 1_000_000L
        minIntervalMs = (costMs * 100).coerceIn(MIN_INTERVAL_MS, MAX_INTERVAL_MS).let { if (result.truncated) maxOf(it, 2_000L) else it }
        if (TrooperSpi.isDebug && result.truncated) TrooperSpi.debug("replay: frame truncated (${result.nodes.size} nodes, ${costMs} ms)")
        val base = decor?.takeIf { it.width > 0 } ?: roots.first()
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

    private fun wrapWindow(activity: Activity) {
        val window = activity.window ?: return
        if (wrappedWindows[window] == true) return
        val cb = window.callback ?: return
        if (cb is TouchCallback) return
        window.callback = TouchCallback(cb) { ev -> Guard.run("replay.touch", Unit) { onTouch(ev) } }
        wrappedWindows[window] = true
    }

    // ------------------------------------------------------------------ lifecycle

    override fun onActivityResumed(activity: Activity) = Guard.run("replay.resumed", Unit) {
        current = WeakReference(activity)
        wrapWindow(activity)
        activity.window?.peekDecorView()?.let { observe(it) }
        markDirty()
    }

    override fun onActivityStarted(activity: Activity) { startedActivities++ }

    override fun onActivityStopped(activity: Activity) {
        startedActivities = (startedActivities - 1).coerceAtLeast(0)
        if (startedActivities == 0) Guard.run("replay.background", Unit) { onBackground() }
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
internal class TouchCallback(private val delegate: Window.Callback, private val onTouch: (MotionEvent) -> Unit) :
    Window.Callback by delegate {
    override fun dispatchTouchEvent(event: MotionEvent?): Boolean {
        if (event != null) try { onTouch(event) } catch (_: Throwable) {}
        return delegate.dispatchTouchEvent(event)
    }
}
