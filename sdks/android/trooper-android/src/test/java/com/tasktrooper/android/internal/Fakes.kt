package com.tasktrooper.android.internal

import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.tasktrooper.android.RumResource
import java.io.File
import java.util.zip.GZIPInputStream

internal class FakeClock(var now: Long = 1_700_000_000_000L, var uptime: Long = 10_000L) : Clock {
    override fun nowMs() = now
    override fun uptimeMs() = uptime
    fun advance(ms: Long) { now += ms; uptime += ms }
}

/** Deterministic scheduler driven by [FakeClock.uptime]. */
internal class FakeScheduler(private val clock: FakeClock) : Scheduler {
    private class Task(val due: Long, val seq: Long, val run: () -> Unit) { var cancelled = false }
    private val tasks = ArrayList<Task>()
    private var seq = 0L
    val scheduledDelays = ArrayList<Long>()

    override fun execute(task: () -> Unit) { tasks.add(Task(clock.uptime, seq++, task)) }

    override fun schedule(delayMs: Long, task: () -> Unit): Cancellable {
        scheduledDelays.add(delayMs)
        val t = Task(clock.uptime + delayMs, seq++, task)
        tasks.add(t)
        return object : Cancellable { override fun cancel() { t.cancelled = true } }
    }

    /** Runs every task due at the current time (including ones they enqueue). */
    fun runDue() {
        while (true) {
            val next = tasks.filter { !it.cancelled && it.due <= clock.uptime }.minWithOrNull(compareBy({ it.due }, { it.seq })) ?: break
            tasks.remove(next)
            next.run()
        }
        tasks.removeAll { it.cancelled }
    }

    fun advance(ms: Long) { clock.advance(ms); runDue() }
    fun pending(): Int = tasks.count { !it.cancelled }
}

internal class FakeTransport(vararg results: SendResult) : Transport {
    private val results = ArrayDeque(results.toList())
    val bodies = ArrayList<String>()
    override fun send(gzippedBody: ByteArray): SendResult {
        bodies.add(gunzip(gzippedBody))
        return results.removeFirstOrNull() ?: SendResult.Success
    }
    fun batches(): List<JsonObject> = bodies.map { JsonParser.parseString(it).asJsonObject }
}

internal fun gunzip(b: ByteArray): String = GZIPInputStream(b.inputStream()).use { it.readBytes().toString(Charsets.UTF_8) }

internal fun testResource() = RumResource(
    app = "test-app", version = "1.2.3+45", environment = "test", sdkVersion = "0.1.0",
    deviceType = "mobile", manufacturer = "Google", model = "Pixel 9", locale = "en-US",
    screenWidth = 1080, screenHeight = 2400, osVersion = "16", attributes = mapOf("os.api_level" to "36"),
)

internal fun tempDir(): File = kotlin.io.path.createTempDirectory("trooper-test").toFile().apply { deleteOnExit() }

/** Full core wired with fakes. */
internal class Harness(
    sampleRate: Double = 1.0,
    val clock: FakeClock = FakeClock(),
    val dir: File = tempDir(),
    val transport: FakeTransport = FakeTransport(),
    maxBytes: Long = 5L * 1024 * 1024,
    config: com.tasktrooper.android.TrooperConfig = com.tasktrooper.android.TrooperConfig(dsn = "https://k@h/1", app = "test-app", sampleRate = sampleRate),
    store: KeyValueStore = MemoryStore(),
) {
    val scheduler = FakeScheduler(clock)
    val queue = DiskQueue(dir, maxBytes)
    val uploader = Uploader(queue, transport, scheduler, Backoff(random = java.util.Random(1)))
    val pipeline = Pipeline(testResource(), queue, uploader, scheduler, config.maxBatchSize, config.flushIntervalMs)
    val sessions = SessionManager(clock, store, config.sampleRate)
    val core = RumCore(config, clock, sessions, pipeline) { "wifi" }

    /** Flushes and uploads; returns every event sent so far. */
    fun sentEvents(): List<JsonObject> {
        pipeline.flush(); scheduler.runDue()
        return transport.batches().flatMap { b -> b.getAsJsonArray("events").map { it.asJsonObject } }
    }
}
