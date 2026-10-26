package com.tasktrooper.android.internal

import android.annotation.SuppressLint
import android.app.Application
import android.app.UiModeManager
import android.content.Context
import android.content.SharedPreferences
import android.content.res.Configuration
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.Build
import android.os.SystemClock
import com.tasktrooper.android.BuildConfig
import com.tasktrooper.android.RumResource
import com.tasktrooper.android.TrooperConfig
import java.util.Locale

internal class PrefsStore(context: Context) : KeyValueStore {
    private val prefs: SharedPreferences = context.getSharedPreferences("tasktrooper", Context.MODE_PRIVATE)
    override fun get(key: String): String? = prefs.getString(key, null)
    override fun put(key: String, value: String) { prefs.edit().putString(key, value).apply() }
}

internal object DeviceInfo {
    fun resource(app: Application, config: TrooperConfig): RumResource {
        val dm = app.resources.displayMetrics
        val cfg = app.resources.configuration
        val uiMode = Internal.guard("uiMode", Configuration.UI_MODE_TYPE_NORMAL) {
            (app.getSystemService(Context.UI_MODE_SERVICE) as? UiModeManager)?.currentModeType
                ?: Configuration.UI_MODE_TYPE_NORMAL
        }
        val type = when {
            uiMode == Configuration.UI_MODE_TYPE_TELEVISION -> "tv"
            cfg.smallestScreenWidthDp >= 600 -> "tablet"
            else -> "mobile"
        }
        return RumResource(
            app = config.app,
            version = config.version.ifEmpty { appVersion(app) },
            environment = config.environment,
            sdkVersion = BuildConfig.SDK_VERSION,
            deviceType = type,
            manufacturer = Build.MANUFACTURER ?: "",
            model = Build.MODEL ?: "",
            locale = Locale.getDefault().toString().replace('_', '-'),
            screenWidth = dm.widthPixels,
            screenHeight = dm.heightPixels,
            osVersion = Build.VERSION.RELEASE ?: "",
            attributes = config.resourceAttributes + ("os.api_level" to Build.VERSION.SDK_INT.toString()),
        )
    }

    @Suppress("DEPRECATION")
    private fun appVersion(app: Application): String = Internal.guard("appVersion", "") {
        val pi = app.packageManager.getPackageInfo(app.packageName, 0)
        val code = if (Build.VERSION.SDK_INT >= 28) pi.longVersionCode else pi.versionCode.toLong()
        "${pi.versionName ?: ""}+$code"
    }

    /** Process start in uptime ms (API 24+), else SDK init time. */
    fun processStartUptime(): Long =
        if (Build.VERSION.SDK_INT >= 24) android.os.Process.getStartUptimeMillis() else SystemClock.uptimeMillis()
}

/** Network type for `RumEvent.connection`, cached for 2 s. */
internal class ConnectionProvider(context: Context) {
    private val cm = context.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
    @Volatile private var cached: String? = null
    @Volatile private var cachedAt = 0L

    fun get(): String {
        val now = SystemClock.uptimeMillis()
        val c = cached
        if (c != null && now - cachedAt < 2_000) return c
        val v = Internal.guard("connection", "unknown") { compute() }
        cached = v; cachedAt = now
        return v
    }

    @SuppressLint("MissingPermission")
    @Suppress("DEPRECATION")
    private fun compute(): String {
        val cm = cm ?: return "unknown"
        if (Build.VERSION.SDK_INT >= 23) {
            val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return "offline"
            return when {
                caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "wifi"
                caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "cellular"
                else -> "unknown"
            }
        }
        val info = cm.activeNetworkInfo ?: return "offline"
        if (!info.isConnected) return "offline"
        return when (info.type) {
            ConnectivityManager.TYPE_WIFI -> "wifi"
            ConnectivityManager.TYPE_MOBILE -> "cellular"
            else -> "unknown"
        }
    }
}
