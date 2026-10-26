package com.tasktrooper.android

import android.os.SystemClock
import com.tasktrooper.android.internal.DeviceInfo
import com.tasktrooper.android.internal.Internal

/**
 * Shared context for hybrid apps (Flutter / React Native on top of this SDK), contracts.md §9.
 * Callers forward their calls here, so native crashes and JS/Dart events share one session:
 * - Flutter: the `trooper_flutter` plugin's MethodChannel [CHANNEL] (`dev.tasktrooper.telemetry/rum`);
 * - React Native: the native module named [RN_MODULE] (`TrooperRum`; the JS side falls back to a module
 *   named [CHANNEL]).
 * Map keys: session_id, anonymous_id, view_id, view_name, user_id.
 *
 * This module has no Flutter/RN dependency; the MethodChannel / NativeModule wrapper lives in those SDKs.
 */
object TrooperHybridBridge {
    const val CHANNEL = "dev.tasktrooper.telemetry/rum"

    /** React Native native module name. */
    const val RN_MODULE = "TrooperRum"

    /** JS/Dart → native. Missing or empty keys are ignored. */
    @JvmStatic
    fun setContext(context: Map<String, Any?>) = Internal.guard("bridge.setContext", Unit) {
        val core = TaskTrooper.core ?: return@guard
        fun str(k: String) = (context[k] as? String)?.takeIf { it.isNotEmpty() }
        str("anonymous_id")?.let { core.sessions.adoptAnonymousId(it) }
        str("session_id")?.let { core.sessions.adopt(it) }
        str("user_id")?.let { if (core.user?.id != it) core.user = RumUser(it) }
        val viewId = str("view_id"); val viewName = str("view_name")
        if (viewId != null && viewName != null) core.adoptView(viewId, viewName)
    }

    /** Native → JS/Dart. Empty map before init. Starts a session if none is active. */
    @JvmStatic
    fun getContext(): Map<String, String> = Internal.guard("bridge.getContext", emptyMap()) {
        val core = TaskTrooper.core ?: return@guard emptyMap()
        val session = core.sessions.touch(extend = false).first
        buildMap {
            put("session_id", session.id)
            put("anonymous_id", core.sessions.anonymousId)
            core.currentViewId?.let { put("view_id", it) }
            core.currentViewName?.let { put("view_name", it) }
            core.user?.id?.let { put("user_id", it) }
        }
    }

    /** OS process start as unix epoch ms (API 24+; SDK init time before), for APP_START start_reference=process. */
    @JvmStatic
    fun getProcessStartTime(): Long = Internal.guard("bridge.processStart", 0L) {
        System.currentTimeMillis() - (SystemClock.uptimeMillis() - DeviceInfo.processStartUptime())
    }
}
