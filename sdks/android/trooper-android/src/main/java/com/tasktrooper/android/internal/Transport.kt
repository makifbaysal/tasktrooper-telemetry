package com.tasktrooper.android.internal

import java.net.HttpURLConnection
import java.net.URL
import java.util.Random

internal sealed class SendResult {
    object Success : SendResult()
    /** Non-retryable (4xx except 429): drop the batch. */
    data class Drop(val status: Int) : SendResult()
    /** 429 / 5xx / IO error: keep the batch and back off. */
    data class Retry(val retryAfterMs: Long? = null) : SendResult()
}

internal interface Transport {
    fun send(gzippedBody: ByteArray): SendResult

    companion object {
        fun classify(status: Int, retryAfterHeader: String?): SendResult = when {
            status in 200..299 -> SendResult.Success
            status == 429 || status >= 500 -> SendResult.Retry(parseRetryAfter(retryAfterHeader))
            else -> SendResult.Drop(status)
        }

        fun parseRetryAfter(v: String?): Long? = v?.trim()?.toLongOrNull()?.takeIf { it >= 0 }?.times(1000)
    }
}

/** POST /v1/rum with gzip, using HttpURLConnection (no OkHttp dependency in core). */
internal class HttpTransport(private val endpoint: String, private val key: String) : Transport {
    override fun send(gzippedBody: ByteArray): SendResult {
        var conn: HttpURLConnection? = null
        return try {
            conn = (URL(endpoint).openConnection() as HttpURLConnection).apply {
                requestMethod = "POST"
                connectTimeout = 10_000
                readTimeout = 15_000
                doOutput = true
                useCaches = false
                setFixedLengthStreamingMode(gzippedBody.size)
                setRequestProperty("Content-Type", "application/json")
                setRequestProperty("Content-Encoding", "gzip")
                setRequestProperty("x-trooper-key", key)
            }
            conn.outputStream.use { it.write(gzippedBody) }
            val status = conn.responseCode
            try { (if (status >= 400) conn.errorStream else conn.inputStream)?.use { it.readBytes() } } catch (_: Throwable) {}
            Transport.classify(status, conn.getHeaderField("Retry-After"))
        } catch (t: Throwable) {
            Internal.debug("upload failed: $t")
            SendResult.Retry()
        } finally {
            try { conn?.disconnect() } catch (_: Throwable) {}
        }
    }
}

/** Exponential backoff 1 s → 60 s with full jitter (contracts.md §2). */
internal class Backoff(
    private val baseMs: Long = 1_000,
    private val maxMs: Long = 60_000,
    private val random: Random = Random(),
) {
    fun delayMs(attempt: Int, retryAfterMs: Long? = null): Long {
        val exp = baseMs * (1L shl (attempt - 1).coerceIn(0, 16))
        val capped = minOf(exp, maxMs)
        // Jitter in [capped/2, capped]: avoids thundering herds while guaranteeing growth.
        val jittered = capped / 2 + (random.nextDouble() * (capped / 2)).toLong()
        // Retry-After from the server wins when it asks for a longer pause.
        return if (retryAfterMs != null) maxOf(retryAfterMs, jittered) else jittered
    }
}
