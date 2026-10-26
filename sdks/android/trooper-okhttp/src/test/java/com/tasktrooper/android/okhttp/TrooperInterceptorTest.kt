package com.tasktrooper.android.okhttp

import com.tasktrooper.android.RumPayload
import com.tasktrooper.android.internal.Baggage
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.SocketPolicy
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.IOException

class TrooperInterceptorTest {
    private val server = MockWebServer().apply { start() }
    private val events = ArrayList<RumPayload.Resource>()
    private val traceparentRe = Regex("^00-([0-9a-f]{32})-([0-9a-f]{16})-01$")

    private fun client(propagate: Boolean, ingest: Boolean = false, app: String? = "shop app") = OkHttpClient.Builder()
        .addInterceptor(TrooperInterceptor({ events.add(it) }, { propagate }, { ingest }, { app }))
        .build()

    @After fun tearDown() = server.shutdown()

    @Test fun injectsTraceparentAndRecordsResource() {
        server.enqueue(MockResponse().setResponseCode(201).setBody("hello"))
        val url = server.url("/v1/orders/42?token=secret&page=2")
        client(propagate = true).newCall(Request.Builder().url(url).post("{}".toRequestBody()).build()).execute().close()

        val header = server.takeRequest().getHeader("traceparent")!!
        val m = traceparentRe.find(header) ?: return fail("bad traceparent $header")
        val e = events.single()
        assertEquals(m.groupValues[1], e.traceId)
        assertEquals(m.groupValues[2], e.spanId)
        assertEquals("POST", e.method)
        assertEquals(201, e.statusCode)
        assertEquals(2L, e.requestBytes)
        assertEquals(5L, e.responseBytes)
        assertEquals("okhttp", e.initiator)
        assertTrue(e.durationMs >= 0)
        assertEquals("http://${url.host}:${url.port}/v1/orders/42?token=&page=", e.url) // query values stripped
    }

    @Test fun addsBaggageServiceNextToTraceparent() {
        server.enqueue(MockResponse())
        client(propagate = true).newCall(Request.Builder().url(server.url("/")).build()).execute().close()
        assertEquals("tt.svc=shop%20app", server.takeRequest().getHeader("baggage"))
    }

    @Test fun mergesExistingBaggageAndReplacesServiceMember() {
        server.enqueue(MockResponse())
        val existing = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
        client(propagate = true).newCall(Request.Builder().url(server.url("/"))
            .header("traceparent", existing)
            .addHeader("baggage", "k=v, tt.svc=old;p=1")
            .addHeader("baggage", "x=y")
            .build()).execute().close()
        val r = server.takeRequest()
        assertEquals(listOf("tt.svc=shop%20app,k=v,x=y"), r.headers.values("baggage"))
        assertEquals(existing, r.getHeader("traceparent"))
    }

    @Test fun nonMatchingHostKeepsBaggageUntouched() {
        server.enqueue(MockResponse())
        client(propagate = false).newCall(Request.Builder().url(server.url("/")).header("baggage", "k=v").build()).execute().close()
        assertEquals("k=v", server.takeRequest().getHeader("baggage"))
    }

    @Test fun baggageMergeRules() {
        assertEquals("tt.svc=a%2Cb%3Dc", Baggage.merge(null, "a,b=c"))
        assertEquals("a=1", Baggage.merge("a=1", ""))
        val big = "k=" + "x".repeat(Baggage.MAX_BYTES - 5)
        assertEquals(big, Baggage.merge(big, "svc"))
    }

    @Test fun nonMatchingHostGetsNoHeader() {
        server.enqueue(MockResponse().setResponseCode(500))
        client(propagate = false).newCall(Request.Builder().url(server.url("/x")).build()).execute().close()
        val r = server.takeRequest()
        assertNull(r.getHeader("traceparent"))
        assertNull(r.getHeader("baggage"))
        assertNull(events.single().traceId)
        assertEquals(500, events.single().statusCode)
    }

    @Test fun existingTraceparentIsKept() {
        server.enqueue(MockResponse())
        val existing = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
        client(propagate = true).newCall(Request.Builder().url(server.url("/")).header("traceparent", existing).build()).execute().close()
        assertEquals(existing, server.takeRequest().getHeader("traceparent"))
        assertNull(events.single().traceId)
    }

    @Test fun networkErrorRecordsStatusZeroAndRethrows() {
        server.enqueue(MockResponse().setSocketPolicy(SocketPolicy.DISCONNECT_AT_START))
        try {
            client(propagate = true).newCall(Request.Builder().url(server.url("/fail")).build()).execute()
            fail("expected IOException")
        } catch (_: IOException) {
        }
        assertEquals(0, events.single().statusCode)
    }

    @Test fun ingestRequestsAreNotInstrumented() {
        server.enqueue(MockResponse())
        client(propagate = true, ingest = true).newCall(Request.Builder().url(server.url("/v1/rum")).build()).execute().close()
        assertNull(server.takeRequest().getHeader("traceparent"))
        assertTrue(events.isEmpty())
    }

    @Test fun sinkFailureNeverBreaksRequest() {
        server.enqueue(MockResponse().setBody("ok"))
        val c = OkHttpClient.Builder().addInterceptor(TrooperInterceptor({ throw IllegalStateException() }, { throw IllegalStateException() }, { false })).build()
        c.newCall(Request.Builder().url(server.url("/")).build()).execute().use { assertEquals("ok", it.body!!.string()) }
    }

    @Test fun defaultConstructorIsNoOpWithoutInit() {
        server.enqueue(MockResponse())
        OkHttpClient.Builder().addInterceptor(TrooperInterceptor()).build()
            .newCall(Request.Builder().url(server.url("/")).build()).execute().close()
        assertNull(server.takeRequest().getHeader("traceparent"))
    }
}
