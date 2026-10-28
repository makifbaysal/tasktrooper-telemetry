package com.tasktrooper.android.internal

import com.tasktrooper.android.TaskTrooper
import com.tasktrooper.android.spi.TrooperSpi
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ReplaySpiTest {
    @After fun reset() { TaskTrooper.installForTest(null) }

    @Test fun errorListenersSeeErrorsAnrsAndCrashesOfSampledSessions() {
        val h = Harness()
        val seen = ArrayList<Pair<Long, Boolean>>()
        h.core.errorListeners.add(TrooperSpi.ErrorListener { ts, crash -> seen.add(ts to crash) })
        h.core.error(RuntimeException("x"), "source", emptyMap())
        h.core.anr(emptyArray())
        h.core.crash(Thread.currentThread(), RuntimeException("boom"))
        assertEquals(listOf(h.clock.now to false, h.clock.now to false, h.clock.now to true), seen)
    }

    @Test fun unsampledSessionsDoNotNotify() {
        val h = Harness(sampleRate = 0.0)
        var calls = 0
        h.core.errorListeners.add(TrooperSpi.ErrorListener { _, _ -> calls++ })
        h.core.error(RuntimeException("x"), "source", emptyMap())
        assertEquals(0, calls)
    }

    @Test fun spiContextFollowsRumSession() {
        assertNull(TrooperSpi.context())
        assertFalse(TrooperSpi.isInitialized)
        val h = Harness()
        TaskTrooper.installForTest(h.core)
        h.core.startView("Checkout")
        val ctx = TrooperSpi.context()
        assertNotNull(ctx)
        assertEquals(h.sessions.peek()!!.id, ctx!!.sessionId)
        assertTrue(ctx.sampled)
        assertEquals("Checkout", ctx.viewName)
        assertEquals(h.sessions.anonymousId, ctx.anonymousId)
        // Replay does not keep a session alive: after 30 min idle it rotates (SESSION_START is emitted).
        h.clock.advance(31 * 60_000L)
        assertTrue(TrooperSpi.context()!!.sessionId != ctx.sessionId)
        assertEquals(2, h.sentEvents().count { it["type"].asString == "SESSION_START" })
    }
}
