package com.tasktrooper.android.okhttp

import com.tasktrooper.android.RumPayload
import com.tasktrooper.android.TaskTrooper
import com.tasktrooper.android.internal.Baggage
import com.tasktrooper.android.internal.TraceIds
import com.tasktrooper.android.internal.UrlSanitizer
import okhttp3.Interceptor
import okhttp3.Response
import java.io.IOException

/**
 * OkHttp interceptor producing RUM `RESOURCE` events. For URLs matching
 * `TrooperConfig.tracePropagationTargets` it injects a W3C `traceparent` header and records
 * `trace_id`/`span_id` so the request joins the backend trace. The same requests get
 * `baggage: tt.svc=<app>` (merged into an existing `baggage` header) so the backend knows the caller.
 *
 * Add as an application interceptor: `OkHttpClient.Builder().addInterceptor(TrooperInterceptor())`.
 * Headers, bodies and query values are never recorded.
 */
class TrooperInterceptor internal constructor(
    private val sink: (RumPayload.Resource) -> Unit,
    private val shouldPropagate: (String) -> Boolean,
    private val isIngest: (String) -> Boolean,
    private val serviceName: () -> String? = { null },
) : Interceptor {

    constructor() : this(
        TaskTrooper::recordResource, TaskTrooper::shouldPropagateTrace, TaskTrooper::isIngestUrl, TaskTrooper::baggageServiceName,
    )

    override fun intercept(chain: Interceptor.Chain): Response {
        val original = chain.request()
        // Telemetry failures must never affect the request: fall back to a plain proceed.
        val prepared = try { prepare(original) } catch (_: Throwable) { null }
            ?: return chain.proceed(original)
        val (request, traceId, spanId) = prepared
        // A request started right after a tap means the tap was not "dead" (even if it completes later).
        try { TaskTrooper.notifyActivity() } catch (_: Throwable) {}
        val start = System.nanoTime()
        val response = try {
            chain.proceed(request)
        } catch (e: IOException) {
            record(original, 0, start, null, traceId, spanId)
            throw e
        }
        record(original, response.code, start, response, traceId, spanId)
        return response
    }

    private fun prepare(original: okhttp3.Request): Triple<okhttp3.Request, String?, String?>? {
        val url = original.url.toString()
        if (isIngest(original.url.host)) return null
        if (!shouldPropagate(url)) return Triple(original, null, null)
        val builder = original.newBuilder()
        var traceId: String? = null
        var spanId: String? = null
        if (original.header(TRACEPARENT) == null) {
            traceId = TraceIds.traceId()
            spanId = TraceIds.spanId()
            builder.header(TRACEPARENT, TraceIds.traceparent(traceId, spanId))
        }
        try {
            val existing = original.headers(Baggage.HEADER).takeIf { it.isNotEmpty() }?.joinToString(",")
            val merged = Baggage.merge(existing, serviceName())
            if (merged != null && merged != existing) builder.header(Baggage.HEADER, merged)
        } catch (_: Throwable) {
        }
        return Triple(builder.build(), traceId, spanId)
    }

    private fun record(req: okhttp3.Request, status: Int, startNs: Long, response: Response?, traceId: String?, spanId: String?) {
        try {
            val durationMs = (System.nanoTime() - startNs) / 1_000_000.0
            val reqBytes = try { req.body?.contentLength() ?: 0L } catch (_: Throwable) { 0L }
            val respBytes = response?.body?.contentLength()?.takeIf { it >= 0 }
                ?: response?.header("Content-Length")?.toLongOrNull() ?: 0L
            sink(RumPayload.Resource(
                url = UrlSanitizer.sanitize(req.url.toString()),
                method = req.method,
                statusCode = status,
                durationMs = durationMs,
                requestBytes = reqBytes.coerceAtLeast(0),
                responseBytes = respBytes.coerceAtLeast(0),
                traceId = traceId,
                spanId = spanId,
                initiator = "okhttp",
            ))
        } catch (_: Throwable) {
        }
    }

    companion object {
        const val TRACEPARENT = "traceparent"
    }
}
