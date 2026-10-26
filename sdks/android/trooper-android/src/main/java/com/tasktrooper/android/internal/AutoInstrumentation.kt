package com.tasktrooper.android.internal

import android.app.Activity
import android.app.Application
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.HandlerThread
import android.os.Looper
import android.os.SystemClock
import android.view.Choreographer
import android.view.FrameMetrics
import android.view.ViewTreeObserver
import android.view.Window
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import com.tasktrooper.android.TrooperConfig
import java.util.WeakHashMap

/** Activity views, app start, frame metrics. Every callback is guarded. */
internal class ActivityTracker(
    private val core: RumCore,
    private val config: TrooperConfig,
    processStartUptime: Long,
) : Application.ActivityLifecycleCallbacks {
    private val main = Handler(Looper.getMainLooper())
    private val createdAt = WeakHashMap<Activity, Long>()
    private val frameListeners = WeakHashMap<Activity, Any>()
    private val fragmentsRegistered = WeakHashMap<Activity, Boolean>()
    private val resumed = WeakHashMap<Activity, Boolean>()
    private val layoutListeners = WeakHashMap<Activity, ViewTreeObserver.OnGlobalLayoutListener>()
    private val ttfdOrigin = WeakHashMap<Activity, Long>()
    private val vsyncFrames: VsyncFrameCallback? = if (Build.VERSION.SDK_INT < 24) VsyncFrameCallback(core.frames) else null
    private val appStart = AppStartTracker(processStartUptime) { kind, ms, cold ->
        // Before API 24 the cold-start origin is SDK init, not the OS process start: no start_reference then.
        if (config.trackAppStart) core.appStart(kind, ms, if (cold && Build.VERSION.SDK_INT >= 24) "process" else null)
    }
    private var frameHandler: Handler? = null

    override fun onActivityCreated(activity: Activity, savedInstanceState: Bundle?) = guard("created") {
        val now = SystemClock.uptimeMillis()
        createdAt[activity] = now
        ttfdOrigin[activity] = now
        appStart.onActivityCreated(now)
        if (ACTIVITY_REPORTER_AVAILABLE) registerFullyDrawn(activity)
        if (config.trackFragments && FRAGMENTS_AVAILABLE && fragmentsRegistered[activity] != true) {
            if (FragmentTracker.register(activity, core)) fragmentsRegistered[activity] = true
        }
    }

    override fun onActivityStarted(activity: Activity) = guard("started") {
        appStart.onActivityStarted(SystemClock.uptimeMillis())
    }

    override fun onActivityResumed(activity: Activity) = guard("resumed") {
        if (config.trackActivities) {
            if (fragmentsRegistered[activity] == true) {
                // Fragments resume in onPostResume (same message). Only fall back to the activity as the
                // view when no fragment screen claimed it, to avoid zero-length activity views.
                resumed[activity] = true
                main.post { guard("deferredView") { if (resumed[activity] == true && core.currentViewKey == null) core.startView(viewName(activity), activity) } }
            } else core.startView(viewName(activity), activity, deferStart = true)
        }
        if (config.trackFrames) addFrameListener(activity)
        if (config.trackDeadTaps) addLayoutListener(activity)
        val created = createdAt.remove(activity) ?: SystemClock.uptimeMillis()
        Choreographer.getInstance().postFrameCallback {
            // Runs right before the first frame is drawn; posting to the front of the queue lands after it.
            main.postAtFrontOfQueue {
                guard("firstFrame") {
                    val now = SystemClock.uptimeMillis()
                    appStart.onFirstFrame(now)
                    // loading_time_ms = onCreate (or onResume when not re-created) → first frame, on VIEW_START.
                    core.onViewFirstFrame(activity, (now - created).toDouble())
                }
            }
        }
    }

    override fun onActivityPaused(activity: Activity) = guard("paused") {
        resumed.remove(activity)
        removeFrameListener(activity)
        removeLayoutListener(activity)
        if (config.trackActivities) core.stopView(activity)
    }

    override fun onActivityStopped(activity: Activity) = guard("stopped") { appStart.onActivityStopped() }
    override fun onActivitySaveInstanceState(activity: Activity, outState: Bundle) {}
    override fun onActivityDestroyed(activity: Activity) = guard("destroyed") {
        createdAt.remove(activity); fragmentsRegistered.remove(activity); ttfdOrigin.remove(activity)
    }

    /** androidx `ComponentActivity.reportFullyDrawn()` / `ReportDrawn*` composables → TTFD (from onCreate). */
    private fun registerFullyDrawn(activity: Activity) {
        val ca = activity as? androidx.activity.ComponentActivity ?: return
        ca.fullyDrawnReporter.addOnReportDrawnListener {
            guard("fullyDrawn") { core.fullyDrawn(activity, ttfdOrigin[activity]) }
        }
    }

    /** Layout passes count as UI updates for dead-tap detection (ripples/animations do not re-layout). */
    private fun addLayoutListener(activity: Activity) {
        if (layoutListeners.containsKey(activity)) return
        val l = ViewTreeObserver.OnGlobalLayoutListener { core.noteActivity() }
        activity.window?.decorView?.viewTreeObserver?.addOnGlobalLayoutListener(l) ?: return
        layoutListeners[activity] = l
    }

    private fun removeLayoutListener(activity: Activity) {
        val l = layoutListeners.remove(activity) ?: return
        try { activity.window?.decorView?.viewTreeObserver?.removeOnGlobalLayoutListener(l) } catch (_: Throwable) {}
    }

    private fun addFrameListener(activity: Activity) {
        if (Build.VERSION.SDK_INT < 24) {
            // No FrameMetrics: measure vsync-to-vsync intervals with a Choreographer callback while resumed.
            vsyncFrames?.start()
            return
        }
        val h = frameHandler ?: Handler(HandlerThread("trooper-frames").apply { start() }.looper).also { frameHandler = it }
        val l = Window.OnFrameMetricsAvailableListener { _, metrics, _ ->
            try {
                if (metrics.getMetric(FrameMetrics.FIRST_DRAW_FRAME) == 0L) {
                    core.frames.record(metrics.getMetric(FrameMetrics.TOTAL_DURATION) / 1_000_000.0)
                }
            } catch (_: Throwable) {}
        }
        activity.window.addOnFrameMetricsAvailableListener(l, h)
        frameListeners[activity] = l
    }

    private fun removeFrameListener(activity: Activity) {
        if (Build.VERSION.SDK_INT < 24) { vsyncFrames?.stop(); return }
        val l = frameListeners.remove(activity) as? Window.OnFrameMetricsAvailableListener ?: return
        try { activity.window.removeOnFrameMetricsAvailableListener(l) } catch (_: Throwable) {}
    }

    private inline fun guard(where: String, block: () -> Unit) = Internal.guard("activity.$where", Unit, block)

    companion object {
        val FRAGMENTS_AVAILABLE: Boolean = try {
            Class.forName("androidx.fragment.app.FragmentActivity"); true
        } catch (_: Throwable) { false }

        val ACTIVITY_REPORTER_AVAILABLE: Boolean = try {
            Class.forName("androidx.activity.FullyDrawnReporter"); true
        } catch (_: Throwable) { false }

        fun viewName(activity: Activity): String = activity.javaClass.simpleName.ifEmpty { activity.javaClass.name }
    }
}

