package com.tasktrooper.android.replay

/**
 * Mobile session replay options (contracts.md §23, ADR-021). Wireframes only: no screenshots or pixels.
 *
 * @param sampleRate fraction of (RUM-sampled) sessions recorded from the start. Decided once per session.
 * @param onErrorSampleRate fraction of the remaining sessions that keep the last 60 s in memory and start
 *   uploading (and keep recording) when an ERROR / ANR / crash is recorded.
 * @param maskAllText mask all text (length preserved as `•`). Inputs and password fields are always masked;
 *   opt subtrees out with `view.trooperUnmask()` / Compose `Modifier.trooperUnmask()`.
 * @param enabled false turns replay off entirely.
 * @param maxQueueBytes offline cap for unsent segments; newer segments are dropped beyond it.
 */
data class ReplayOptions @JvmOverloads constructor(
    val sampleRate: Double = 0.1,
    val onErrorSampleRate: Double = 1.0,
    val maskAllText: Boolean = true,
    val enabled: Boolean = true,
    val maxQueueBytes: Long = 2L * 1024 * 1024,
)
