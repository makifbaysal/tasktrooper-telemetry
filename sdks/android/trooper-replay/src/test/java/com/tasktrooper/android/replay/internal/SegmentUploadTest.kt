package com.tasktrooper.android.replay.internal

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Random

class SegmentUploadTest {
    private class FakeScheduler : ReplayScheduler {
        val delays = ArrayList<Long>()
        val pending = ArrayList<() -> Unit>()
        override fun execute(task: () -> Unit) = task()
        override fun schedule(delayMs: Long, task: () -> Unit) { delays.add(delayMs); pending.add(task) }
        fun runPending() { val p = ArrayList(pending); pending.clear(); p.forEach { it() } }
    }

    private fun dir() = kotlin.io.path.createTempDirectory("replay-test").toFile().apply { deleteOnExit() }

    @Test fun storeDropsNewSegmentsBeyondCap() {
        val store = SegmentStore(dir(), maxBytes = 100)
        assertTrue(store.write(ByteArray(60)))
        assertFalse(store.write(ByteArray(60)))
        assertEquals(1, store.count())
        assertEquals(1, store.dropped)
    }

    @Test fun classifyFollowsContract() {
        assertEquals(SendResult.Success, SendResult.classify(200, null))
        assertEquals(SendResult.Retry(3_000), SendResult.classify(429, "3"))
        assertEquals(SendResult.Retry(null), SendResult.classify(503, null))
        assertEquals(SendResult.Drop(413), SendResult.classify(413, null))
        assertEquals(SendResult.Drop(400), SendResult.classify(400, null))
    }

    @Test fun uploaderRetriesWithBackoffThenDeletes() {
        val store = SegmentStore(dir(), 1_000_000)
        store.write(byteArrayOf(1)); store.write(byteArrayOf(2))
        val results = ArrayDeque(listOf<SendResult>(SendResult.Retry(), SendResult.Success, SendResult.Drop(400)))
        val sent = ArrayList<Byte>()
        val sched = FakeScheduler()
        val up = SegmentUploader(store, { body -> sent.add(body[0]); results.removeFirst() }, sched, Random(1))
        up.drain()
        assertEquals(listOf<Byte>(1), sent)
        assertEquals(2, store.count())
        assertTrue(sched.delays.single() in 500L..1_000L)
        up.drain() // ignored while a retry is pending
        assertEquals(1, sent.size)
        sched.runPending()
        assertEquals(listOf<Byte>(1, 1, 2), sent)
        assertEquals(0, store.count())
    }
}
