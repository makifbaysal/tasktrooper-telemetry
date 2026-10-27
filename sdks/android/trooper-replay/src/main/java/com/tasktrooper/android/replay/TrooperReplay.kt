package com.tasktrooper.android.replay

import android.app.Activity
import android.app.Application
import android.content.Context
import android.os.Handler
import android.os.Looper
import com.tasktrooper.android.replay.internal.CpuClock
import com.tasktrooper.android.replay.internal.ExecutorReplayScheduler
import com.tasktrooper.android.replay.internal.Guard
import com.tasktrooper.android.replay.internal.HttpSegmentTransport
import com.tasktrooper.android.replay.internal.Recorder
import com.tasktrooper.android.replay.internal.ReplayEngine
import com.tasktrooper.android.replay.internal.ReplayStore
import com.tasktrooper.android.replay.internal.SegmentStore
import com.tasktrooper.android.replay.internal.SegmentUploader
import com.tasktrooper.android.replay.internal.ViewMapper
import com.tasktrooper.android.spi.ReplayViewMapper
import com.tasktrooper.android.spi.TrooperSpi
import java.io.File

/**
 * Mobile session replay (wireframe). Install after `TaskTrooper.init`, from `Application.onCreate()`:
 *
 * ```kotlin
 * TaskTrooper.init(this, TrooperConfig(dsn = "...", app = "shop-android"))
 * TrooperReplay.install(this, ReplayOptions(sampleRate = 0.1, onErrorSampleRate = 1.0))
 * ```
 *
 * Segments share the RUM `session_id` and are sent to `POST /v1/replay`. Never throws.
 */
object TrooperReplay {
    private class Installed(
        val engine: ReplayEngine, val recorder: Recorder, val uploader: SegmentUploader, val worker: ExecutorReplayScheduler,
        val errorListener: TrooperSpi.ErrorListener,
    )

    @Volatile private var installed: Installed? = null

    @JvmStatic @JvmOverloads
    fun install(context: Context, options: ReplayOptions = ReplayOptions()) = Guard.run("replay.install", Unit) {
        synchronized(this) {
            if (installed != null || !options.enabled) return@run
            val app = context.applicationContext as? Application ?: return@run
            if (!TrooperSpi.isInitialized) TrooperSpi.debug("replay: TaskTrooper.init has not been called yet; recording starts once it is")
            val worker = ExecutorReplayScheduler("trooper-replay")
            val store = SegmentStore(File(app.noBackupFilesDir, "tasktrooper/replay"), options.maxQueueBytes)
            val uploader = SegmentUploader(store, HttpSegmentTransport { TrooperSpi.ingest() }, worker)
            val prefs = app.getSharedPreferences("tasktrooper_replay", Context.MODE_PRIVATE)
            val kv = object : ReplayStore {
                override fun get(key: String): String? = prefs.getString(key, null)
                override fun put(values: Map<String, String>, sync: Boolean) {
                    val e = prefs.edit()
                    for ((k, v) in values) e.putString(k, v)
                    if (sync) e.commit() else e.apply()
                }
            }
            val sdkVersion = BuildConfig.SDK_VERSION
            val engine = ReplayEngine(
                options, { System.currentTimeMillis() }, kv,
                { gz, sync -> if (store.write(gz) && !sync) uploader.drain() },
                { TrooperSpi.context() },
                { TrooperSpi.resource()?.let { ReplayEngine.ResourceInfo(it.app, it.environment, it.version, sdkVersion) } },
            )
            val compose = Guard.run("replay.compose", null) {
                Class.forName(ReplayViewMapper.COMPOSE).getDeclaredConstructor().newInstance() as ReplayViewMapper
            }
            val recorder = Recorder(app, engine, worker, ViewMapper(options.maskAllText, compose, nanoTime = CpuClock::nanos)) {
                worker.execute { engine.flush(); uploader.drain() }
            }
            var listening = false
            val errorListener = TrooperSpi.ErrorListener { ts, isCrash ->
                // Crash: persist synchronously on the crashing thread; uploaded on next launch.
                if (isCrash) engine.onError(ts, true) else worker.execute { engine.onError(ts, false) }
            }
            installed = Installed(engine, recorder, uploader, worker, errorListener)
            fun tick() {
                if (engine.stopped) return
                if (!listening && TrooperSpi.isInitialized) { TrooperSpi.addErrorListener(errorListener); listening = true }
                engine.tick()
                worker.schedule(1_000) { tick() }
            }
            worker.execute {
                tick()
                engine.refreshSession()
                uploader.drain() // segments left by previous runs (offline, crash)
            }
            // Synchronously when installed from Application.onCreate: the first activity's onResume must be seen
            // (window callback wrapped for touches). Later windows are also picked up while capturing.
            // An Activity passed as context (late install from React Native) is already resumed: seed it.
            val resumed = context as? Activity
            if (Looper.myLooper() == Looper.getMainLooper()) Guard.run("replay.start", Unit) { recorder.start(resumed) }
            else Handler(Looper.getMainLooper()).post { Guard.run("replay.start", Unit) { recorder.start(resumed) } }
        }
    }

    /**
     * Stops replay: detaches capture and touch hooks, seals and uploads the open segment. Idempotent; [isRecording]
     * is false afterwards and [install] can start it again (same session: segment numbering continues).
     */
    @JvmStatic
    fun stop() = Guard.run("replay.stop", Unit) {
        val i = synchronized(this) { installed.also { installed = null } } ?: return@run
        Guard.run("replay.stopListener", Unit) { TrooperSpi.removeErrorListener(i.errorListener) }
        if (Looper.myLooper() == Looper.getMainLooper()) Guard.run("replay.stopRecorder", Unit) { i.recorder.stop() }
        else Handler(Looper.getMainLooper()).post { Guard.run("replay.stopRecorder", Unit) { i.recorder.stop() } }
        i.worker.execute { i.engine.stop() }
        i.uploader.drain()
        i.worker.shutdown()
    }

    /** True while the current session is being recorded (not just buffered for errors). */
    @JvmStatic val isRecording: Boolean get() = installed?.engine?.mode == ReplayEngine.Mode.RECORD

    /**
     * An ERROR recorded outside this SDK (hybrid apps: a React Native JS error) — flushes the on-error buffer and keeps
     * recording, like a native ERROR. No-op before [install] or when the session is not captured.
     */
    @JvmStatic
    fun notifyError() {
        val i = installed ?: return
        val ts = System.currentTimeMillis()
        i.worker.execute { i.engine.onError(ts, false) }
    }

    /** Seals the current segment and uploads pending segments (also done automatically in background). */
    @JvmStatic
    fun flush() {
        val i = installed ?: return
        i.worker.execute { i.engine.flush(); i.uploader.drain() }
    }
}
