package com.tasktrooper.android.internal

import org.junit.Assert.assertEquals
import org.junit.Test

/** Drives [AnrDetector] with a fake main looper (a runnable list) and a fake clock. */
class AnrDetectorTest {
    private val mainQueue = ArrayList<Runnable>()
    private var now = 0L
    private val reports = ArrayList<Array<StackTraceElement>>()
    private val stack = arrayOf(StackTraceElement("com.app.Main", "blockingCall", "Main.kt", 42))
    private val detector = AnrDetector(5_000, { mainQueue.add(it) }, { stack }, { reports.add(it) }, pollMs = 500)

    private fun loopMain() { val q = ArrayList(mainQueue); mainQueue.clear(); q.forEach { it.run() } }

    /** Runs the watchdog for [ms] of fake time; the main thread processes messages only if [responsive]. */
    private fun run(ms: Long, responsive: Boolean) {
        val end = now + ms
        while (now < end) {
            val sleep = detector.step(now)
            if (responsive) loopMain()
            now += sleep
        }
    }

    @Test fun responsiveMainThreadNeverReports() {
        run(60_000, responsive = true)
        assertEquals(0, reports.size)
    }

    @Test fun blockedMainThreadReportsOnceWithStack() {
        run(1_000, responsive = true)
        run(4_000, responsive = false)
        assertEquals(0, reports.size) // < 5 s blocked
        run(2_000, responsive = false)
        assertEquals(1, reports.size)
        assertEquals("blockingCall", reports[0][0].methodName)
        run(30_000, responsive = false)
        assertEquals(1, reports.size) // same freeze reported once
    }

    @Test fun rearmsAfterRecovery() {
        run(6_000, responsive = false)
        run(2_000, responsive = true)
        run(6_000, responsive = false)
        assertEquals(2, reports.size)
    }

    @Test fun anrEventShape() {
        val h = Harness()
        h.core.anr(stack)
        val e = h.sentEvents().last()
        assertEquals("ERROR", e.get("type").asString)
        val p = e.getAsJsonObject("error")
        assertEquals("anr", p.get("source").asString)
        assertEquals("main", p.get("thread").asString)
        assertEquals(false, p.has("is_crash"))
    }
}
