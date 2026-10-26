package com.tasktrooper.android.internal

import com.tasktrooper.android.RumResource

/**
 * In-memory batching: flush when [maxBatch] events are buffered or [flushIntervalMs] after the
 * first buffered event. Flushed batches go to the [DiskQueue] first, then the [Uploader] drains it.
 */
internal class Pipeline(
    private val resource: RumResource,
    private val queue: DiskQueue,
    private val uploader: Uploader,
    private val scheduler: Scheduler,
    private val maxBatch: Int = 100,
    private val flushIntervalMs: Long = 5_000,
) {
    private val lock = Any()
    private var buffer = ArrayList<String>(maxBatch)
    private var timer: Cancellable? = null

    fun add(eventJson: String) {
        var flushNow = false
        synchronized(lock) {
            buffer.add(eventJson)
            if (buffer.size % maxBatch == 0) {
                flushNow = true
            } else if (timer == null) {
                timer = scheduler.schedule(flushIntervalMs) { flush() }
            }
        }
        if (flushNow) scheduler.execute { flush() }
    }

    /** Off-thread [flush]. */
    fun flushAsync() = scheduler.execute { flush() }

    /** Persists buffered events and triggers an upload. */
    fun flush() {
        persist()
        uploader.drain()
    }

    /** Synchronous: used from the uncaught exception handler; no upload is attempted. */
    fun persist(extra: String? = null): Boolean {
        val events: List<String>
        synchronized(lock) {
            if (extra != null) buffer.add(extra)
            timer?.cancel()
            timer = null
            if (buffer.isEmpty()) return false
            events = buffer
            buffer = ArrayList(maxBatch)
        }
        var ok = false
        // A full buffer may exceed maxBatch only via `extra`; split to respect the ≤100 contract.
        for (chunk in events.chunked(maxBatch)) ok = queue.write(resource.batchJson(chunk)) or ok
        return ok
    }

    fun buffered(): Int = synchronized(lock) { buffer.size }
}

/** Drains the disk queue oldest-first; backs off on 429/5xx/network errors. Runs on its own scheduler. */
internal class Uploader(
    private val queue: DiskQueue,
    private val transport: Transport,
    private val scheduler: Scheduler,
    private val backoff: Backoff = Backoff(),
) {
    @Volatile var attempt = 0
        private set
    private var retryPending = false

    fun drain() = scheduler.execute { drainNow(fromRetry = false) }

    private fun drainNow(fromRetry: Boolean) {
        if (fromRetry) retryPending = false
        if (retryPending) return // a backoff timer is armed; it will drain
        while (true) {
            val file = queue.oldest() ?: return
            val body = try { file.readBytes() } catch (t: Throwable) { queue.remove(file); continue }
            when (val r = transport.send(body)) {
                is SendResult.Success -> { attempt = 0; queue.remove(file) }
                is SendResult.Drop -> { attempt = 0; queue.remove(file); Internal.debug("batch dropped: HTTP ${r.status}") }
                is SendResult.Retry -> {
                    attempt++
                    val delay = backoff.delayMs(attempt, r.retryAfterMs)
                    retryPending = true
                    scheduler.schedule(delay) { drainNow(fromRetry = true) }
                    return
                }
            }
        }
    }
}
