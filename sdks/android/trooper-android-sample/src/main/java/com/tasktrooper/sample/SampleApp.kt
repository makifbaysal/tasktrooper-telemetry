package com.tasktrooper.sample

import android.app.Application
import com.tasktrooper.android.TaskTrooper
import com.tasktrooper.android.TrooperConfig
import com.tasktrooper.android.replay.ReplayOptions
import com.tasktrooper.android.replay.TrooperReplay

class SampleApp : Application() {
    override fun onCreate() {
        super.onCreate()
        TaskTrooper.init(
            this,
            TrooperConfig(
                // 10.0.2.2 = host machine from the emulator (local docker-compose ingest-gw).
                dsn = "http://tt_dev_ingest_key@10.0.2.2:4318/1",
                app = "sample-android",
                version = BuildConfig.VERSION_NAME,
                environment = "development",
                tracePropagationTargets = listOf(Regex("""^https://api\.example\.com""")),
                debug = true,
            ),
        )
        // Wireframe session replay (no screenshots): record every session in the sample; text is masked
        // except where `Modifier.trooperUnmask()` / `view.trooperUnmask()` opts in.
        TrooperReplay.install(this, ReplayOptions(sampleRate = 1.0, onErrorSampleRate = 1.0))
    }
}
