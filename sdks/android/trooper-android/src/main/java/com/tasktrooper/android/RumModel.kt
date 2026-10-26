package com.tasktrooper.android

import com.tasktrooper.android.internal.JsonWriter

/** Mirrors `trooper.rum.v1.EventType` (proto/rum/v1/rum.proto). */
enum class EventType {
    SESSION_START, VIEW_START, VIEW_END, VITAL, ERROR, RESOURCE, ACTION, APP_START, CUSTOM
}

/** User attached after [TaskTrooper.identify]. Traits are opt-in; PII is the app's responsibility. */
data class RumUser(val id: String, val traits: Map<String, String> = emptyMap())

/** One RUM event. JSON field names follow rum.proto `RumEvent` exactly (snake_case). */
data class RumEvent(
    val type: EventType,
    val tsMs: Long,
    val sessionId: String,
    val viewId: String?,
    val viewName: String?,
    val anonymousId: String,
    val user: RumUser?,
    val connection: String?,
    val payload: RumPayload?,
    val attributes: Map<String, String> = emptyMap(),
) {
    internal fun toJson(): String {
        val w = JsonWriter()
        w.beginObject()
        w.name("type").value(type.name)
        w.name("ts_ms").value(tsMs)
        w.field("session_id", sessionId)
        w.field("view_id", viewId)
        w.field("view_name", viewName)
        w.field("anonymous_id", anonymousId)
        if (user != null) {
            w.name("user").beginObject().field("id", user.id).field("traits", user.traits).endObject()
        }
        w.field("connection", connection)
        if (payload != null) {
            w.name(payload.jsonName).beginObject()
            payload.write(w)
            w.endObject()
        }
        w.field("attributes", attributes)
        w.endObject()
        return w.toString()
    }
}

/** `oneof payload` of RumEvent. */
sealed class RumPayload(internal val jsonName: String) {
    internal abstract fun write(w: JsonWriter)

    data class View(
        val url: String? = null,
        val referrer: String? = null,
        val loadingTimeMs: Double = 0.0,
        val timeSpentMs: Double = 0.0,
        val isInitial: Boolean = false,
    ) : RumPayload("view") {
        override fun write(w: JsonWriter) {
            w.field("url", url).field("referrer", referrer).field("loading_time_ms", loadingTimeMs)
                .field("time_spent_ms", timeSpentMs).field("is_initial", isInitial)
        }
    }

    data class Vital(val name: String, val value: Double, val rating: String? = null) : RumPayload("vital") {
        override fun write(w: JsonWriter) {
            w.field("name", name).name("value").value(value)
            w.field("rating", rating)
        }
    }

    data class Error(
        val type: String,
        val message: String?,
        val stack: String?,
        val source: String,
        val isCrash: Boolean = false,
        val thread: String? = null,
        val fingerprint: String? = null,
    ) : RumPayload("error") {
        override fun write(w: JsonWriter) {
            w.field("type", type).field("message", message).field("stack", stack).field("source", source)
                .field("is_crash", isCrash).field("thread", thread).field("fingerprint", fingerprint)
        }
    }

    data class Resource(
        val url: String,
        val method: String,
        val statusCode: Int,
        val durationMs: Double,
        val requestBytes: Long = 0,
        val responseBytes: Long = 0,
        val traceId: String? = null,
        val spanId: String? = null,
        val initiator: String = "okhttp",
    ) : RumPayload("resource") {
        override fun write(w: JsonWriter) {
            w.field("url", url).field("method", method).field("status_code", statusCode)
                .field("duration_ms", durationMs).field("request_bytes", requestBytes)
                .field("response_bytes", responseBytes).field("trace_id", traceId).field("span_id", spanId)
                .field("initiator", initiator)
        }
    }

    data class Action(val name: String, val target: String? = null, val rage: Boolean = false, val dead: Boolean = false) :
        RumPayload("action") {
        override fun write(w: JsonWriter) {
            w.field("name", name).field("target", target).field("rage", rage).field("dead", dead)
        }
    }

    data class AppStart(val kind: String, val durationMs: Double) : RumPayload("app_start") {
        override fun write(w: JsonWriter) {
            w.field("kind", kind).name("duration_ms").value(durationMs)
        }
    }

    data class Custom(val name: String, val props: Map<String, String> = emptyMap(), val value: Double = 0.0) :
        RumPayload("custom") {
        override fun write(w: JsonWriter) {
            w.field("name", name).field("props", props).field("value", value)
        }
    }
}

/** `trooper.rum.v1.Resource`: who sent the batch. Constant per process. */
data class RumResource(
    val app: String,
    val version: String,
    val environment: String,
    val sdkName: String = "trooper-android",
    val sdkVersion: String,
    val deviceType: String,
    val manufacturer: String,
    val model: String,
    val locale: String,
    val screenWidth: Int,
    val screenHeight: Int,
    val osName: String = "Android",
    val osVersion: String,
    val attributes: Map<String, String> = emptyMap(),
) {
    internal fun write(w: JsonWriter) {
        w.beginObject()
        w.field("app", app).field("version", version).field("environment", environment)
        w.name("platform").value("PLATFORM_ANDROID")
        w.name("sdk").beginObject().field("name", sdkName).field("version", sdkVersion).endObject()
        w.name("device").beginObject().field("type", deviceType).field("manufacturer", manufacturer)
            .field("model", model).field("locale", locale).field("screen_width", screenWidth)
            .field("screen_height", screenHeight).endObject()
        w.name("os").beginObject().field("name", osName).field("version", osVersion).endObject()
        w.field("attributes", attributes)
        w.endObject()
    }

    /** Serializes a `RumBatch` from pre-serialized event JSON strings. */
    internal fun batchJson(events: List<String>): String {
        val w = JsonWriter(StringBuilder(128 + events.sumOf { it.length + 1 }))
        w.beginObject().name("resource")
        write(w)
        w.name("events").beginArray()
        for (e in events) w.raw(e)
        w.endArray().endObject()
        return w.toString()
    }
}
