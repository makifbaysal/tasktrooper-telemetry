package dev.tasktrooper.reactnative

import android.content.Context
import android.os.Build
import android.os.Process
import android.os.SystemClock
import java.lang.reflect.Method

/**
 * Hybrid bridge logic (contracts.md §9), shared by the New/Old Architecture module classes.
 *
 * The native SDK (`com.tasktrooper.android.TrooperHybridBridge`, trooper-android) is an optional runtime dependency:
 * it is looked up via reflection once, and every failure is swallowed (missing SDK = no-op / null).
 * Session replay (contracts.md §23) likewise goes to `com.tasktrooper.android.replay.TrooperReplay` (trooper-replay) when
 * the app includes it; its segments use the RUM session, which the JS layer owns via [setContext].
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

  /**
   * JS → native SDK. Non-empty string values and the boolean `sampled` (JS RUM sampling decision, contracts.md §9) are
   * forwarded in the map; `TrooperHybridBridge.setContext(Map<String, Any?>)` reads `sampled` once the native SDK supports it.
   */
  fun setContext(ctx: Map<String, Any?>) {
    try {
      val m = bridge()?.setContext ?: return
      val clean = ctx.filter { (k, v) -> (v is String && v.isNotEmpty()) || (k == "sampled" && v is Boolean) }
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

  // ------------------------------------------------------------------ session replay (trooper-replay, optional)

  private const val REPLAY_CLASS = "com.tasktrooper.android.replay.TrooperReplay"
  private const val REPLAY_OPTIONS_CLASS = "com.tasktrooper.android.replay.ReplayOptions"

  private class Replay(
    val install: Method,
    val defaults: Any,
    val options: Class<*>,
    val isRecording: Method?,
    val notifyError: Method?,
    val stop: Method?,
  )

  @Volatile private var replayResolved = false
  @Volatile private var replay: Replay? = null

  private fun replay(): Replay? {
    if (replayResolved) return replay
    synchronized(this) {
      if (!replayResolved) {
        replay = try {
          val cls = Class.forName(REPLAY_CLASS)
          val opts = Class.forName(REPLAY_OPTIONS_CLASS)
          fun method(name: String): Method? = try { cls.getMethod(name) } catch (_: Throwable) { null }
          Replay(
            cls.getMethod("install", Context::class.java, opts),
            opts.getConstructor().newInstance(),
            opts,
            method("isRecording"),
            method("notifyError"),
            method("stop"), // not in every trooper-replay version
          )
        } catch (_: Throwable) {
          null
        }
        replayResolved = true
      }
    }
    return replay
  }

  /**
   * Installs native replay with JS options (missing keys = native defaults). [context] should be the current Activity when
   * there is one (replay keeps the application context). False when trooper-replay is not in the app.
   */
  fun startReplay(context: Context, sampleRate: Double?, onErrorSampleRate: Double?, maskAllText: Boolean?): Boolean {
    return try {
      val r = replay() ?: return false
      val d = r.defaults
      fun get(name: String): Any? = r.options.getMethod(name).invoke(d)
      val options = r.options
        .getConstructor(Double::class.javaPrimitiveType, Double::class.javaPrimitiveType, Boolean::class.javaPrimitiveType,
          Boolean::class.javaPrimitiveType, Long::class.javaPrimitiveType)
        .newInstance(
          sampleRate ?: get("getSampleRate"),
          onErrorSampleRate ?: get("getOnErrorSampleRate"),
          maskAllText ?: get("getMaskAllText"),
          true,
          get("getMaxQueueBytes"),
        )
      r.install.invoke(null, context, options)
      true
    } catch (_: Throwable) {
      false
    }
  }

  /** False when replay is absent or this trooper-replay version cannot stop. */
  fun stopReplay(): Boolean {
    return try {
      val m = replay()?.stop ?: return false
      m.invoke(null)
      true
    } catch (_: Throwable) {
      false
    }
  }

  fun isReplayRecording(): Boolean =
    try { replay()?.isRecording?.invoke(null) as? Boolean ?: false } catch (_: Throwable) { false }

  /** A JS ERROR: flush the on-error buffer like a native ERROR (native error listeners never see JS errors). */
  fun notifyReplayError() {
    try {
      replay()?.notifyError?.invoke(null)
    } catch (_: Throwable) {
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
