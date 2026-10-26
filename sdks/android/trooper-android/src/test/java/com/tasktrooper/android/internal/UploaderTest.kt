package com.tasktrooper.android.internal

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class UploaderTest {
    private fun enqueue(h: Harness, n: Int) = repeat(n) { h.queue.write("""{"resource":{},"events":[{"i":$it}]}""") }

    @Test fun retriesWithExponentialBackoffThenSucceeds() {
        val h = Harness(transport = FakeTransport(SendResult.Retry(), SendResult.Retry(), SendResult.Retry()))
        enqueue(h, 2)
        h.uploader.drain(); h.scheduler.runDue()
        assertEquals(1, h.transport.bodies.size)
        assertEquals(2, h.queue.size())
        // further drains are no-ops while a backoff timer is armed
        h.uploader.drain(); h.scheduler.runDue()
        assertEquals(1, h.transport.bodies.size)

        val delays = h.scheduler.scheduledDelays
        h.scheduler.advance(delays.last()); assertEquals(2, h.transport.bodies.size)
        h.scheduler.advance(delays.last()); assertEquals(3, h.transport.bodies.size)
        assertTrue("backoff grows: $delays", delays[0] < delays[2])
        h.scheduler.advance(delays.last())
        assertEquals(5, h.transport.bodies.size) // success drains both
        assertEquals(0, h.queue.size())
        assertEquals(0, h.uploader.attempt)
    }

    @Test fun clientErrorsDropBatch() {
        val h = Harness(transport = FakeTransport(SendResult.Drop(400)))
        enqueue(h, 2)
        h.uploader.drain(); h.scheduler.runDue()
        assertEquals(2, h.transport.bodies.size)
        assertEquals(0, h.queue.size())
    }

    @Test fun retryAfterIsHonoured() {
        val h = Harness(transport = FakeTransport(SendResult.Retry(retryAfterMs = 30_000)))
        enqueue(h, 1)
        h.uploader.drain(); h.scheduler.runDue()
        assertTrue(h.scheduler.scheduledDelays.last() >= 30_000)
        h.scheduler.advance(29_000); assertEquals(1, h.transport.bodies.size)
        h.scheduler.advance(h.scheduler.scheduledDelays.last() - 29_000); assertEquals(2, h.transport.bodies.size)
    }

    @Test fun statusClassification() {
        val cases = mapOf(
            200 to SendResult.Success::class, 204 to SendResult.Success::class,
            400 to SendResult.Drop::class, 401 to SendResult.Drop::class, 413 to SendResult.Drop::class,
            429 to SendResult.Retry::class, 500 to SendResult.Retry::class, 503 to SendResult.Retry::class,
        )
        for ((status, kind) in cases) assertEquals("HTTP $status", kind, Transport.classify(status, null)::class)
        assertEquals(SendResult.Retry(5_000), Transport.classify(429, "5"))
        assertEquals(SendResult.Retry(null), Transport.classify(503, "Wed, 21 Oct 2015 07:28:00 GMT"))
    }

    @Test fun backoffBoundsWithJitter() {
        val b = Backoff(random = java.util.Random(42))
        repeat(200) {
            for (attempt in 1..12) {
                val d = b.delayMs(attempt)
                val cap = minOf(1_000L shl (attempt - 1), 60_000L)
                assertTrue("attempt=$attempt d=$d", d in cap / 2..cap)
            }
        }
        assertTrue((1..1000).all { b.delayMs(30) <= 60_000 })
    }
}
