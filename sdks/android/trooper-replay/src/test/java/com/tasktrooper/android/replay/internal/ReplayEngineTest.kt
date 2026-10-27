package com.tasktrooper.android.replay.internal

import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.tasktrooper.android.replay.ReplayOptions
import com.tasktrooper.android.spi.TrooperSpi
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Random
import java.util.zip.GZIPInputStream

class ReplayEngineTest {
    private class MemStore : ReplayStore {
        val map = HashMap<String, String>()
        var syncWrites = 0
        override fun get(key: String) = map[key]
        override fun put(values: Map<String, String>, sync: Boolean) { map.putAll(values); if (sync) syncWrites++ }
    }

    /** Random whose nextDouble() returns the given values in order (then repeats the last). */
    private class SeqRandom(vararg val values: Double) : Random() {
        private var i = 0
        override fun nextDouble(): Double = values[minOf(i++, values.size - 1)]
    }

    private class Harness(
        options: ReplayOptions = ReplayOptions(sampleRate = 1.0),
        random: Random = Random(1),
        val store: MemStore = MemStore(),
        sampled: Boolean = true,
    ) {
        var now = 1_700_000_000_000L
        var ctx = TrooperSpi.Context("s1", sampled, "anon-1", "user-7", "v1", "Checkout")
        val segments = ArrayList<JsonObject>()
        val syncFlags = ArrayList<Boolean>()
        val engine = ReplayEngine(
            options, { now }, store,
            { gz, sync -> segments.add(parse(gz)); syncFlags.add(sync) },
            { ctx },
            { ReplayEngine.ResourceInfo("shop-android", "production", "1.4.0", "0.1.0") },
            random,
        )

        fun frame(vararg nodes: WireNode, keyboard: Boolean? = null) = engine.onFrame(now, 390, 844, 3f, nodes.toList(), keyboard)
        fun advance(ms: Long) { now += ms; engine.tick() }
    }

    companion object {
        fun parse(gz: ByteArray): JsonObject =
            JsonParser.parseString(GZIPInputStream(gz.inputStream()).use { it.readBytes().toString(Charsets.UTF_8) }).asJsonObject

        internal fun node(id: Int, text: String? = null) = WireNode(id, 0f, 0f, 100f, 40f, "text", text = text)
        fun types(seg: JsonObject) = seg.getAsJsonArray("events").map { it.asJsonObject["type"].asString }
    }

    @Test fun segmentPayloadMatchesContract() {
        val h = Harness()
        h.frame(node(1, "••••"), keyboard = false)
        h.engine.onTouch(h.now + 100, 10f, 20f, "down")
        h.engine.onTouch(h.now + 150, 10f, 20f, "up")
        h.engine.flush()

        assertEquals(1, h.segments.size)
        val s = h.segments[0]
        val res = s.getAsJsonObject("resource")
        assertEquals("shop-android", res["service"].asString)
        assertEquals("production", res["env"].asString)
        assertEquals("1.4.0", res["version"].asString)
        assertEquals("android", res["platform"].asString)
        assertEquals(ReplayEngine.SDK_NAME, res["sdk_name"].asString)
        assertEquals("s1", s["session_id"].asString)
        assertEquals(0, s["segment"].asInt)
        assertEquals("wireframe", s["format"].asString)
        assertTrue(s["has_full_snapshot"].asBoolean)
        assertEquals(h.now, s["start_ts"].asLong)
        assertEquals(h.now + 150, s["end_ts"].asLong)
        assertEquals("v1", s["view_id"].asString)
        assertEquals("Checkout", s["view_name"].asString)
        assertEquals("user-7", s["user_id"].asString)
        assertEquals("anon-1", s["anonymous_id"].asString)
        assertEquals(1, s.getAsJsonObject("stats")["clicks"].asInt)
        assertEquals(listOf("screen", "keyboard", "frame", "touch", "touch"), types(s))

        val ev = s.getAsJsonArray("events")
        val screen = ev[0].asJsonObject
        assertEquals(390, screen["width"].asInt); assertEquals(844, screen["height"].asInt)
        assertEquals(3.0, screen["density"].asDouble, 0.0); assertEquals("Checkout", screen["name"].asString)
        val frameNode = ev[2].asJsonObject.getAsJsonArray("nodes")[0].asJsonObject
        assertEquals(1, frameNode["id"].asInt)
        assertEquals("text", frameNode["kind"].asString)
        assertEquals(100.0, frameNode["w"].asDouble, 0.0)
        val touch = ev[3].asJsonObject
        assertEquals("down", touch["phase"].asString); assertEquals(10.0, touch["x"].asDouble, 0.0)
    }

    @Test fun unchangedFramesAreNotRecorded() {
        val h = Harness()
        h.frame(node(1, "a")); h.now += 500
        h.frame(node(1, "a")); h.now += 500
        h.frame(node(1, "b"))
        h.engine.flush()
        assertEquals(listOf("screen", "frame", "frame"), types(h.segments[0]))
    }

    @Test fun segmentsAreSealedAfterFiveSecondsAndCounted() {
        val h = Harness()
        h.frame(node(1, "a"))
        h.advance(4_000)
        assertTrue(h.segments.isEmpty())
        h.engine.onTouch(h.now, 1f, 1f, "down")
        h.advance(1_000)
        assertEquals(1, h.segments.size)
        h.frame(node(1, "b"))
        h.engine.flush()
        assertEquals(2, h.segments.size)
        assertEquals(1, h.segments[1]["segment"].asInt)
        // Every segment starts with the screen so it can be laid out alone.
        assertEquals(listOf("screen", "frame"), types(h.segments[1]))
        assertEquals("2", h.store.map[ReplayEngine.KEY_SEGMENT])
    }

