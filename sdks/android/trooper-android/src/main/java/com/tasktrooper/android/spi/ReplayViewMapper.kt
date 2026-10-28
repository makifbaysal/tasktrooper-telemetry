package com.tasktrooper.android.spi

import android.view.View

/**
 * SDK-internal SPI: maps a View whose content is not made of child Views (Jetpack Compose's
 * `AndroidComposeView`) into session replay wireframe nodes. Implemented by `trooper-compose`
 * ([COMPOSE]) and loaded reflectively by `trooper-replay`, so neither module depends on the other.
 * Not a public API: may change without notice.
 */
interface ReplayViewMapper {
    /**
     * Emits [view]'s content into [sink] (back to front). Returns false when [view] is not handled.
     * Called for every visible ViewGroup: recognize the host by type (not by class name — R8 renames it), cheaply.
     * Child Views (e.g. Compose `AndroidView` interop) are still traversed by the caller afterwards.
     * Runs on the main thread inside a time budget: keep it allocation-light.
     */
    fun map(view: View, sink: ReplayNodeSink): Boolean

    companion object {
        const val COMPOSE = "com.tasktrooper.android.compose.ComposeReplayMapper"
    }
}

/** Receives nodes in screen pixels; the replay module converts units, applies clip, alpha and masking. */
interface ReplayNodeSink {
    /**
     * @param kind contracts.md §23 node kind: view|text|image|button|input|switch|webview|list|icon.
     * @param text raw text (masked by the sink unless unmasked); inputs are always masked.
     * @param unmask true/false when the node (or an ancestor node) opts in/out; null inherits from the View.
     * @param bg / [fg] ARGB colors, null when unknown.
     * @param checked toggle state for `switch` nodes (null when unknown / not a toggle).
     */
    fun node(
        id: Int, left: Float, top: Float, right: Float, bottom: Float, kind: String,
        text: String? = null, input: Boolean = false, unmask: Boolean? = null,
        bg: Int? = null, fg: Int? = null, radiusPx: Float = 0f, checked: Boolean? = null,
    )

    /** False once the frame's node or time budget is spent: stop traversing. */
    val hasBudget: Boolean
}
