package com.tasktrooper.android.internal

import com.tasktrooper.android.RumPayload
import java.io.File

/** A report written by trooper-ndk's signal handler (format: trooper_crash.c header comment). */
internal data class NativeCrashReport(
    val pid: Int,
    val tid: Long,
    val thread: String,
    val signal: Int,
    val code: Int,
    val faultAddr: Long,
    val tsMs: Long,
    val abi: String,
    val context: CrashContext?,
    val frames: List<Frame>,
    val modules: Map<Int, Module>,
    val complete: Boolean,
) {
    data class Frame(val pc: Long, val relPc: Long, val module: Int)
    data class Module(val buildId: String?, val path: String)

    val signalName: String get() = SIGNALS[signal] ?: "SIG$signal"

    /**
     * Android tombstone-style text (what `ndk-stack` and services/symbolicator parse):
     * ```
     * signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000
     * backtrace:
     *       #00 pc 000000000001a2b4  /data/app/~~x/lib/arm64/libfoo.so (BuildId: 5f2d…)
     * ```
     * `pc` is the module-relative address (ELF vaddr). Frames after #00 are return addresses minus the
     * instruction-size adjustment libunwindstack applies (4 on arm64, 2 on arm, 1 on x86), so they point
     * into the call instruction.
     */
    fun stack(): String {
        val is64 = abi.contains("64")
        val width = if (is64) 16 else 8
        val sb = StringBuilder()
        sb.append("signal ").append(signal).append(" (").append(signalName).append("), code ").append(code)
        codeName()?.let { sb.append(" (").append(it).append(')') }
        sb.append(", fault addr 0x").append(hex(faultAddr, width)).append('\n')
        sb.append("abi: '").append(abi).append("'\n")
        sb.append("backtrace:\n")
        frames.forEachIndexed { i, f ->
            val m = modules[f.module]
            val adjust = if (i == 0) 0 else pcAdjustment()
            sb.append("      #").append(i.toString().padStart(2, '0')).append(" pc ")
            if (m == null) {
                sb.append(hex(f.pc - adjust, width)).append("  <unknown>\n")
            } else {
                sb.append(hex(f.relPc - adjust, width)).append("  ").append(m.path.ifEmpty { "<anonymous>" })
                if (!m.buildId.isNullOrEmpty()) sb.append(" (BuildId: ").append(m.buildId).append(')')
                sb.append('\n')
            }
        }
        return sb.toString()
    }

    fun toPayload(): RumPayload.Error = RumPayload.Error(
        type = signalName,
        message = buildString {
            append("Fatal signal ").append(signal).append(" (").append(signalName).append(')')
            codeName()?.let { append(", code ").append(code).append(" (").append(it).append(')') }
            append(", fault addr 0x").append(java.lang.Long.toHexString(faultAddr))
            append(" in tid ").append(tid).append(" (").append(thread).append(')')
        },
        stack = stack().let { if (it.length > RumCore.MAX_STACK) it.substring(0, RumCore.MAX_STACK) else it },
        source = "crash",
        isCrash = true,
        thread = thread.ifEmpty { null },
    )

    fun attributes(): Map<String, String> = buildMap {
        put("native", "true")
        put("abi", abi)
        if (!complete) put("report_truncated", "true")
    }

    private fun pcAdjustment(): Long = when {
        abi.startsWith("arm64") || abi.endsWith("arm64-v8a") -> 4
        abi.startsWith("armeabi") -> 2
        abi.contains("x86") -> 1
        else -> 0
    }

    private fun codeName(): String? = when (signal) {
        11 -> mapOf(1 to "SEGV_MAPERR", 2 to "SEGV_ACCERR", 3 to "SEGV_BNDERR", 4 to "SEGV_PKUERR", 8 to "SEGV_MTEAERR", 9 to "SEGV_MTESERR")[code]
        7 -> mapOf(1 to "BUS_ADRALN", 2 to "BUS_ADRERR", 3 to "BUS_OBJERR")[code]
        4 -> mapOf(1 to "ILL_ILLOPC", 2 to "ILL_ILLOPN", 3 to "ILL_ILLADR", 4 to "ILL_ILLTRP", 5 to "ILL_PRVOPC")[code]
        8 -> mapOf(1 to "FPE_INTDIV", 2 to "FPE_INTOVF", 3 to "FPE_FLTDIV", 4 to "FPE_FLTOVF")[code]
        5 -> mapOf(1 to "TRAP_BRKPT", 2 to "TRAP_TRACE")[code]
        else -> null
    } ?: when (code) {
        0 -> "SI_USER"
        -1 -> "SI_QUEUE"
        -6 -> "SI_TKILL"
        else -> null
    }

    companion object {
        val SIGNALS = mapOf(4 to "SIGILL", 5 to "SIGTRAP", 6 to "SIGABRT", 7 to "SIGBUS", 8 to "SIGFPE", 11 to "SIGSEGV",
            9 to "SIGKILL", 15 to "SIGTERM", 3 to "SIGQUIT")

        private fun hex(v: Long, width: Int): String {
            val h = java.lang.Long.toHexString(if (width == 8) v and 0xffffffffL else v)
            return if (h.length >= width) h else "0".repeat(width - h.length) + h
        }

        private fun parseHex(s: String): Long? = try {
            java.lang.Long.parseUnsignedLong(s.removePrefix("0x"), 16)
        } catch (_: NumberFormatException) { null }

        /** Returns null when the text is not a report (empty file = the process did not crash). */
        fun parse(text: String): NativeCrashReport? {
            val lines = text.split('\n')
            if (lines.isEmpty() || lines[0].trim() != "TTNDK 1") return null
            val kv = HashMap<String, String>()
            val frames = ArrayList<Frame>()
            val modules = HashMap<Int, Module>()
            var complete = false
            for (raw in lines.drop(1)) {
                if (raw.isEmpty()) continue
                val sp = raw.indexOf(' ')
                val key = if (sp < 0) raw else raw.substring(0, sp)
                val rest = if (sp < 0) "" else raw.substring(sp + 1)
                when (key) {
                    "end" -> complete = true
                    "frame" -> {
                        val p = rest.split(' ')
                        if (p.size >= 3) {
                            val pc = parseHex(p[0]) ?: continue
                            frames.add(Frame(pc, parseHex(p[1]) ?: pc, p[2].toIntOrNull() ?: -1))
                        }
                    }
                    "module" -> {
                        val p = rest.split(' ', limit = 3)
                        if (p.size >= 2) {
                            val idx = p[0].toIntOrNull() ?: continue
                            modules[idx] = Module(p[1].takeIf { it != "-" }, p.getOrElse(2) { "" })
                        }
                    }
                    else -> kv[key] = rest
                }
            }
            val signal = kv["signal"]?.toIntOrNull() ?: return null
            return NativeCrashReport(
                pid = kv["pid"]?.toIntOrNull() ?: 0,
                tid = kv["tid"]?.toLongOrNull() ?: 0,
                thread = kv["thread"].orEmpty(),
                signal = signal,
                code = kv["code"]?.toIntOrNull() ?: 0,
                faultAddr = kv["fault_addr"]?.let { parseHex(it) } ?: 0,
                tsMs = kv["ts_ms"]?.toLongOrNull() ?: 0,
                abi = kv["abi"].orEmpty(),
                context = CrashContext.decode(kv["ctx"]),
                frames = frames,
                modules = modules,
                complete = complete,
            )
        }
    }
}