/**
 * API < 24 frame vitals: a Choreographer callback re-posted every frame; the interval between consecutive
 * vsync timestamps is the frame duration. Runs only while an activity is resumed.
 */
internal class VsyncFrameCallback(
    private val frames: FrameStats,
    private val choreographer: () -> Choreographer = { Choreographer.getInstance() },
) : Choreographer.FrameCallback {
    private var running = false
    private val counter = VsyncIntervals(frames)

    fun start() {
        if (running) return
        running = true
        counter.reset()
        choreographer().postFrameCallback(this)
    }

    fun stop() {
        running = false
        try { choreographer().removeFrameCallback(this) } catch (_: Throwable) {}
    }

    override fun doFrame(frameTimeNanos: Long) {
        if (!running) return
        Internal.guard("vsync", Unit) { counter.onFrame(frameTimeNanos) }
        choreographer().postFrameCallback(this)
    }
}

/** Vsync timestamps → frame durations. Intervals ≤ 17.5 ms are on-time frames (contracts.md §9 jitter tolerance). */
internal class VsyncIntervals(private val frames: FrameStats) {
    private var last = 0L

    fun reset() { last = 0L }

    fun onFrame(frameTimeNanos: Long) {
        val prev = last
        last = frameTimeNanos
        if (prev == 0L || frameTimeNanos <= prev) return
        val ms = (frameTimeNanos - prev) / 1_000_000.0
        frames.record(if (ms <= ON_TIME_MS) minOf(ms, 16.0) else ms)
    }

    companion object { const val ON_TIME_MS = 17.5 }
}

/** Flushes the pipeline when the whole app goes to background (ProcessLifecycleOwner ON_STOP). */
internal class ProcessFlushObserver(private val onBackground: () -> Unit) : DefaultLifecycleObserver {
    override fun onStop(owner: LifecycleOwner) { Internal.guard("onStop", Unit) { onBackground() } }

    fun register() {
        Handler(Looper.getMainLooper()).post {
            Internal.guard("processLifecycle", Unit) { ProcessLifecycleOwner.get().lifecycle.addObserver(this) }
        }
    }
}

/** Chains the previous handler after persisting the crash synchronously. */
internal class CrashHandler(
    private val core: RumCore,
    private val previous: Thread.UncaughtExceptionHandler?,
    private val onCrash: () -> Unit = {},
) : Thread.UncaughtExceptionHandler {
    override fun uncaughtException(t: Thread, e: Throwable) {
        try { core.crash(t, e) } catch (_: Throwable) {}
        try { onCrash() } catch (_: Throwable) {}
        previous?.uncaughtException(t, e)
    }
}
