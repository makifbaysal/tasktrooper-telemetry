package dev.tasktrooper.trooper_flutter

import android.os.Build
import android.os.Process
import android.os.SystemClock
import io.flutter.embedding.engine.plugins.FlutterPlugin
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.lang.reflect.Method

/**
 * Flutter side of the hybrid bridge (contracts.md §9), channel `dev.tasktrooper.telemetry/rum`.
 *
 * Forwards `setContext` / `getContext` / `getProcessStartTime` to the native TaskTrooper Android SDK
 * (`com.tasktrooper.android.TrooperHybridBridge`) when the host app includes it. The SDK is resolved by
 * reflection so this plugin has no compile-time dependency on it. `getProcessStartTime` works without it.
 * Every failure is swallowed: the plugin never crashes the host app.
 */
class TrooperFlutterPlugin : FlutterPlugin, MethodChannel.MethodCallHandler {
    private var channel: MethodChannel? = null

    override fun onAttachedToEngine(binding: FlutterPlugin.FlutterPluginBinding) {
        channel = MethodChannel(binding.binaryMessenger, CHANNEL).also { it.setMethodCallHandler(this) }
    }

    override fun onDetachedFromEngine(binding: FlutterPlugin.FlutterPluginBinding) {
        channel?.setMethodCallHandler(null)
        channel = null
    }

    override fun onMethodCall(call: MethodCall, result: MethodChannel.Result) {
        val value: Any? = try {
            when (call.method) {
                "setContext" -> {
                    setContext(call.arguments as? Map<*, *>)
                    null
                }
                "getContext" -> getContext()
                "getProcessStartTime" -> processStartTimeMs()
                else -> {
                    result.notImplemented()
                    return
                }
            }
        } catch (_: Throwable) {
            null
        }
        result.success(value)
    }

    private fun setContext(args: Map<*, *>?) {
        val method = NativeBridge.setContext ?: return
        if (args == null) return
        val map = HashMap<String, Any?>(args.size)
        for ((k, v) in args) if (k is String) map[k] = v
        method.invoke(null, map)
    }

    private fun getContext(): Map<String, String>? {
        val method = NativeBridge.getContext ?: return null
        val raw = method.invoke(null) as? Map<*, *> ?: return null
        val out = HashMap<String, String>(raw.size)
        for ((k, v) in raw) if (k is String && v is String) out[k] = v
        return out.takeIf { it.isNotEmpty() }
    }

    companion object {
        const val CHANNEL = "dev.tasktrooper.telemetry/rum"

        /** Process start as unix epoch ms: native SDK value if > 0, else computed here; null below API 24. */
        @JvmStatic
        fun processStartTimeMs(): Long? {
            try {
                val fromSdk = (NativeBridge.getProcessStartTime?.invoke(null) as? Number)?.toLong() ?: 0L
                if (fromSdk > 0) return fromSdk
            } catch (_: Throwable) {
            }
            return try {
                if (Build.VERSION.SDK_INT < Build.VERSION_CODES.N) return null
                val sinceStart = SystemClock.elapsedRealtime() - Process.getStartElapsedRealtime()
                (System.currentTimeMillis() - sinceStart).takeIf { it > 0 }
            } catch (_: Throwable) {
                null
            }
        }
    }

    /** Lazily resolved, cached reflection handles to `com.tasktrooper.android.TrooperHybridBridge`. */
    private object NativeBridge {
        private val clazz: Class<*>? by lazy {
            try {
                Class.forName("com.tasktrooper.android.TrooperHybridBridge")
            } catch (_: Throwable) {
                null
            }
        }

        val setContext: Method? by lazy { find("setContext", Map::class.java) }
        val getContext: Method? by lazy { find("getContext") }
        val getProcessStartTime: Method? by lazy { find("getProcessStartTime") }

        private fun find(name: String, vararg params: Class<*>): Method? = try {
            clazz?.getMethod(name, *params)?.takeIf { java.lang.reflect.Modifier.isStatic(it.modifiers) }
        } catch (_: Throwable) {
            null
        }
    }
}
