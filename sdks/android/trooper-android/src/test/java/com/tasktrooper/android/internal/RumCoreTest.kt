package com.tasktrooper.android.internal

import com.tasktrooper.android.EventType
import com.tasktrooper.android.TrooperConfig
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class RumCoreTest {
    @Test fun viewsCarryTimingsAndIds() {
        val h = Harness()
        val startWall = h.clock.now
        h.core.startView("HomeActivity", "k1", deferStart = true)
        h.clock.advance(120)
        h.core.onViewFirstFrame("k1", 120.0) // VIEW_START emitted now, carrying loading_time_ms
        h.clock.advance(2_880)
        repeat(8) { h.core.frames.record(10.0) }
        h.core.frames.record(30.0); h.core.frames.record(800.0); h.core.frames.record(700.0)
        h.core.startView("DetailActivity", "k2") // implicitly ends HomeActivity
        val ev = h.sentEvents()
        assertEquals(listOf("SESSION_START", "VIEW_START", "VITAL", "VITAL", "VIEW_END", "VIEW_START"), ev.map { it.get("type").asString })
        val start = ev[1]; val end = ev[4]
        assertEquals(start.get("view_id").asString, end.get("view_id").asString)
        assertEquals(startWall, start.get("ts_ms").asLong) // timestamp of the view start, not of the first frame
        assertEquals(120.0, start.getAsJsonObject("view").get("loading_time_ms").asDouble, 0.0)
        assertTrue(start.getAsJsonObject("view").get("is_initial").asBoolean)
        assertFalse(ev[5].getAsJsonObject("view").has("is_initial"))
        assertEquals(3_000.0, end.getAsJsonObject("view").get("time_spent_ms").asDouble, 0.0)
        assertFalse(end.getAsJsonObject("view").has("loading_time_ms"))
        assertEquals("SLOW_FRAMES", ev[2].getAsJsonObject("vital").get("name").asString)
        assertEquals(2.0, ev[2].getAsJsonObject("vital").get("value").asDouble, 0.0) // 30 ms and 700 ms (frozen is > 700)
        assertEquals(1.0, ev[3].getAsJsonObject("vital").get("value").asDouble, 0.0)
        assertEquals("11", ev[2].getAsJsonObject("attributes").get("total_frames").asString)
        assertEquals("11", ev[3].getAsJsonObject("attributes").get("total_frames").asString)
        assertNotEquals(start.get("view_id").asString, ev[5].get("view_id").asString)
        assertEquals("wifi", start.get("connection").asString)
    }

    @Test fun deferredViewStartIsEmittedWhenViewEndsBeforeFirstFrame() {
        val h = Harness()
        h.core.startView("A", "a", deferStart = true)
        h.core.stopView("a")
        h.core.onViewFirstFrame("a", 50.0) // too late: ignored
        val ev = h.sentEvents().map { it.get("type").asString }
        assertEquals(listOf("SESSION_START", "VIEW_START", "VIEW_END"), ev)
    }

    @Test fun appStartReference() {
        val h = Harness()
        h.core.appStart("cold", 800.0, "process")
        h.core.appStart("hot", 90.0)
        val ev = h.sentEvents().filter { it.get("type").asString == "APP_START" }
        assertEquals("process", ev[0].getAsJsonObject("attributes").get("start_reference").asString)
        assertFalse(ev[1].has("attributes"))
    }

    @Test fun hybridAdoptionSharesSessionAndView() {
        val h = Harness()
        h.sessions.adoptAnonymousId("anon-from-js")
        h.sessions.adopt("sess-from-js")
        h.core.adoptView("view-js", "/checkout")
        h.core.crash(Thread.currentThread(), RuntimeException("native crash"))
        h.uploader.drain(); h.scheduler.runDue()
        val ev = h.transport.batches().flatMap { it.getAsJsonArray("events").map { e -> e.asJsonObject } }
        assertEquals(listOf("ERROR"), ev.map { it.get("type").asString }) // no SESSION_START: the JS layer owns it
        assertEquals("sess-from-js", ev[0].get("session_id").asString)
        assertEquals("anon-from-js", ev[0].get("anonymous_id").asString)
        assertEquals("view-js", ev[0].get("view_id").asString)
        assertEquals("/checkout", ev[0].get("view_name").asString)
        h.core.startView("NativeScreen") // ending an adopted view emits nothing for it
        assertEquals(listOf("VIEW_START"), h.sentEvents().drop(1).map { it.get("type").asString })
    }

    @Test fun stopViewOnlyMatchingKey() {
        val h = Harness()
        h.core.startView("A", "a")
        h.core.stopView("other")
        assertEquals("A", h.core.currentViewName)
        h.core.stopView("a")
        assertEquals(null, h.core.currentViewName)
    }

    @Test fun userAndCustomEvents() {
        val h = Harness()
        h.core.user = com.tasktrooper.android.RumUser("u-1", mapOf("plan" to "pro"))
        h.core.track("checkout_started", mapOf("cart" to "3"), 120.5)
        val e = h.sentEvents().last()
        assertEquals("u-1", e.getAsJsonObject("user").get("id").asString)
        assertEquals("pro", e.getAsJsonObject("user").getAsJsonObject("traits").get("plan").asString)
        assertEquals(120.5, e.getAsJsonObject("custom").get("value").asDouble, 0.0)
        assertEquals(h.sessions.anonymousId, e.get("anonymous_id").asString)
    }

    @Test fun rageTapsAreFlagged() {
        val h = Harness()
        repeat(3) { h.core.action("pay", "btn_pay"); h.clock.advance(200) }
        h.clock.advance(2_000)
        h.core.action("pay", "btn_pay")
        val actions = h.sentEvents().filter { it.get("type").asString == "ACTION" }.map { it.getAsJsonObject("action") }
        assertEquals(listOf(false, false, true, false), actions.map { it.has("rage") })
    }

    @Test fun beforeSendCanDropAndMutate() {
        val cfg = TrooperConfig(dsn = "https://k@h/1", app = "a", beforeSend = { e ->
            when (e.type) {
                EventType.CUSTOM -> null
                EventType.ERROR -> e.copy(attributes = e.attributes + ("masked" to "1"))
                else -> e
            }
        })
        val h = Harness(config = cfg)
        h.core.track("secret", emptyMap(), 0.0)
        h.core.error(RuntimeException("x"), "source", emptyMap())
        val ev = h.sentEvents()
        assertTrue(ev.none { it.get("type").asString == "CUSTOM" })
        assertEquals("1", ev.last().getAsJsonObject("attributes").get("masked").asString)
    }

    @Test fun throwingBeforeSendDoesNotLoseEventsOrCrash() {
        val h = Harness(config = TrooperConfig(dsn = "https://k@h/1", app = "a", beforeSend = { throw IllegalStateException() }))
        h.core.track("x", emptyMap(), 0.0)
        assertTrue(h.sentEvents().any { it.get("type").asString == "CUSTOM" })
    }

    @Test fun unsampledSessionsSendNothing() {
        val h = Harness(sampleRate = 0.0)
        h.core.startView("A"); h.core.track("x", emptyMap(), 0.0)
        assertEquals(0, h.sentEvents().size)
    }

    @Test fun newSessionEmitsSessionStartAndInitialView() {
        val h = Harness()
        h.core.startView("A")
        h.clock.advance(31 * 60_000L)
        h.core.startView("B")
        val ev = h.sentEvents()
        assertEquals(2, ev.count { it.get("type").asString == "SESSION_START" })
        val b = ev.last { it.get("type").asString == "VIEW_START" }
        assertTrue(b.getAsJsonObject("view").get("is_initial").asBoolean)
    }

    @Test fun dsnParsing() {
        val d = com.tasktrooper.android.Dsn.parse("http://tt_dev_ingest_key@localhost:4318/1")!!
        assertEquals("http://localhost:4318/v1/rum", d.endpoint)
        assertEquals("tt_dev_ingest_key", d.key)
        assertEquals("1", d.projectId)
        assertEquals(null, com.tasktrooper.android.Dsn.parse("not a dsn"))
        assertEquals(null, com.tasktrooper.android.Dsn.parse("https://ingest.trooper.dev/1"))
    }

    @Test fun urlSanitizerKeepsKeysOnly() {
        assertEquals("https://a.com/p?token=&page=", UrlSanitizer.sanitize("https://a.com/p?token=abc&page=2#frag"))
        assertEquals("https://a.com/p", UrlSanitizer.sanitize("https://a.com/p?"))
        assertEquals("https://a.com/p", UrlSanitizer.sanitize("https://a.com/p"))
    }

    @Test fun traceparentFormat() {
        val t = TraceIds.traceId(); val s = TraceIds.spanId()
        assertTrue(Regex("^00-[0-9a-f]{32}-[0-9a-f]{16}-01$").matches(TraceIds.traceparent(t, s)))
    }

    @Test fun baggageMergeEncodesAndReplacesServiceMember() {
        assertEquals("tt.svc=shop%20app", Baggage.merge(null, "shop app"))
        assertEquals("tt.svc=svc,a=1,b=2;prop", Baggage.merge("tt.svc=x, a=1 ,, b=2;prop", "svc"))
        assertEquals(null, Baggage.merge(null, null))
    }
}
