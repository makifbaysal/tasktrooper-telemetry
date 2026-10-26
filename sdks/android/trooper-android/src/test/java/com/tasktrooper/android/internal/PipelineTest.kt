package com.tasktrooper.android.internal

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class PipelineTest {
    @Test fun flushesImmediatelyAt100Events() {
        val h = Harness()
        repeat(150) { h.pipeline.add("""{"type":"CUSTOM","ts_ms":$it}""") }
        h.scheduler.runDue()
        val batches = h.transport.batches()
        // flush was triggered at 100; events that arrived before the worker ran are split into ≤100 chunks
        assertEquals(listOf(100, 50), batches.map { it.getAsJsonArray("events").size() })
        assertEquals("test-app", batches[0].getAsJsonObject("resource").get("app").asString)
        assertEquals(0, h.pipeline.buffered())
    }

    @Test fun flushesAfter5Seconds() {
        val h = Harness()
        repeat(3) { h.pipeline.add("""{"type":"CUSTOM"}""") }
        h.scheduler.advance(4_999)
        assertEquals(0, h.transport.bodies.size)
        h.scheduler.advance(1)
        assertEquals(1, h.transport.bodies.size)
        assertEquals(3, h.transport.batches()[0].getAsJsonArray("events").size())
        assertEquals(0, h.queue.size())
    }

    @Test fun explicitFlushOnBackground() {
        val h = Harness()
        h.pipeline.add("""{"type":"CUSTOM"}""")
        h.pipeline.flushAsync()
        h.scheduler.runDue()
        assertEquals(1, h.transport.bodies.size)
    }

    @Test fun batchesAreStoredOnDiskBeforeUpload() {
        val h = Harness(transport = FakeTransport(SendResult.Retry()))
        h.pipeline.add("""{"type":"CUSTOM"}""")
        h.pipeline.flush()
        h.scheduler.runDue()
        assertEquals(1, h.queue.size())
        assertTrue(h.queue.oldest()!!.name.endsWith(".batch"))
    }

    @Test fun diskQueueDropsOldestWhenOverCap() {
        val dir = tempDir()
        val q = DiskQueue(dir, maxBytes = 3_000)
        val random = java.util.Random(7)
        val names = (1..20).map {
            // incompressible payload ~ 600 B gzipped
            val s = (1..800).map { ('a' + random.nextInt(26)) }.joinToString("")
            q.write("""{"i":$it,"p":"$s"}""")
            q.all().last().name
        }
        assertTrue(q.totalBytes() <= 3_000)
        val remaining = q.all().map { it.name }
        assertEquals(names.takeLast(remaining.size), remaining) // FIFO: newest survive
        assertTrue(remaining.size in 2..5)
    }

    @Test fun leftoverQueueIsSentByNextInstance() {
        val dir = tempDir()
        val offline = Harness(dir = dir, transport = FakeTransport(SendResult.Retry(), SendResult.Retry()))
        offline.pipeline.add("""{"type":"CUSTOM"}"""); offline.pipeline.flush(); offline.scheduler.runDue()
        assertEquals(1, offline.queue.size())

        val next = Harness(dir = dir)
        next.uploader.drain(); next.scheduler.runDue()
        assertEquals(1, next.transport.bodies.size)
        assertEquals(0, next.queue.size())
    }
}
