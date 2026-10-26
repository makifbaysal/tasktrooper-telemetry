package com.tasktrooper.android.internal

import android.annotation.TargetApi
import android.app.ActivityManager
import android.app.ApplicationExitInfo
import android.content.Context
import com.tasktrooper.android.RumPayload
import java.io.File

/**
 * Small per-process records in `<dir>/<pid>.ctx` / `<pid>.own`, read by a later process:
 * - `.ctx`: last [CrashContext] (session/view/user) so exits are attributed to the right session/screen;
 * - `.own`: kinds this SDK already reported for that process (`crash`, `anr`), to dedupe ApplicationExitInfo.
 * Only the newest [keep] processes are kept.
 */
internal class ProcessRecords(private val dir: File, private val keep: Int = 8) {
    init { Internal.guard("records.mkdirs", Unit) { dir.mkdirs() } }

    fun writeContext(pid: Int, ctx: CrashContext) = Internal.guard("records.ctx", Unit) {
        val tmp = File(dir, "$pid.ctx.tmp")
        tmp.writeText(ctx.encode())
        tmp.renameTo(File(dir, "$pid.ctx"))
    }

    /** Synchronous (used from the crash handler). */
    fun markOwn(pid: Int, kind: String) = Internal.guard("records.own", Unit) {
        File(dir, "$pid.own").appendText("$kind\n")
    }

    fun context(pid: Int): CrashContext? = Internal.guard("records.read", null) {
        val f = File(dir, "$pid.ctx")
        if (f.exists()) CrashContext.decode(f.readText()) else null
    }

    fun ownKinds(pid: Int): Set<String> = Internal.guard("records.read", emptySet()) {
        val f = File(dir, "$pid.own")
        if (f.exists()) f.readLines().filter { it.isNotBlank() }.toSet() else emptySet()
    }

    /** Deletes records of all but the newest [keep] processes (besides [currentPid]). */
    fun prune(currentPid: Int) = Internal.guard("records.prune", Unit) {
        val byPid = (dir.listFiles() ?: return@guard).groupBy { it.name.substringBefore('.').toIntOrNull() ?: -1 }
        val old = byPid.filterKeys { it != currentPid }.entries
            .sortedByDescending { e -> e.value.maxOf { it.lastModified() } }
            .drop(keep)
        for (e in old) e.value.forEach { it.delete() }
    }
}

/** Platform-independent view of `android.app.ApplicationExitInfo` (testable on the JVM). */
internal data class ExitRecord(
    val pid: Int,
    val reason: Int,
    val status: Int,
    val importance: Int,
    val timestampMs: Long,
    val description: String?,
    val trace: () -> String? = { null },
)

/**
 * Maps previous-process exits (API 30+) to ERROR events, skipping what the SDK reported itself.
 * Reported: ANR, native crash, Java crash, low-memory kill and excessive resource use while the app was
 * visible, initialization failure. User-initiated / normal exits are ignored.
 */
