package com.tasktrooper.android.replay.internal

import com.tasktrooper.android.spi.TrooperSpi
import java.io.File
import java.io.FileOutputStream
import java.net.HttpURLConnection
import java.net.URL
import java.util.Locale
import java.util.Random
import java.util.concurrent.ScheduledThreadPoolExecutor
import java.util.concurrent.TimeUnit

/**
 * Offline segment store: one gzipped segment per file (tmp + rename). Unlike the RUM queue, a segment that
 * would push the directory past [maxBytes] is **dropped** (replay is optional data; the disk stays small).
 */
internal class SegmentStore(private val dir: File, private val maxBytes: Long) {
    private var seq = 0L
    var dropped = 0L
        private set

    init {
        Guard.run("replay.store.init", Unit) {
            dir.mkdirs()
            dir.listFiles()?.filter { it.name.endsWith(".tmp") }?.forEach { it.delete() }
        }
    }

    @Synchronized
    fun write(gz: ByteArray): Boolean = Guard.run("replay.store.write", false) {
        if (totalBytes() + gz.size > maxBytes) {
            dropped++
            TrooperSpi.debug("replay: offline store full, segment dropped")
            return@run false
        }
        val name = String.format(Locale.ROOT, "%019d-%06d", System.currentTimeMillis(), (seq++) % 1_000_000)
        val tmp = File(dir, "$name.tmp")
        FileOutputStream(tmp).use { out ->
            out.write(gz)
            try { out.fd.sync() } catch (_: Throwable) {}
        }
        tmp.renameTo(File(dir, "$name.seg"))
    }

    @Synchronized fun oldest(): File? = files().firstOrNull()
    @Synchronized fun remove(f: File) { f.delete() }
    @Synchronized fun count(): Int = files().size
    @Synchronized fun totalBytes(): Long = files().sumOf { it.length() }

    private fun files(): List<File> = (dir.listFiles { f -> f.name.endsWith(".seg") } ?: emptyArray()).sortedBy { it.name }
}

internal sealed class SendResult {
    object Success : SendResult()
    data class Drop(val status: Int) : SendResult()
    data class Retry(val retryAfterMs: Long? = null) : SendResult()

    companion object {
        /** contracts.md §2: 2xx ok; 429/5xx retry with backoff; other 4xx (incl. 413) drop. */
        fun classify(status: Int, retryAfter: String?): SendResult = when {
            status in 200..299 -> Success
            status == 429 || status >= 500 -> Retry(retryAfter?.trim()?.toLongOrNull()?.takeIf { it >= 0 }?.times(1000))
            else -> Drop(status)
        }
    }
}

internal fun interface SegmentTransport {
    fun send(gz: ByteArray): SendResult
}

/** `POST /v1/replay`, gzip body, `x-trooper-key` (same conventions as `/v1/rum`, contracts.md §23). */
internal class HttpSegmentTransport(private val ingest: () -> TrooperSpi.Ingest?) : SegmentTransport {
    override fun send(gz: ByteArray): SendResult {
        val target = ingest() ?: return SendResult.Retry()
        var conn: HttpURLConnection? = null
        return try {
            conn = (URL(target.baseUrl + "/v1/replay").openConnection() as HttpURLConnection).apply {
                requestMethod = "POST"
                connectTimeout = 10_000
                readTimeout = 15_000
                doOutput = true
                useCaches = false
                setFixedLengthStreamingMode(gz.size)
                setRequestProperty("Content-Type", "application/json")
                setRequestProperty("Content-Encoding", "gzip")
                setRequestProperty("x-trooper-key", target.key)
            }
            conn.outputStream.use { it.write(gz) }
            val status = conn.responseCode
            try { (if (status >= 400) conn.errorStream else conn.inputStream)?.use { it.readBytes() } } catch (_: Throwable) {}
            SendResult.classify(status, conn.getHeaderField("Retry-After"))
        } catch (t: Throwable) {
            TrooperSpi.debug("replay upload failed: $t")
            SendResult.Retry()
        } finally {
            try { conn?.disconnect() } catch (_: Throwable) {}
        }
    }
}

internal interface ReplayScheduler {
    fun execute(task: () -> Unit)
    fun schedule(delayMs: Long, task: () -> Unit)
}

internal class ExecutorReplayScheduler(name: String) : ReplayScheduler {
    private val executor = ScheduledThreadPoolExecutor(1) { r ->
        Thread(r, name).apply { isDaemon = true; priority = Thread.MIN_PRIORITY }
    }.apply { executeExistingDelayedTasksAfterShutdownPolicy = false }

    /** Runs the tasks already queued, drops delayed ones (timers, upload retries: the files stay on disk). */
    fun shutdown() { try { executor.shutdown() } catch (_: Throwable) {} }

    override fun execute(task: () -> Unit) {
        if (executor.isShutdown) return
        try { executor.execute { Guard.run("replay.task", Unit, task) } } catch (t: Throwable) { Guard.report("replay.execute", t) }
    }

    override fun schedule(delayMs: Long, task: () -> Unit) {
        if (executor.isShutdown) return
        try { executor.schedule({ Guard.run("replay.task", Unit, task) }, delayMs, TimeUnit.MILLISECONDS) }
        catch (t: Throwable) { Guard.report("replay.schedule", t) }
    }
}

/** Uploads stored segments oldest-first; exponential backoff 1 s → 60 s with jitter on 429/5xx/IO errors. */
internal class SegmentUploader(
    private val store: SegmentStore,
    private val transport: SegmentTransport,
    private val scheduler: ReplayScheduler,
    private val random: Random = Random(),
) {
    private var attempt = 0
    private var retryPending = false

    fun drain() = scheduler.execute { drainNow(false) }

    private fun drainNow(fromRetry: Boolean) {
        if (fromRetry) retryPending = false
        if (retryPending) return
        while (true) {
            val f = store.oldest() ?: return
            val body = try { f.readBytes() } catch (t: Throwable) { store.remove(f); continue }
            when (val r = transport.send(body)) {
                is SendResult.Success -> { attempt = 0; store.remove(f) }
                is SendResult.Drop -> { attempt = 0; store.remove(f); TrooperSpi.debug("replay segment dropped: HTTP ${r.status}") }
                is SendResult.Retry -> {
                    attempt++
                    val capped = minOf(1_000L * (1L shl (attempt - 1).coerceIn(0, 16)), 60_000L)
                    val delay = maxOf(r.retryAfterMs ?: 0L, capped / 2 + (random.nextDouble() * (capped / 2)).toLong())
                    retryPending = true
                    scheduler.schedule(delay) { drainNow(true) }
                    return
                }
            }
        }
    }
}
