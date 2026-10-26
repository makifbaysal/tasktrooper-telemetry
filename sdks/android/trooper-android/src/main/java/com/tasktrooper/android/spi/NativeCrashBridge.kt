package com.tasktrooper.android.spi

/**
 * SDK-internal SPI implemented by `trooper-ndk` (`com.tasktrooper.android.ndk.NdkCrashBridge`), loaded
 * reflectively so `trooper-android` has no native code. Not a public API: may change without notice.
 */
interface NativeCrashBridge {
    /** Installs the signal handlers; the report of this process will be written to [reportPath]. */
    fun install(reportPath: String): Boolean

    /** Opaque one-line context copied into the report (session / view / user ids). */
    fun setContext(context: String)

    /** Re-snapshots loaded native libraries (path, address range, build-id). */
    fun refreshModules()

    companion object {
        const val IMPLEMENTATION = "com.tasktrooper.android.ndk.NdkCrashBridge"
    }
}
