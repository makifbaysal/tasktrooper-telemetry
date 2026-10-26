package dev.tasktrooper.reactnative

import android.os.Build
import android.os.Process
import android.os.SystemClock
import java.lang.reflect.Method

/**
 * Hybrid bridge logic (contracts.md §9), shared by the New/Old Architecture module classes.
 *
 * The native SDK (`com.tasktrooper.android.TrooperHybridBridge`, trooper-android) is an optional runtime dependency:
 * it is looked up via reflection once, and every failure is swallowed (missing SDK = no-op / null).
 */
internal object TrooperRumImpl {
  const val NAME = "TrooperRum"
  private const val BRIDGE_CLASS = "com.tasktrooper.android.TrooperHybridBridge"

  private class Bridge(val setContext: Method?, val getContext: Method?, val getProcessStartTime: Method?)

  @Volatile private var resolved = false
  @Volatile private var bridge: Bridge? = null

  private fun bridge(): Bridge? {
    if (resolved) return bridge
    synchronized(this) {
      if (!resolved) {
        bridge = try {
          val cls = Class.forName(BRIDGE_CLASS)
          fun method(name: String, vararg params: Class<*>): Method? =
            try { cls.getMethod(name, *params) } catch (_: Throwable) { null }
          Bridge(
            method("setContext", Map::class.java),
            method("getContext"),
            method("getProcessStartTime"),
          )
        } catch (_: Throwable) {
          null
        }
        resolved = true
      }
    }
    return bridge
  }

  /** JS → native SDK. Only non-empty string values are forwarded. */
  fun setContext(ctx: Map<String, Any?>) {
    try {
      val m = bridge()?.setContext ?: return
      val clean = ctx.filter { (_, v) -> v is String && v.isNotEmpty() }
      m.invoke(null, clean)
    } catch (_: Throwable) {
    }
  }

  /** Native SDK context, or null without the SDK / before it started. */
  fun getContext(): Map<String, String>? {
    return try {
      val m = bridge()?.getContext ?: return null
      @Suppress("UNCHECKED_CAST")
      val map = m.invoke(null) as? Map<String, Any?> ?: return null
      val out = LinkedHashMap<String, String>()
      for ((k, v) in map) if (v is String && v.isNotEmpty()) out[k] = v
      out.takeIf { it.isNotEmpty() }
    } catch (_: Throwable) {
      null
    }
  }

  /** OS process start as unix epoch ms: native SDK value if > 0, else Process.getStartElapsedRealtime (API 24+). */
  fun getProcessStartTime(): Long? {
    try {
      val v = (bridge()?.getProcessStartTime?.invoke(null) as? Number)?.toLong()
      if (v != null && v > 0) return v
    } catch (_: Throwable) {
    }
    return try {
      if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
        System.currentTimeMillis() - (SystemClock.elapsedRealtime() - Process.getStartElapsedRealtime())
      } else {
        null
      }
    } catch (_: Throwable) {
      null
    }
  }
}
