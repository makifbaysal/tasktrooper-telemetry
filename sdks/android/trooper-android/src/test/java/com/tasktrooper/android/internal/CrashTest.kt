package com.tasktrooper.android.internal

import org.junit.Assert.assertEquals
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

class CrashTest {
    @Test fun crashIsPersistedSynchronouslyAndSentOnNextLaunch() {
        val dir = tempDir()
        val first = Harness(dir = dir)
        first.core.startView("CheckoutActivity")
        first.core.track("before_crash", emptyMap(), 0.0)
        assertEquals(0, first.queue.size())

        first.core.crash(Thread.currentThread(), IllegalStateException("boom"))
        // Written to disk on the crashing thread, without running the scheduler.
        assertEquals(1, first.queue.size())
        assertEquals(0, first.transport.bodies.size)

        // "Next launch": a new process with the same queue directory.
        val second = Harness(dir = dir)
        second.uploader.drain(); second.scheduler.runDue()
        val events = second.transport.batches().single().getAsJsonArray("events").map { it.asJsonObject }
        val types = events.map { it.get("type").asString }
        assertEquals(listOf("SESSION_START", "VIEW_START", "CUSTOM", "ERROR"), types)
        val err = events.last()
        val payload = err.getAsJsonObject("error")
        assertTrue(payload.get("is_crash").asBoolean)
        assertEquals("crash", payload.get("source").asString)
        assertEquals("java.lang.IllegalStateException", payload.get("type").asString)
        assertEquals("boom", payload.get("message").asString)
        assertTrue(payload.get("stack").asString.contains("CrashTest"))
        assertEquals("CheckoutActivity", err.get("view_name").asString)
        assertEquals(0, second.queue.size())
    }

    @Test fun handlerChainsPreviousAndNeverThrows() {
        val h = Harness()
        var forwarded: Throwable? = null
        val handler = CrashHandler(h.core, { _, e -> forwarded = e })
        val ex = RuntimeException("x")
        handler.uncaughtException(Thread.currentThread(), ex)
        assertSame(ex, forwarded)
        assertEquals(1, h.queue.size())
    }
}