internal class ExitInfoProcessor(
    private val store: KeyValueStore,
    private val records: ProcessRecords,
    private val core: RumCore,
) {
    /**
     * [exits]: newest first, as returned by `getHistoricalProcessExitReasons`. [nativeCrashPids]: processes
     * whose NDK report was already sent. The first run only records a watermark (no history is replayed).
     */
    fun process(exits: List<ExitRecord>, nativeCrashPids: Set<Int>): Int {
        val last = store.get(KEY_WATERMARK)?.toLongOrNull()
        val newest = exits.maxOfOrNull { it.timestampMs }
        if (newest != null && (last == null || newest > last)) store.put(KEY_WATERMARK, newest.toString())
        if (last == null) return 0
        var sent = 0
        for (e in exits.sortedBy { it.timestampMs }) {
            if (e.timestampMs <= last) continue
            val mapped = map(e, records.ownKinds(e.pid), nativeCrashPids) ?: continue
            core.previousProcessError(records.context(e.pid), e.timestampMs, mapped.first, mapped.second)
            sent++
        }
        return sent
    }

    fun map(e: ExitRecord, own: Set<String>, nativeCrashPids: Set<Int>): Pair<RumPayload.Error, Map<String, String>>? {
        val visible = e.importance in 1..IMPORTANCE_VISIBLE
        val (type, source) = when (e.reason) {
            REASON_ANR -> if ("anr" in own || "crash" in own) return null else "ANR" to "anr"
            REASON_CRASH_NATIVE -> if (e.pid in nativeCrashPids || "crash" in own) return null
                else (NativeCrashReport.SIGNALS[e.status] ?: "NativeCrash") to "crash"
            REASON_CRASH -> if ("crash" in own) return null else "JavaCrash" to "crash"
            REASON_LOW_MEMORY -> if (!visible) return null else "LowMemoryKill" to "crash"
            REASON_EXCESSIVE_RESOURCE_USAGE -> if (!visible) return null else "ExcessiveResourceUsage" to "crash"
            REASON_INITIALIZATION_FAILURE -> "InitializationFailure" to "crash"
            else -> return null
        }
        val reasonName = REASON_NAMES[e.reason] ?: e.reason.toString()
        val stack = if (e.reason == REASON_ANR) Internal.guard("exit.trace", null) { e.trace()?.let(::mainThreadSection) } else null
        val msg = buildString {
            append("Process exited: ").append(reasonName)
            if (!e.description.isNullOrBlank()) append(" (").append(e.description.take(512)).append(')')
        }
        val payload = RumPayload.Error(type = type, message = msg, stack = stack, source = source, isCrash = true,
            thread = if (e.reason == REASON_ANR) "main" else null)
        val attrs = mapOf(
            "exit_info" to "true",
            "exit_reason" to reasonName,
            "exit_status" to e.status.toString(),
            "exit_importance" to e.importance.toString(),
        )
        return payload to attrs
    }

    companion object {
        const val KEY_WATERMARK = "exit_info_watermark"
        // ApplicationExitInfo.REASON_* (stable public constants)
        const val REASON_SIGNALED = 2
        const val REASON_LOW_MEMORY = 3
        const val REASON_CRASH = 4
        const val REASON_CRASH_NATIVE = 5
        const val REASON_ANR = 6
        const val REASON_INITIALIZATION_FAILURE = 7
        const val REASON_EXCESSIVE_RESOURCE_USAGE = 9
        const val IMPORTANCE_VISIBLE = 200
        private val REASON_NAMES = mapOf(REASON_LOW_MEMORY to "low_memory", REASON_CRASH to "crash",
            REASON_CRASH_NATIVE to "crash_native", REASON_ANR to "anr", REASON_INITIALIZATION_FAILURE to "initialization_failure",
            REASON_EXCESSIVE_RESOURCE_USAGE to "excessive_resource_usage", REASON_SIGNALED to "signaled")
        private const val MAX_TRACE = 256 * 1024

        /** ANR trace dump → the `"main"` thread block (falls back to the head of the dump). */
        fun mainThreadSection(dump: String): String {
            val text = if (dump.length > MAX_TRACE) dump.substring(0, MAX_TRACE) else dump
            val start = text.indexOf("\n\"main\"").let { if (it >= 0) it + 1 else if (text.startsWith("\"main\"")) 0 else -1 }
            val section = if (start < 0) text else {
                val end = text.indexOf("\n\n", start)
                if (end < 0) text.substring(start) else text.substring(start, end)
            }
            return "ANR\n" + section.take(RumCore.MAX_STACK - 4)
        }
    }
}

/** Reads `ActivityManager.getHistoricalProcessExitReasons` (API 30+). */
@TargetApi(30)
internal object ExitInfoSource {
    fun read(context: Context, max: Int = 16): List<ExitRecord> = Internal.guard("exitInfo", emptyList()) {
        val am = context.getSystemService(Context.ACTIVITY_SERVICE) as? ActivityManager ?: return@guard emptyList()
        am.getHistoricalProcessExitReasons(context.packageName, 0, max).map { it.toRecord() }
    }

    private fun ApplicationExitInfo.toRecord() = ExitRecord(pid, reason, status, importance, timestamp, description) {
        traceInputStream?.use { s ->
            val bytes = s.readNBytesCompat(256 * 1024)
            String(bytes, Charsets.UTF_8)
        }
    }

    private fun java.io.InputStream.readNBytesCompat(max: Int): ByteArray {
        val out = java.io.ByteArrayOutputStream()
        val buf = ByteArray(8192)
        while (out.size() < max) {
            val n = read(buf, 0, minOf(buf.size, max - out.size()))
            if (n <= 0) break
            out.write(buf, 0, n)
        }
        return out.toByteArray()
    }
}
