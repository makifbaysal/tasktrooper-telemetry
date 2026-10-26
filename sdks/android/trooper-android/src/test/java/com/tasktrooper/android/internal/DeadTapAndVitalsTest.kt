package com.tasktrooper.android.internal

import com.tasktrooper.android.TrooperConfig
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class DeadTapAndVitalsTest {
    private class DeadHarness {
        val h = Harness()
        val core = RumCore(TrooperConfig(dsn = "https://k@h/1", app = "t"), h.clock, h.sessions, h.pipeline, h.scheduler) { "wifi" }
        fun actions() = h.sentEvents().filter { it.get("type").asString == "ACTION" }
    }

    @Test fun tapWithoutAnyActivityIsDeadAndKeepsTapTimestamp() {
        val d = DeadHarness()
        d.core.startView("Home")
        d.h.clock.advance(5_000)
        val tapTs = d.h.clock.now
        d.core.action("buy", "btn_buy")
        assertTrue(d.actions().isEmpty()) // held for the 1 s window
        d.h.scheduler.advance(1_000)
        val a = d.actions().single()
        assertTrue(a.getAsJsonObject("action").get("dead")?.asBoolean ?: false)
        assertEquals(tapTs, a.get("ts_ms").asLong)
        assertEquals("Home", a.get("view_name").asString)
    }

    @Test fun networkNavigationOrLayoutWithinOneSecondIsNotDead() {
        val d = DeadHarness()
        d.core.startView("Home")
        d.h.clock.advance(5_000)
        d.core.action("a", null); d.h.clock.advance(300); d.core.noteActivity()          // layout / request start
        d.h.scheduler.advance(1_000)
        d.core.action("b", null); d.h.clock.advance(200); d.core.startView("Detail")      // navigation
        d.h.scheduler.advance(1_000)
        d.core.action("c", null); d.h.clock.advance(100)
        d.core.resource(com.tasktrooper.android.RumPayload.Resource("https://x/y", "GET", 200, 10.0))
        d.h.scheduler.advance(1_000)
        d.core.action("d", null); d.h.scheduler.advance(1_100); d.core.noteActivity()      // too late
        val dead = d.actions().associate { it.getAsJsonObject("action").get("name").asString to (it.getAsJsonObject("action").get("dead")?.asBoolean ?: false) }
        assertEquals(mapOf("a" to false, "b" to false, "c" to false, "d" to true), dead)
    }

    @Test fun crashFlushesPendingTapsAsNotDead() {
        val d = DeadHarness()
        d.core.action("x", null)
        d.core.crash(Thread.currentThread(), RuntimeException("boom"))
        val sent = d.h.sentEvents()
        val a = sent.single { it.get("type").asString == "ACTION" }
        assertFalse(a.getAsJsonObject("action").get("dead")?.asBoolean ?: false) // false is omitted (proto default)
    }

    @Test fun ttfdIsReportedOncePerView() {
        val h = Harness()
        h.core.startView("Home", key = "act")
        h.clock.advance(1_500)
        h.core.fullyDrawn("act", fromUptime = h.clock.uptime - 1_800)
        h.core.fullyDrawn("act")
        h.core.fullyDrawn("other")
        val ttfd = h.sentEvents().filter { it.get("type").asString == "VITAL" && it.getAsJsonObject("vital").get("name").asString == "TTFD" }
        assertEquals(1, ttfd.size)
        assertEquals(1_800.0, ttfd[0].getAsJsonObject("vital").get("value").asDouble, 0.0)
        assertEquals("good", ttfd[0].getAsJsonObject("vital").get("rating").asString)
        assertEquals("Home", ttfd[0].get("view_name").asString)
    }

    @Test fun vsyncIntervalsBecomeFrameCounts() {
        val stats = FrameStats()
        val v = VsyncIntervals(stats)
        var t = 1_000_000_000L
        v.onFrame(t)                                   // first frame: no interval
        repeat(10) { t += 16_666_667; v.onFrame(t) }   // on time
        t += 17_400_000; v.onFrame(t)                  // jitter within tolerance
        t += 33_333_333; v.onFrame(t)                  // one missed vsync: slow
        t += 800_000_000; v.onFrame(t)                 // frozen
        assertEquals(13, stats.total)
        assertEquals(1, stats.slow)
        assertEquals(1, stats.frozen)
    }
}
