package com.tasktrooper.android

import android.app.Activity
import android.app.Application
import androidx.test.core.app.ApplicationProvider
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import android.os.Looper

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [35])
class TaskTrooperRobolectricTest {
    private val app: Application = ApplicationProvider.getApplicationContext()

    @After fun reset() { TaskTrooper.installForTest(null) }

    @Test fun apiIsNoOpBeforeInit() {
        TaskTrooper.track("x"); TaskTrooper.startView("A"); TaskTrooper.captureError(RuntimeException()); TaskTrooper.flush()
        assertFalse(TaskTrooper.isInitialized)
    }

    @Test fun invalidDsnKeepsSdkDisabled() {
        TaskTrooper.init(app, TrooperConfig(dsn = "garbage", app = "a"))
        assertFalse(TaskTrooper.isInitialized)
    }

    @Test fun initTracksActivityViews() {
        TaskTrooper.init(app, TrooperConfig(dsn = "http://k@127.0.0.1:9/1", app = "robo", trackAnr = false))
        assertTrue(TaskTrooper.isInitialized)
        assertNotNull(TaskTrooper.anonymousId)
        assertEquals(TaskTrooper.anonymousId, app.getSharedPreferences("tasktrooper", 0).getString("anonymous_id", null))

        val controller = Robolectric.buildActivity(Activity::class.java).setup()
        shadowOf(Looper.getMainLooper()).idle()
        assertEquals("Activity", TaskTrooper.core!!.currentViewName)
        assertNotNull(TaskTrooper.sessionId)
        controller.pause()
        assertNull(TaskTrooper.core!!.currentViewName)
        controller.stop().destroy()
    }

    @Test fun propagationTargets() {
        TaskTrooper.init(app, TrooperConfig(dsn = "http://k@127.0.0.1:9/1", app = "robo", trackAnr = false,
            tracePropagationTargets = listOf(Regex("^https://api\\.shop\\.com"))))
        assertTrue(TaskTrooper.shouldPropagateTrace("https://api.shop.com/v1/orders"))
        assertFalse(TaskTrooper.shouldPropagateTrace("https://cdn.other.com/x"))
        assertTrue(TaskTrooper.isIngestUrl("127.0.0.1"))
    }
}

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [35])
class TrooperHybridBridgeTest {
    private val app: Application = ApplicationProvider.getApplicationContext()

    @After fun reset() { TaskTrooper.installForTest(null) }

    @Test fun noOpBeforeInit() {
        TrooperHybridBridge.setContext(mapOf("session_id" to "x"))
        assertTrue(TrooperHybridBridge.getContext().isEmpty())
    }

    @Test fun contextRoundTrip() {
        TaskTrooper.init(app, TrooperConfig(dsn = "http://k@127.0.0.1:9/1", app = "robo", trackAnr = false))
        val native = TrooperHybridBridge.getContext()
        assertNotNull(native["session_id"]); assertEquals(TaskTrooper.anonymousId, native["anonymous_id"])

        TrooperHybridBridge.setContext(mapOf("session_id" to "s-dart", "anonymous_id" to "a-dart",
            "view_id" to "v1", "view_name" to "/home", "user_id" to "u1", "unknown" to 3))
        assertEquals(mapOf("session_id" to "s-dart", "anonymous_id" to "a-dart", "view_id" to "v1",
            "view_name" to "/home", "user_id" to "u1"), TrooperHybridBridge.getContext())
        assertEquals("dev.tasktrooper.telemetry/rum", TrooperHybridBridge.CHANNEL)
        val start = TrooperHybridBridge.getProcessStartTime()
        assertTrue(start in 1..System.currentTimeMillis())
    }

    @Test fun setContextCarriesTheSamplingDecision() {
        TaskTrooper.init(app, TrooperConfig(dsn = "http://k@127.0.0.1:9/1", app = "robo", trackAnr = false, sampleRate = 1.0))
        TrooperHybridBridge.setContext(mapOf("session_id" to "s-rn", "sampled" to false))
        val core = TaskTrooper.core!!
        assertEquals(false, core.sessions.peek()?.sampled)
        assertEquals(true, core.sessions.peek()?.external)
        TrooperHybridBridge.setContext(mapOf("session_id" to "s-rn", "sampled" to "true")) // string from a loose bridge
        assertEquals(true, core.sessions.peek()?.sampled)
    }
}
