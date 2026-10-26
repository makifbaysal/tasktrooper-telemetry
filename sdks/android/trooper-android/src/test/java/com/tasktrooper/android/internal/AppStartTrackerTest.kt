package com.tasktrooper.android.internal

import org.junit.Assert.assertEquals
import org.junit.Test

class AppStartTrackerTest {
    private val starts = ArrayList<Pair<String, Double>>()
    private fun tracker(processStart: Long) = AppStartTracker(processStart) { k, d, _ -> starts.add(k to d) }

    @Test fun coldStartMeasuredFromProcessStart() {
        val t = tracker(1_000)
        t.onActivityCreated(1_300); t.onActivityStarted(1_350); t.onFirstFrame(1_800)
        t.onFirstFrame(2_000) // later frames ignored
        assertEquals(listOf("cold" to 800.0), starts)
    }

    @Test fun backgroundProcessThenActivityIsWarm() {
        val t = tracker(1_000)
        t.onActivityCreated(60_000); t.onActivityStarted(60_010); t.onFirstFrame(60_250)
        assertEquals(listOf("warm" to 250.0), starts)
    }

    @Test fun recreateFromBackgroundIsWarmAndResumeIsHot() {
        val t = tracker(0)
        t.onActivityCreated(100); t.onActivityStarted(110); t.onFirstFrame(400)
        t.onActivityStopped() // app in background
        t.onActivityStarted(10_000); t.onFirstFrame(10_120) // hot: no onCreate
        t.onActivityStopped()
        t.onActivityCreated(20_000); t.onActivityStarted(20_010); t.onFirstFrame(20_300) // warm
        assertEquals(listOf("cold" to 400.0, "hot" to 120.0, "warm" to 300.0), starts)
    }

    @Test fun navigatingBetweenActivitiesIsNotAStart() {
        val t = tracker(0)
        t.onActivityCreated(100); t.onActivityStarted(110); t.onFirstFrame(400)
        t.onActivityCreated(1_000); t.onActivityStarted(1_010); t.onFirstFrame(1_200); t.onActivityStopped()
        assertEquals(1, starts.size)
    }
}