/**
 * Native crash reports live in `<dir>/<pid>-<start ms>.ndk`: the handler of each process pre-opens its own
 * (empty) file, so collecting old reports never races with installing the handler.
 * On launch, reports of previous processes are converted to ERROR events and deleted.
 */
internal class NativeCrashStore(private val dir: File) {
    fun reportFile(pid: Int, startMs: Long): File = File(dir, "$pid-$startMs$EXT")

    /** Converts and deletes every report except [current]. Returns the pids that had crashed. */
    fun collect(current: File?, core: RumCore, clock: Clock): Set<Int> {
        val crashed = HashSet<Int>()
        val files = dir.listFiles { f -> f.name.endsWith(EXT) } ?: return crashed
        for (f in files.sortedBy { it.name }) {
            if (f.name == current?.name) continue
            Internal.guard("ndk.collect", Unit) {
                val text = if (f.length() in 1..MAX_REPORT_BYTES) f.readText() else ""
                val r = NativeCrashReport.parse(text)
                if (r != null) {
                    crashed.add(r.pid)
                    val ts = if (r.tsMs > 0) r.tsMs else f.lastModified().takeIf { it > 0 } ?: clock.nowMs()
                    core.previousProcessError(r.context, ts, r.toPayload(), r.attributes())
                }
            }
            f.delete()
        }
        return crashed
    }

    companion object {
        const val EXT = ".ndk"
        private const val MAX_REPORT_BYTES = 256L * 1024
    }
}
