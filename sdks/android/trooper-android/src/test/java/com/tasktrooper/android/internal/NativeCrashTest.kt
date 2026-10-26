package com.tasktrooper.android.internal

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class NativeCrashTest {
    private val ctx = CrashContext("sess-1", "anon-1", "view-1", "Checkout", "user-9", sampled = true)

    /** Shape produced by trooper_crash.c (frames 0-2 in libapp.so, 3 unknown, 4 in libc). */
    private fun report(pid: Int = 4242, context: String = ctx.encode(), end: Boolean = true) = buildString {
        append("TTNDK 1\npid $pid\ntid 4250\nthread RenderThread\nsignal 11\ncode 1\nfault_addr 0x0\n")
        append("ts_ms 1700000000123\nabi arm64-v8a\nunwinder unwind\nctx $context\n")
        append("frame 0x7a1b01a2b4 0x1a2b4 3\n")
        append("frame 0x7a1b01a300 0x1a300 3\n")
        append("frame 0x7a1b01a400 0x1a400 3\n")
        append("frame 0x1234 0x1234 -1\n")
        append("frame 0x7c00051000 0x51000 7\n")
        append("module 3 5f2d8e01aa77b0c3d4e5f60718293a4b5c6d7e8f /data/app/~~a1/com.example-1/lib/arm64/libapp.so\n")
        append("module 7 - /apex/com.android.runtime/lib64/bionic/libc.so\n")
        if (end) append("end\n")
    }

    @Test fun parsesReportAndFormatsTombstoneStyleStack() {
        val r = NativeCrashReport.parse(report())!!
        assertEquals(4242, r.pid)
        assertEquals("SIGSEGV", r.signalName)
        assertEquals("RenderThread", r.thread)
        assertEquals(ctx, r.context)
        assertEquals(5, r.frames.size)
        assertTrue(r.complete)
        val stack = r.stack()
        assertTrue(stack, stack.startsWith("signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000\n"))
        assertTrue(stack, stack.contains("      #00 pc 000000000001a2b4  /data/app/~~a1/com.example-1/lib/arm64/libapp.so (BuildId: 5f2d8e01aa77b0c3d4e5f60718293a4b5c6d7e8f)\n"))
        // Return addresses are adjusted by the arm64 instruction size (like libunwindstack).
        assertTrue(stack, stack.contains("      #01 pc 000000000001a2fc  /data/app/"))
        assertTrue(stack, stack.contains("      #03 pc 0000000000001230  <unknown>\n"))
        assertTrue(stack, stack.contains("      #04 pc 0000000000050ffc  /apex/com.android.runtime/lib64/bionic/libc.so\n"))
        val p = r.toPayload()
        assertEquals("SIGSEGV", p.type)
        assertEquals("crash", p.source)
        assertTrue(p.isCrash)
        assertTrue(p.message!!.contains("SEGV_MAPERR"))
    }

    @Test fun rejectsEmptyAndForeignFilesAndKeepsTruncatedReports() {
        assertNull(NativeCrashReport.parse(""))
        assertNull(NativeCrashReport.parse("hello"))
        val r = NativeCrashReport.parse(report(end = false))!!
        assertFalse(r.complete)
        assertEquals("true", r.attributes()["report_truncated"])
    }

    @Test fun realHostHarnessReportParses() {
        // Captured from `:trooper-ndk:hostNativeTest` (macOS arm64): the fp-walk fallback path.
        val text = "TTNDK 1\npid 70639\ntid 120215438\nthread worker-7\nsignal 11\ncode 2\nfault_addr 0x0\n" +
            "ts_ms 1792944806045\nabi host-arm64-v8a\nunwinder fp\nctx session=s-1\tview=v-9 Checkout\n" +
            "frame 0x10228c9d4 0x1000009d4 0\nframe 0x10228c9e8 0x1000009e8 0\n" +
            "module 0 5693c155db883f31b9c7a7e61b691ff5 /tmp/hostbuild/crash_harness\nend\n"
        val r = NativeCrashReport.parse(text)!!
        assertEquals(2, r.frames.size)
        assertNull(r.context) // not a CrashContext line
        assertTrue(r.stack().contains("(BuildId: 5693c155db883f31b9c7a7e61b691ff5)"))
    }

    @Test fun storeSendsPreviousReportsWithTheirSessionAndSkipsCurrentFile() {
        val h = Harness()
        val dir = tempDir()
        val store = NativeCrashStore(dir)
        store.reportFile(4242, 1).writeText(report())
        store.reportFile(5000, 2).writeText("") // previous process that did not crash
        val current = store.reportFile(6000, 3).apply { writeText("") }
        val unsampled = CrashContext("sess-x", "anon-1", null, null, null, sampled = false)
        store.reportFile(4100, 0).writeText(report(pid = 4100, context = unsampled.encode()))

        val pids = store.collect(current, h.core, h.clock)
        assertEquals(setOf(4242, 4100), pids)
        assertEquals(listOf(current.name), dir.list()!!.toList())

        val errors = h.sentEvents().filter { it.get("type").asString == "ERROR" }
        assertEquals(1, errors.size) // the unsampled session's crash is dropped
        val e = errors.single()
        assertEquals("sess-1", e.get("session_id").asString)
        assertEquals("view-1", e.get("view_id").asString)
        assertEquals("Checkout", e.get("view_name").asString)
        assertEquals("user-9", e.getAsJsonObject("user").get("id").asString)
        assertEquals(1700000000123L, e.get("ts_ms").asLong)
        val err = e.getAsJsonObject("error")
        assertEquals("SIGSEGV", err.get("type").asString)
        assertTrue(err.get("is_crash").asBoolean)
        assertEquals("true", e.getAsJsonObject("attributes").get("native").asString)
    }

    @Test fun contextListenerFiresOnSessionAndViewChanges() {
        val h = Harness()
        val seen = ArrayList<CrashContext>()
        h.core.contextListener = { seen.add(it) }
        h.core.startView("Home")
        h.core.track("x", emptyMap(), 0.0)
        h.core.user = com.tasktrooper.android.RumUser("u-1")
        assertNotNull(seen.lastOrNull())
        val last = seen.last()
        assertEquals("Home", last.viewName)
        assertEquals("u-1", last.userId)
        assertEquals(last, CrashContext.decode(last.encode()))
        val distinct = seen.toSet().size
        assertEquals(seen.size, distinct) // only changes are published
    }

    @Test fun processRecordsKeepContextAndOwnKindsAndPrune() {
        val dir = tempDir()
        val r = ProcessRecords(dir, keep = 2)
        r.writeContext(10, ctx)
        r.markOwn(10, "crash")
        r.markOwn(10, "anr")
        assertEquals(ctx, r.context(10))
        assertEquals(setOf("crash", "anr"), r.ownKinds(10))
        for (pid in 11..14) { r.writeContext(pid, ctx); File(dir, "$pid.ctx").setLastModified(1_000L * pid) }
        File(dir, "10.ctx").setLastModified(1); File(dir, "10.own").setLastModified(1)
        r.prune(currentPid = 99)
        assertNull(r.context(10))
        assertEquals(setOf("13.ctx", "14.ctx"), dir.list()!!.toSet())
    }
}
