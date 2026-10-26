package com.tasktrooper.android

import android.app.Application
import android.os.Handler
import android.os.Looper
import com.tasktrooper.android.internal.ActivityTracker
import com.tasktrooper.android.internal.AnrDetector
import com.tasktrooper.android.internal.AnrWatchdogThread
import com.tasktrooper.android.internal.ConnectionProvider
import com.tasktrooper.android.internal.CrashHandler
import com.tasktrooper.android.internal.DeviceInfo
import com.tasktrooper.android.internal.DiskQueue
import com.tasktrooper.android.internal.ExecutorScheduler
import com.tasktrooper.android.internal.HttpTransport
import com.tasktrooper.android.internal.Internal
import com.tasktrooper.android.internal.Pipeline
import com.tasktrooper.android.internal.PrefsStore
import com.tasktrooper.android.internal.ProcessFlushObserver
import com.tasktrooper.android.internal.RumCore
import com.tasktrooper.android.internal.SessionManager
import com.tasktrooper.android.internal.SystemClockImpl
import com.tasktrooper.android.internal.Uploader
import com.tasktrooper.android.internal.ExitInfoProcessor
import com.tasktrooper.android.internal.ExitInfoSource
import com.tasktrooper.android.internal.NativeCrashStore
import com.tasktrooper.android.internal.ProcessRecords
import com.tasktrooper.android.spi.NativeCrashBridge
import android.os.Build
import android.os.Process
import java.io.File

/**
 * TaskTrooper Android RUM SDK entry point. Every method is safe to call from any thread, before
 * [init] (no-op) and never throws.
 */
object TaskTrooper {
    @Volatile internal var core: RumCore? = null
        private set
    @Volatile private var targets: List<Regex> = emptyList()
    @Volatile private var ingestHost: String? = null
    @Volatile private var app: String? = null

    /** Call once from `Application.onCreate()`. */
    @JvmStatic
    fun init(application: Application, config: TrooperConfig) {
        Internal.guard("init", Unit) {
            synchronized(this) {
                if (core != null) return@guard
                Internal.debug = config.debug
                val dsn = Dsn.parse(config.dsn)
                if (dsn == null) {
                    Internal.debug("invalid DSN, SDK disabled")
                    return@guard
                }
                val clock = SystemClockImpl
                val resource = DeviceInfo.resource(application, config)
                val queue = DiskQueue(File(application.noBackupFilesDir, "tasktrooper/queue"), config.maxQueueBytes)
                val uploader = Uploader(queue, HttpTransport(dsn.endpoint, dsn.key), ExecutorScheduler("trooper-upload"))
                val worker = ExecutorScheduler("trooper-worker")
                val pipeline = Pipeline(resource, queue, uploader, worker,
                    config.maxBatchSize.coerceIn(1, 100), config.flushIntervalMs)
                val connection = ConnectionProvider(application)
                val prefs = PrefsStore(application)
                val sessions = SessionManager(clock, prefs, config.sampleRate)
                val c = RumCore(config, clock, sessions, pipeline, worker) { connection.get() }
                val pid = Process.myPid()
                val records = ProcessRecords(File(application.noBackupFilesDir, "tasktrooper/proc"))
                targets = config.tracePropagationTargets
                app = config.app
                ingestHost = Internal.guard("host", null) { java.net.URL(dsn.endpoint).host }
                core = c

                if (config.trackCrashes) {
                    Thread.setDefaultUncaughtExceptionHandler(CrashHandler(c, Thread.getDefaultUncaughtExceptionHandler()) {
                        records.markOwn(pid, "crash")
                    })
                }
                // Native crashes: trooper-ndk (optional artifact) installs signal handlers writing to a per-process file.
                val ndkStore = NativeCrashStore(File(application.noBackupFilesDir, "tasktrooper/ndk"))
                var reportFile: File? = null
                val native: NativeCrashBridge? = if (config.trackCrashes && config.trackNativeCrashes) {
                    Internal.guard("ndk", null) {
                        val bridge = Class.forName(NativeCrashBridge.IMPLEMENTATION).getDeclaredConstructor().newInstance() as NativeCrashBridge
                        File(application.noBackupFilesDir, "tasktrooper/ndk").mkdirs()
                        val f = ndkStore.reportFile(pid, clock.nowMs())
                        if (bridge.install(f.path)) { reportFile = f; bridge } else null
                    }
                } else null
                var lastModuleRefresh = 0L
                c.contextListener = { ctx ->
                    native?.setContext(ctx.encode())
                    worker.execute {
                        records.writeContext(pid, ctx)
                        val now = clock.uptimeMs()
                        if (native != null && now - lastModuleRefresh > 10_000) { lastModuleRefresh = now; native.refreshModules() }
                    }
                }
                application.registerActivityLifecycleCallbacks(ActivityTracker(c, config, DeviceInfo.processStartUptime()))
                ProcessFlushObserver { pipeline.flushAsync() }.register()
                if (config.trackAnr) {
                    val main = Handler(Looper.getMainLooper())
                    val detector = AnrDetector(config.anrThresholdMs, { main.post(it) },
                        { Looper.getMainLooper().thread.stackTrace }, { c.anr(it); records.markOwn(pid, "anr") })
                    AnrWatchdogThread(detector, clock).start()
                }
                // Previous processes: native crash reports, then exits the SDK did not see (deduped against both).
                val currentReport = reportFile
                worker.execute {
                    val nativePids = Internal.guard("ndk.collect", emptySet()) { ndkStore.collect(currentReport, c, clock) }
                    if (config.trackExitReasons && Build.VERSION.SDK_INT >= 30) {
                        Internal.guard("exitInfo", Unit) {
                            ExitInfoProcessor(prefs, records, c).process(ExitInfoSource.read(application), nativePids)
                        }
                    }
                    records.prune(pid)
                }
                // Send what previous runs left behind (offline batches, persisted crash).
                uploader.drain()
            }
        }
    }

