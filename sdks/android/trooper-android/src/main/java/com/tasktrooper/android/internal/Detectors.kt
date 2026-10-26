package com.tasktrooper.android.internal

import java.security.SecureRandom

/**
 * Main-thread watchdog logic (thread-free so it can be tested with a fake looper + clock).
 * [step] posts a ping to the main thread; if it is not acknowledged within [thresholdMs], [onAnr]
 * fires once with the main thread stack. It re-arms after the main thread recovers.
 */
internal class AnrDetector(
    private val thresholdMs: Long,
    private val postToMain: (Runnable) -> Unit,
    private val mainStack: () -> Array<StackTraceElement>,
    private val onAnr: (Array<StackTraceElement>) -> Unit,
    private val pollMs: Long = 500,
) {
    @Volatile private var acked = true
    private var pingAt = -1L
    private var reported = false

    /** Advances the state machine at [nowMs]; returns how long the caller should sleep before the next step. */
    fun step(nowMs: Long): Long {
        if (pingAt < 0 || acked) {
            if (acked && reported) reported = false
            acked = false
            pingAt = nowMs
            postToMain(Runnable { acked = true })
            return pollMs
        }
        if (!reported && nowMs - pingAt >= thresholdMs) {
            reported = true
            onAnr(mainStack())
        }
        return pollMs
    }
}

/** Runs [AnrDetector] on a daemon thread. */
internal class AnrWatchdogThread(private val detector: AnrDetector, private val clock: Clock) :
    Thread("trooper-anr-watchdog") {
    init { isDaemon = true }

    @Volatile var running = true

    override fun run() {
        while (running) {
            try {
                if (android.os.Debug.isDebuggerConnected()) { sleep(1_000); continue }
                sleep(detector.step(clock.uptimeMs()))
            } catch (_: InterruptedException) {
                return
            } catch (t: Throwable) {
                Internal.error("anr", t)
                return
            }
        }
    }
}

/**
 * App start classification. Times are uptime ms.
 * - cold: first activity of a process created shortly after process start; duration = process start → first frame.
 * - warm: activity re-created while the process was already alive in background; duration = onCreate → first frame.
 * - hot: app brought to foreground without re-creating an activity; duration = onStart → first frame.
 */
internal class AppStartTracker(
    private val processStartUptime: Long,
    private val coldStartWindowMs: Long = 10_000,
    private val onAppStart: (kind: String, durationMs: Double, fromProcessStart: Boolean) -> Unit,
) {
    private var startedActivities = 0
    private var firstActivitySeen = false
    private var pendingKind: String? = null
    private var pendingFrom = 0L

    fun onActivityCreated(nowUptime: Long) {
        if (!firstActivitySeen) {
            firstActivitySeen = true
            if (nowUptime - processStartUptime <= coldStartWindowMs) {
                pendingKind = "cold"; pendingFrom = processStartUptime
            } else {
                pendingKind = "warm"; pendingFrom = nowUptime
            }
        } else if (startedActivities == 0 && pendingKind == null) {
            pendingKind = "warm"; pendingFrom = nowUptime
        }
    }

    fun onActivityStarted(nowUptime: Long) {
        if (startedActivities == 0 && pendingKind == null && firstActivitySeen) {
            pendingKind = "hot"; pendingFrom = nowUptime
        }
        startedActivities++
    }

    fun onActivityStopped() {
        if (startedActivities > 0) startedActivities--
    }

    /** Called on the first frame after an activity resumed. */
    fun onFirstFrame(nowUptime: Long) {
        val kind = pendingKind ?: return
        pendingKind = null
        val d = (nowUptime - pendingFrom).coerceAtLeast(0)
        onAppStart(kind, d.toDouble(), kind == "cold")
    }
}

/** Detects rage taps: ≥3 taps on the same target within 1 s. */
internal class RageTapDetector(private val windowMs: Long = 1_000, private val threshold: Int = 3) {
    private var target: String? = null
    private val times = ArrayDeque<Long>()

    @Synchronized
    fun onTap(target: String, nowMs: Long): Boolean {
        if (target != this.target) { this.target = target; times.clear() }
        times.addLast(nowMs)
        while (times.isNotEmpty() && nowMs - times.first() > windowMs) times.removeFirst()
        return times.size >= threshold
    }
}

/** Accumulates frame durations for the current view. */
internal class FrameStats(private val slowThresholdMs: Double = 16.0, private val frozenThresholdMs: Double = 700.0) {
    var total = 0; private set
    var slow = 0; private set
    var frozen = 0; private set

    @Synchronized
    fun record(durationMs: Double) {
        total++
        if (durationMs > frozenThresholdMs) frozen++ else if (durationMs > slowThresholdMs) slow++
    }

    @Synchronized
    fun reset() { total = 0; slow = 0; frozen = 0 }
}

/**
 * W3C `baggage` helper: carries the calling app name as `tt.svc=<app>` so backends can attribute the
 * call (`trooper.caller`, contracts.md §4).
 */
object Baggage {
    const val HEADER = "baggage"
    const val SERVICE_KEY = "tt.svc"
    /** W3C limit for the whole header. */
    const val MAX_BYTES = 8192

    /**
     * Merges `tt.svc=<service>` into [existing], replacing any existing `tt.svc` member and keeping the rest.
     * Returns [existing] unchanged when [service] is blank, the result would exceed [MAX_BYTES], or on any error.
     */
    @JvmStatic
    fun merge(existing: String?, service: String?): String? = try {
        if (service.isNullOrEmpty()) existing else {
            val own = SERVICE_KEY + "=" + java.net.URLEncoder.encode(service, "UTF-8").replace("+", "%20")
            val others = (existing ?: "").split(',').map { it.trim() }
                .filter { it.isNotEmpty() && it.split('=', ';')[0].trim() != SERVICE_KEY }
            val out = (listOf(own) + others).joinToString(",")
            if (out.toByteArray(Charsets.UTF_8).size <= MAX_BYTES) out else existing
        }
    } catch (_: Throwable) {
        existing
    }
}

/** W3C trace context ids. */
object TraceIds {
    private val random = SecureRandom()
    private const val HEX = "0123456789abcdef"

    private fun hex(bytes: Int): String {
        while (true) {
            val b = ByteArray(bytes)
            random.nextBytes(b)
            if (b.any { it.toInt() != 0 }) {
                val sb = StringBuilder(bytes * 2)
                for (x in b) { sb.append(HEX[(x.toInt() shr 4) and 0xF]); sb.append(HEX[x.toInt() and 0xF]) }
                return sb.toString()
            }
        }
    }

    @JvmStatic fun traceId(): String = hex(16)
    @JvmStatic fun spanId(): String = hex(8)
    @JvmStatic fun traceparent(traceId: String, spanId: String, sampled: Boolean = true): String =
        "00-$traceId-$spanId-${if (sampled) "01" else "00"}"
}

/** Strips query values (keys kept) and the fragment, per rum.proto ResourcePayload.url. */
object UrlSanitizer {
    @JvmStatic
    fun sanitize(url: String): String {
        val noFrag = url.substringBefore('#')
        val q = noFrag.indexOf('?')
        if (q < 0) return noFrag
        val keys = noFrag.substring(q + 1).split('&').filter { it.isNotEmpty() }.map { it.substringBefore('=') + "=" }
        return if (keys.isEmpty()) noFrag.substring(0, q) else noFrag.substring(0, q) + "?" + keys.joinToString("&")
    }
}
