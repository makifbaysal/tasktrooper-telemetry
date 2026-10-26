package com.tasktrooper.android.ndk

import com.tasktrooper.android.spi.NativeCrashBridge

/**
 * Loaded reflectively by `TaskTrooper.init` when this artifact is on the classpath (no setup needed).
 * Every call is a no-op when `libtrooper-ndk.so` cannot be loaded (e.g. unsupported ABI).
 */
class NdkCrashBridge : NativeCrashBridge {
    override fun install(reportPath: String): Boolean = loaded && safe(false) { nativeInstall(reportPath) }

    override fun setContext(context: String) { if (loaded) safe(Unit) { nativeSetContext(context) } }

    override fun refreshModules() { if (loaded) safe(Unit) { nativeRefreshModules() } }

    companion object {
        private val loaded: Boolean = try {
            System.loadLibrary("trooper-ndk"); true
        } catch (_: Throwable) { false }

        private inline fun <T> safe(fallback: T, block: () -> T): T = try { block() } catch (_: Throwable) { fallback }

        @JvmStatic private external fun nativeInstall(path: String): Boolean
        @JvmStatic private external fun nativeSetContext(context: String)
        @JvmStatic private external fun nativeRefreshModules(): Int
    }
}
