package com.tasktrooper.android.internal

import android.os.SystemClock
import android.util.Log
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ThreadFactory
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong

internal interface Clock {
    /** Unix epoch ms. */
    fun nowMs(): Long

    /** Monotonic ms (SystemClock.uptimeMillis on device). */
    fun uptimeMs(): Long
}

internal object SystemClockImpl : Clock {
    override fun nowMs() = System.currentTimeMillis()
    override fun uptimeMs() = SystemClock.uptimeMillis()
}

internal interface Cancellable { fun cancel() }

/** Small scheduling abstraction so pipeline/uploader logic can be tested with a fake clock. */
internal interface Scheduler {
    fun execute(task: () -> Unit)
    fun schedule(delayMs: Long, task: () -> Unit): Cancellable
}

internal class ExecutorScheduler(name: String) : Scheduler {
    private val executor: ScheduledExecutorService = Executors.newSingleThreadScheduledExecutor(ThreadFactory { r ->
        Thread(r, name).apply { isDaemon = true; priority = Thread.MIN_PRIORITY }
    })

    override fun execute(task: () -> Unit) {
        try { executor.execute { safely(task) } } catch (t: Throwable) { Internal.error("execute", t) }
    }

    override fun schedule(delayMs: Long, task: () -> Unit): Cancellable = try {
        val f = executor.schedule({ safely(task) }, delayMs, TimeUnit.MILLISECONDS)
        object : Cancellable { override fun cancel() { f.cancel(false) } }
    } catch (t: Throwable) {
        Internal.error("schedule", t)
        object : Cancellable { override fun cancel() {} }
    }

    private fun safely(task: () -> Unit) = try { task() } catch (t: Throwable) { Internal.error("task", t) }
}

/** Swallows SDK-internal failures (AGENTS.md rule 5) and counts them. */
internal object Internal {
    const val TAG = "TaskTrooper"
    @Volatile var debug = false
    val errorCount = AtomicLong()
    val droppedEvents = AtomicLong()

    fun error(where: String, t: Throwable?) {
        errorCount.incrementAndGet()
        if (debug) try { Log.w(TAG, "internal error in $where", t) } catch (_: Throwable) {}
    }

    fun debug(msg: String) {
        if (debug) try { Log.d(TAG, msg) } catch (_: Throwable) {}
    }

    inline fun <T> guard(where: String, fallback: T, block: () -> T): T =
        try { block() } catch (t: Throwable) { error(where, t); fallback }
}

/** Persistent key/value store (SharedPreferences on device, in-memory in tests). */
internal interface KeyValueStore {
    fun get(key: String): String?
    fun put(key: String, value: String)
}

internal class MemoryStore : KeyValueStore {
    private val map = HashMap<String, String>()
    @Synchronized override fun get(key: String) = map[key]
    @Synchronized override fun put(key: String, value: String) { map[key] = value }
}
