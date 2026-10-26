package com.tasktrooper.android.internal

/**
 * Session/view/user of a process, captured so errors reported by a *later* process (native crash
 * reports, ApplicationExitInfo) are attributed to the session and screen where they happened.
 * Encoded as one tab-separated line (it is copied verbatim into native crash reports).
 */
internal data class CrashContext(
    val sessionId: String,
    val anonymousId: String,
    val viewId: String?,
    val viewName: String?,
    val userId: String?,
    val sampled: Boolean,
) {
    fun encode(): String = listOf(VERSION, sessionId, anonymousId, viewId.orEmpty(), viewName.orEmpty(), userId.orEmpty(),
        if (sampled) "1" else "0").joinToString("\t") { clean(it) }

    companion object {
        private const val VERSION = "c1"

        private fun clean(s: String) = s.replace('\t', ' ').replace('\n', ' ').replace('\r', ' ')

        fun decode(line: String?): CrashContext? {
            if (line.isNullOrEmpty()) return null
            val p = line.split('\t')
            if (p.size < 7 || p[0] != VERSION || p[1].isEmpty()) return null
            return CrashContext(p[1], p[2], p[3].ifEmpty { null }, p[4].ifEmpty { null }, p[5].ifEmpty { null }, p[6] == "1")
        }
    }
}
