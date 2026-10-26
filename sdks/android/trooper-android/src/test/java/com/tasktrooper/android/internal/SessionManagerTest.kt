package com.tasktrooper.android.internal

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class SessionManagerTest {
    private val clock = FakeClock()
    private val store = MemoryStore()

    @Test fun anonymousIdIsPersisted() {
        val a = SessionManager(clock, store, 1.0).anonymousId
        val b = SessionManager(clock, store, 1.0).anonymousId
        assertEquals(a, b)
        assertNotEquals(a, SessionManager(clock, MemoryStore(), 1.0).anonymousId)
    }

    @Test fun firstTouchStartsSession() {
        val sm = SessionManager(clock, store, 1.0)
        val (s, isNew) = sm.touch()
        assertTrue(isNew)
        assertFalse(sm.touch().second)
        assertEquals(s.id, sm.touch().first.id)
    }

    @Test fun rotatesAfter30MinInactivity() {
        val sm = SessionManager(clock, store, 1.0)
        val first = sm.touch().first
        clock.advance(29 * 60_000L)
        assertEquals(first.id, sm.touch().first.id)
        clock.advance(29 * 60_000L) // activity at 29 min reset the timer
        assertEquals(first.id, sm.touch().first.id)
        clock.advance(30 * 60_000L)
        val (next, isNew) = sm.touch()
        assertTrue(isNew)
        assertNotEquals(first.id, next.id)
    }

    @Test fun rotatesAfter4HoursEvenWhenActive() {
        val sm = SessionManager(clock, store, 1.0)
        val first = sm.touch().first
        repeat(4 * 6) { clock.advance(10 * 60_000L); sm.touch() } // activity every 10 min
        val last = sm.touch().first
        assertNotEquals(first.id, last.id)
    }

    @Test fun passiveEventsDoNotExtendSession() {
        val sm = SessionManager(clock, store, 1.0)
        val first = sm.touch().first
        clock.advance(20 * 60_000L); sm.touch(extend = false)
        clock.advance(11 * 60_000L)
        assertNotEquals(first.id, sm.touch(extend = false).first.id)
    }

    @Test fun samplingIsPerSession() {
        assertFalse(SessionManager(clock, store, 0.0).touch().first.sampled)
        assertTrue(SessionManager(clock, store, 1.0).touch().first.sampled)
        val rnd = java.util.Random(99)
        val sampled = (1..1000).count { SessionManager(clock, MemoryStore(), 0.3, rnd).touch().first.sampled }
        assertTrue("sampled=$sampled", sampled in 200..400)
    }
}
