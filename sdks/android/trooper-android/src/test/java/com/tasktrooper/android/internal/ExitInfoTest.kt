package com.tasktrooper.android.internal

import com.tasktrooper.android.internal.ExitInfoProcessor.Companion.REASON_ANR
import com.tasktrooper.android.internal.ExitInfoProcessor.Companion.REASON_CRASH
import com.tasktrooper.android.internal.ExitInfoProcessor.Companion.REASON_CRASH_NATIVE
import com.tasktrooper.android.internal.ExitInfoProcessor.Companion.REASON_LOW_MEMORY
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ExitInfoTest {
    private val user = 10 // ApplicationExitInfo.REASON_USER_REQUESTED

    private fun exit(pid: Int, reason: Int, ts: Long, status: Int = 0, importance: Int = 100, trace: String? = null) =
        ExitRecord(pid, reason, status, importance, ts, "desc $pid") { trace }

    @Test fun firstRunOnlyStoresWatermark() {
        val h = Harness()
        val store = MemoryStore()
        val p = ExitInfoProcessor(store, ProcessRecords(tempDir()), h.core)
        assertEquals(0, p.process(listOf(exit(1, REASON_ANR, 500)), emptySet()))
        assertEquals("500", store.get(ExitInfoProcessor.KEY_WATERMARK))
    }

    @Test fun reportsUnseenExitsDedupedAgainstOwnReports() {
        val h = Harness()
        val store = MemoryStore().apply { put(ExitInfoProcessor.KEY_WATERMARK, "100") }
        val records = ProcessRecords(tempDir())
        records.markOwn(3, "crash")      // JVM crash already reported by the uncaught handler
        records.markOwn(4, "anr")        // watchdog already reported this freeze
        records.writeContext(2, CrashContext("old-session", "anon", "v-2", "Cart", null, true))
        val dump = "----- pid 2 -----\n\n\"Signal Catcher\" daemon\n  at x\n\n\"main\" prio=5 tid=1 Blocked\n  at com.example.Cart.load(Cart.kt:10)\n\n\"other\"\n"
        val exits = listOf(
            exit(1, REASON_ANR, 90),                          // older than watermark
            exit(2, REASON_ANR, 200, trace = dump),           // -> ANR
            exit(3, REASON_CRASH, 210),                       // own crash
            exit(4, REASON_ANR, 220),                         // own ANR
            exit(5, REASON_CRASH_NATIVE, 230, status = 11),   // NDK report already sent
            exit(6, REASON_CRASH_NATIVE, 240, status = 6),    // -> SIGABRT (e.g. crash before init)
            exit(7, REASON_LOW_MEMORY, 250, importance = 400),// background LMK: normal
            exit(8, REASON_LOW_MEMORY, 260, importance = 100),// -> foreground LMK
            exit(9, user, 270),                               // user swipe: ignored
        )
        val sent = ExitInfoProcessor(store, records, h.core).process(exits, nativeCrashPids = setOf(5))
        assertEquals(3, sent)
        assertEquals("270", store.get(ExitInfoProcessor.KEY_WATERMARK))
        val errors = h.sentEvents().filter { it.get("type").asString == "ERROR" }
        assertEquals(listOf("ANR", "SIGABRT", "LowMemoryKill"), errors.map { it.getAsJsonObject("error").get("type").asString })
        val anr = errors[0]
        assertEquals("old-session", anr.get("session_id").asString)
        assertEquals("Cart", anr.get("view_name").asString)
        assertEquals(200L, anr.get("ts_ms").asLong)
        val e = anr.getAsJsonObject("error")
        assertEquals("anr", e.get("source").asString)
        val stack = e.get("stack").asString
        assertTrue(stack, stack.startsWith("ANR\n\"main\" prio=5"))
        assertTrue(stack, stack.contains("Cart.load") && !stack.contains("Signal Catcher") && !stack.contains("other"))
        assertEquals("anr", anr.getAsJsonObject("attributes").get("exit_reason").asString)
        assertEquals("crash", errors[1].getAsJsonObject("error").get("source").asString)

        // Second launch: nothing new.
        assertEquals(0, ExitInfoProcessor(store, records, h.core).process(exits, emptySet()))
    }

    @Test fun mapIgnoresUnknownReasons() {
        val p = ExitInfoProcessor(MemoryStore(), ProcessRecords(tempDir()), Harness().core)
        assertNull(p.map(exit(1, 1 /* EXIT_SELF */, 1), emptySet(), emptySet()))
    }
}
