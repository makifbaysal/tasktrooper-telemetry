package com.tasktrooper.android.replay.internal

import android.app.Activity
import android.app.AlertDialog
import android.app.Application
import android.os.Looper
import android.os.SystemClock
import android.view.MotionEvent
import android.view.Window
import android.widget.TextView
import androidx.test.core.app.ApplicationProvider
import com.google.gson.JsonObject
import com.tasktrooper.android.replay.ReplayOptions
import com.tasktrooper.android.spi.TrooperSpi
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotSame
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import org.robolectric.annotation.LooperMode
import java.util.concurrent.TimeUnit

/** Live-device regressions: touches on the launch activity / dialogs, late install, stop(), unlaid-out roots. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [35])
@LooperMode(LooperMode.Mode.PAUSED)
class RecorderTest {
    private val app: Application = ApplicationProvider.getApplicationContext()

    private class Immediate : ReplayScheduler {
        override fun execute(task: () -> Unit) = task()
        override fun schedule(delayMs: Long, task: () -> Unit) {}
    }

    private class Kv : ReplayStore {
        val m = HashMap<String, String>()
        override fun get(key: String) = m[key]
        override fun put(values: Map<String, String>, sync: Boolean) { m.putAll(values) }
    }

    private val segments = ArrayList<JsonObject>()
    private val engine = ReplayEngine(
        ReplayOptions(sampleRate = 1.0), { System.currentTimeMillis() }, Kv(),
        { gz, _ -> segments.add(ReplayEngineTest.parse(gz)) },
        { TrooperSpi.Context("s1", true, "anon", null, null, "Main") },
        { ReplayEngine.ResourceInfo("app", "dev", "1", "0") },
    )
    private val recorder = Recorder(app, engine, Immediate(), ViewMapper(true)) {}

    private fun idle() = shadowOf(Looper.getMainLooper()).idleFor(2, TimeUnit.SECONDS)

    private fun tap(window: Window) {
        val t = SystemClock.uptimeMillis()
        for (action in intArrayOf(MotionEvent.ACTION_DOWN, MotionEvent.ACTION_UP)) {
            val ev = MotionEvent.obtain(t, t, action, 100f, 200f, 0)
            window.callback.dispatchTouchEvent(ev)
            ev.recycle()
        }
    }

    private fun events(): List<JsonObject> {
        engine.flush()
        return segments.flatMap { s -> s.getAsJsonArray("events").map { it.asJsonObject } }
    }

    private fun resumedActivity(): Activity =
        Robolectric.buildActivity(Activity::class.java).setup().get().apply { setContentView(TextView(this).apply { text = "hi" }) }

    @Test fun lateStartSeedsTheResumedActivityForTouches() {
        engine.refreshSession()
        val activity = resumedActivity() // resumed before replay starts (late install / posted start)
        recorder.start(activity)
        assertTrue(activity.window.callback is TouchCallback)
        tap(activity.window)
        idle()
        assertEquals(listOf("down", "up"), events().filter { it["type"].asString == "touch" }.map { it["phase"].asString })
    }

    @Test fun dialogRootResolvesToItsWindow() {
        // Capture wraps the Window.Callback of every root it walks (dialogs, popups with a DecorView); Robolectric
        // has no shown WindowManagerGlobal roots, so the DecorView → Window resolution is checked directly.
        val activity = resumedActivity()
        val dialog = AlertDialog.Builder(activity).setMessage("Pay?").show()
        idle()
        assertSame(dialog.window, RootViews.windowOf(dialog.window!!.decorView))
        assertSame(null, RootViews.windowOf(TextView(activity))) // not a DecorView (e.g. PopupWindow content)
    }

    @Test fun stopRestoresCallbacksAndIsIdempotent() {
        engine.refreshSession()
        val activity = resumedActivity()
        val original = activity.window.callback
        recorder.start(activity)
        assertNotSame(original, activity.window.callback)
        recorder.stop()
        recorder.stop()
        assertSame(original, activity.window.callback)
        recorder.start(activity) // start after stop works again
        assertTrue(activity.window.callback is TouchCallback)
    }

    @Test fun noFrameBeforeLayout() {
        engine.refreshSession()
        val activity = Robolectric.buildActivity(Activity::class.java).create().start().resume().get() // not laid out
        recorder.start(activity)
        recorder.markDirty(); shadowOf(Looper.getMainLooper()).idle()
        val screens = events().filter { it["type"].asString == "screen" }
        assertFalse(screens.any { it["width"].asInt == 0 || it["height"].asInt == 0 })
    }

    @Test fun overBudgetFrameIsDroppedOnceThenKept() {
        engine.refreshSession()
        var t = 0L
        val slow = ViewMapper(true, budgetNs = 1L, nanoTime = { t += 1_000_000L; t }) // every check is over budget
        val r = Recorder(app, engine, Immediate(), slow) {}
        val activity = Robolectric.buildActivity(Activity::class.java).setup().get()
        activity.setContentView(android.widget.LinearLayout(activity).apply {
            repeat(64) { addView(TextView(activity).apply { text = "row $it" }) }
        })
        r.start(activity)
        shadowOf(Looper.getMainLooper()).idleFor(1, TimeUnit.SECONDS)
        assertEquals(0, events().count { it["type"].asString == "frame" }) // partial frame dropped, retry scheduled
        shadowOf(Looper.getMainLooper()).idleFor(10, TimeUnit.SECONDS)
        assertEquals(1, events().count { it["type"].asString == "frame" }) // second over-budget capture is kept
    }
}