    @JvmStatic val isInitialized: Boolean get() = core != null

    /** Attach a user id (and opt-in traits) to subsequent events. */
    @JvmStatic @JvmOverloads
    fun identify(id: String, traits: Map<String, String> = emptyMap()) {
        core?.user = RumUser(id, traits)
    }

    @JvmStatic fun clearUser() { core?.user = null }

    /** Custom event (`CUSTOM`). */
    @JvmStatic @JvmOverloads
    fun track(name: String, props: Map<String, String> = emptyMap(), value: Double = 0.0) {
        Internal.guard("track", Unit) { core?.track(name, props, value) }
    }

    /** Handled error (`ERROR`, source=source). */
    @JvmStatic @JvmOverloads
    fun captureError(throwable: Throwable, attributes: Map<String, String> = emptyMap()) {
        Internal.guard("captureError", Unit) { core?.error(throwable, "source", attributes) }
    }

    /** Manual view (screen) tracking, e.g. for custom navigation. Starting a view ends the previous one. */
    @JvmStatic fun startView(name: String) { Internal.guard("startView", Unit) { core?.startView(name, name) } }

    /** Ends the current view; with [name], only if it is the current view. */
    @JvmStatic @JvmOverloads
    fun stopView(name: String? = null) { Internal.guard("stopView", Unit) { core?.stopView(name) } }

    /** User action (`ACTION`), e.g. a tap. Rage taps (≥3 in 1 s on the same target) are flagged. */
    @JvmStatic @JvmOverloads
    fun addAction(name: String, target: String? = null) { Internal.guard("action", Unit) { core?.action(name, target) } }

    /** Network request (`RESOURCE`). Used by trooper-okhttp; available for custom HTTP clients. */
    @JvmStatic
    fun recordResource(resource: RumPayload.Resource) { Internal.guard("resource", Unit) { core?.resource(resource) } }

    /** True when a `traceparent` header should be added to a request to [url]. */
    @JvmStatic
    fun shouldPropagateTrace(url: String): Boolean = Internal.guard("propagate", false) {
        core != null && targets.any { it.containsMatchIn(url) }
    }

    /**
     * App name sent as W3C `baggage: tt.svc=<app>` next to `traceparent` (backends read it into
     * `trooper.caller`). Null before [init].
     */
    @JvmStatic
    fun baggageServiceName(): String? = if (core != null) app else null

    /** True for requests to the TaskTrooper ingest host (never instrumented). */
    @JvmStatic
    fun isIngestUrl(host: String): Boolean = host == ingestHost

    /**
     * Marks the current screen as fully drawn: `VITAL` `TTFD` (ms since the screen started), once per view.
     * Activities extending androidx `ComponentActivity` are reported automatically when the app calls
     * `reportFullyDrawn()` (or uses `ReportDrawn*` composables); call this for other screens.
     */
    @JvmStatic fun reportFullyDrawn() { Internal.guard("fullyDrawn", Unit) { core?.fullyDrawn() } }

    /**
     * Tells dead-tap detection that the UI changed in a way the SDK cannot observe (e.g. a Compose-only
     * state change without a View layout pass). Taps followed by activity within 1 s are not `dead`.
     */
    @JvmStatic fun notifyActivity() { core?.noteActivity() }

    /** Persist buffered events and attempt an upload now. */
    @JvmStatic fun flush() { Internal.guard("flush", Unit) { core?.pipeline?.flushAsync() } }

    @JvmStatic val anonymousId: String? get() = core?.sessions?.anonymousId
    @JvmStatic val sessionId: String? get() = core?.sessions?.peek()?.id

    /** Test hook: installs a pre-built core. */
    internal fun installForTest(c: RumCore?, propagationTargets: List<Regex> = emptyList(), appName: String? = null) {
        core = c; targets = propagationTargets; app = appName
    }
}