    @Test fun segmentsAreSealedAtSizeLimit() {
        val h = Harness()
        val big = "x".repeat(200)
        var id = 0
        while (h.segments.isEmpty() && id < 5_000) h.frame(*Array(10) { node(id++, big) })
        assertEquals(1, h.segments.size)
        assertTrue(h.segments[0].toString().length >= ReplayEngine.SEGMENT_MAX_BYTES)
    }

    @Test fun segmentCounterContinuesForSameSessionAndResetsForNewOne() {
        val store = MemStore()
        val h1 = Harness(store = store)
        h1.frame(node(1)); h1.engine.flush(); h1.frame(node(2)); h1.engine.flush()
        assertEquals(listOf(0, 1), h1.segments.map { it["segment"].asInt })

        // New process, same RUM session: numbering continues.
        val h2 = Harness(store = store)
        h2.frame(node(3)); h2.engine.flush()
        assertEquals(2, h2.segments[0]["segment"].asInt)

        // Session rotates: the open segment goes to the old session, numbering restarts.
        h2.frame(node(4))
        h2.ctx = TrooperSpi.Context("s2", true, "anon-1", null, null, "Home")
        h2.frame(node(5))
        h2.engine.flush()
        assertEquals(listOf("s1" to 2, "s1" to 3, "s2" to 0), h2.segments.map { it["session_id"].asString to it["segment"].asInt })
    }

    @Test fun samplingDecisionPerSession() {
        // 0.05 < sampleRate 0.1 → record
        assertEquals(ReplayEngine.Mode.RECORD, Harness(ReplayOptions(), SeqRandom(0.05)).engine.refreshSession())
        // 0.5 ≥ 0.1, then 0.5 < onErrorSampleRate 1.0 → buffer
        assertEquals(ReplayEngine.Mode.BUFFER, Harness(ReplayOptions(), SeqRandom(0.5, 0.5)).engine.refreshSession())
        assertEquals(ReplayEngine.Mode.OFF, Harness(ReplayOptions(onErrorSampleRate = 0.0), SeqRandom(0.5)).engine.refreshSession())
        assertEquals(ReplayEngine.Mode.OFF, Harness(ReplayOptions(sampleRate = 1.0), sampled = false).engine.refreshSession())
        assertEquals(ReplayEngine.Mode.OFF, Harness(ReplayOptions(sampleRate = 1.0, enabled = false)).engine.refreshSession())
    }

    @Test fun offModeRecordsNothing() {
        val h = Harness(ReplayOptions(sampleRate = 0.0, onErrorSampleRate = 0.0))
        h.frame(node(1)); h.engine.onTouch(h.now, 1f, 1f, "down"); h.engine.onError(h.now, false); h.engine.flush()
        assertFalse(h.engine.isCapturing)
        assertTrue(h.segments.isEmpty())
    }

    @Test fun onErrorBufferKeepsLastMinuteAndFlushesOnError() {
        val h = Harness(ReplayOptions(sampleRate = 0.0, onErrorSampleRate = 1.0))
        h.frame(node(1, "first"))
        h.advance(30_000)
        h.frame(node(2, "second"))
        h.advance(45_000) // "first" (75 s old) is still the screen state at the window start
        h.engine.onTouch(h.now, 5f, 5f, "down")
        h.advance(20_000) // now "second" (65 s) is the state at the window start: "first" goes
        h.advance(1_000)
        h.engine.flush() // background: buffered data is not sent without an error
        assertTrue(h.segments.isEmpty())

        h.engine.onError(h.now, isCrash = false)
        assertEquals(ReplayEngine.Mode.RECORD, h.engine.mode)
        assertEquals(1, h.segments.size)
        val s = h.segments[0]
        assertEquals(listOf("screen", "frame", "touch"), types(s))
        assertTrue(s["start_ts"].asLong < h.now - ReplayEngine.BUFFER_MS)
        assertEquals(2, s.getAsJsonArray("events")[1].asJsonObject.getAsJsonArray("nodes")[0].asJsonObject["id"].asInt)
        assertEquals(1, s.getAsJsonObject("stats")["errors"].asInt)

        // Recording continues after the error.
        h.frame(node(3)); h.engine.flush()
        assertEquals(2, h.segments.size)
        assertEquals(1, h.segments[1]["segment"].asInt)
    }

    @Test fun crashWritesSynchronously() {
        val h = Harness(ReplayOptions(sampleRate = 0.0, onErrorSampleRate = 1.0))
        h.frame(node(1))
        h.engine.onError(h.now, isCrash = true)
        assertEquals(listOf(true), h.syncFlags)
        assertTrue(h.store.syncWrites > 0)

        val rec = Harness()
        rec.frame(node(1))
        rec.engine.onError(rec.now, isCrash = true)
        assertEquals(listOf(true), rec.syncFlags)
    }

    @Test fun noContextBeforeInit() {
        val h = Harness()
        val engine = ReplayEngine(ReplayOptions(sampleRate = 1.0), { 0L }, MemStore(), { _, _ -> error("no write") }, { null }, { null })
        engine.onFrame(0, 1, 1, 1f, listOf(node(1)), null)
        assertFalse(engine.isCapturing)
        assertNull(engine.sessionId)
        assertTrue(h.segments.isEmpty())
    }
}
