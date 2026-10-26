package com.tasktrooper.android.internal

import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.tasktrooper.android.EventType
import com.tasktrooper.android.RumEvent
import com.tasktrooper.android.RumPayload
import com.tasktrooper.android.RumUser
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.File

/**
 * Validates the JSON we produce against proto/rum/v1/rum.proto (parsed here, not hard-coded):
 * every key must be a proto field name, enum values must be valid names, and every field the SDK
 * is expected to populate must actually be emitted.
 */
class RumSchemaTest {
    private data class Field(val type: String, val repeated: Boolean)

    private val messages = HashMap<String, MutableMap<String, Field>>()
    private val enums = HashMap<String, MutableSet<String>>()
    private val seen = HashSet<String>()

    init { parseProto(protoFile().readText()) }

    private fun protoFile(): File {
        var dir: File? = File("").absoluteFile
        while (dir != null) {
            val f = File(dir, "proto/rum/v1/rum.proto")
            if (f.exists()) return f
            dir = dir.parentFile
        }
        error("rum.proto not found")
    }

    private fun parseProto(src: String) {
        val fieldRe = Regex("""^(repeated\s+)?(map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*\d+\s*;""")
        val enumValRe = Regex("""^(\w+)\s*=\s*\d+\s*;""")
        var msg: String? = null; var en: String? = null
        for (raw in src.lines()) {
            val line = raw.substringBefore("//").trim()
            if (line.isEmpty()) continue
            Regex("""^message\s+(\w+)\s*\{""").find(line)?.let { msg = it.groupValues[1]; messages[msg!!] = LinkedHashMap(); return@let }
            Regex("""^enum\s+(\w+)\s*\{""").find(line)?.let { en = it.groupValues[1]; enums[en!!] = HashSet(); return@let }
            if (line.startsWith("oneof")) continue
            if (line == "}") { if (en != null) en = null; continue }
            if (en != null) { enumValRe.find(line)?.let { enums[en!!]!!.add(it.groupValues[1]) }; continue }
            if (msg != null) fieldRe.find(line)?.let { m ->
                messages[msg!!]!![m.groupValues[3]] = Field(m.groupValues[2], m.groupValues[1].isNotBlank())
            }
        }
        assertTrue("parsed RumEvent", messages["RumEvent"]!!.containsKey("app_start"))
    }

    private fun validate(el: JsonElement, message: String, path: String) {
        val obj = el.asJsonObject
        val fields = messages[message] ?: return fail("unknown message $message at $path")
        for ((key, value) in obj.entrySet()) {
            val f = fields[key] ?: return fail("$path.$key is not a field of $message (fields: ${fields.keys})")
            seen.add("$message.$key")
            when {
                f.type.startsWith("map<") -> value.asJsonObject.entrySet().forEach { assertTrue("$path.$key map value", it.value.asJsonPrimitive.isString) }
                f.repeated && messages.containsKey(f.type) -> value.asJsonArray.forEach { validate(it, f.type, "$path.$key[]") }
                messages.containsKey(f.type) -> validate(value, f.type, "$path.$key")
                enums.containsKey(f.type) -> assertTrue("$path.$key=$value not in enum ${f.type}", value.asString in enums[f.type]!!)
                f.type == "string" -> assertTrue("$path.$key must be string", value.asJsonPrimitive.isString)
                f.type == "bool" -> assertTrue("$path.$key must be bool", value.asJsonPrimitive.isBoolean)
                f.type in setOf("int32", "int64", "double", "float", "uint32", "uint64") ->
                    assertTrue("$path.$key must be number", value.asJsonPrimitive.isNumber)
                else -> fail("unhandled type ${f.type}")
            }
        }
    }

    private fun event(type: EventType, payload: RumPayload?) = RumEvent(
        type = type, tsMs = 1_700_000_000_000, sessionId = "s", viewId = "v", viewName = "Home",
        anonymousId = "a", user = RumUser("u", mapOf("plan" to "pro")), connection = "wifi",
        payload = payload, attributes = mapOf("k" to "v"),
    )

    private val allEvents = listOf(
        event(EventType.SESSION_START, null),
        event(EventType.VIEW_START, RumPayload.View(url = "", referrer = "Prev", loadingTimeMs = 1.5, isInitial = true)),
        event(EventType.VIEW_END, RumPayload.View(url = "x", referrer = "r", loadingTimeMs = 120.0, timeSpentMs = 3000.0)),
        event(EventType.VITAL, RumPayload.Vital("SLOW_FRAMES", 3.0, "good")),
        event(EventType.ERROR, RumPayload.Error("java.lang.NPE", "m", "stack", "crash", true, "main", "fp")),
        event(EventType.RESOURCE, RumPayload.Resource("https://a/b?x=", "GET", 200, 12.5, 10, 20, "t" , "s", "okhttp")),
        event(EventType.ACTION, RumPayload.Action("pay", "btn", rage = true, dead = true)),
        event(EventType.APP_START, RumPayload.AppStart("cold", 812.0)),
        event(EventType.CUSTOM, RumPayload.Custom("checkout", mapOf("p" to "1"), 9.5)),
    )

    @Test fun batchMatchesProto() {
        val json = testResource().batchJson(allEvents.map { it.toJson() })
        validate(JsonParser.parseString(json), "RumBatch", "batch")

        // Every field the Android SDK is responsible for is emitted at least once.
        val expected = listOf("RumBatch", "Resource", "Sdk", "Device", "Os", "User", "RumEvent", "ViewPayload",
            "VitalPayload", "ErrorPayload", "ResourcePayload", "ActionPayload", "AppStartPayload", "CustomPayload")
            .flatMap { m -> messages[m]!!.keys.map { "$m.$it" } }
            .filterNot { it == "Resource.browser" } // web only
        val missing = expected.filterNot { it in seen }
        assertEquals("fields never emitted", emptyList<String>(), missing)
    }

    @Test fun eventTypeEnumMatchesProto() {
        val proto = enums["EventType"]!! - "EVENT_TYPE_UNSPECIFIED"
        assertEquals(proto, EventType.values().map { it.name }.toSet())
        assertTrue("PLATFORM_ANDROID" in enums["Platform"]!!)
    }

    @Test fun eventsFromCoreValidate() {
        val h = Harness()
        h.core.startView("A"); h.core.action("tap", null); h.core.track("t", mapOf("a" to "b"), 1.0)
        h.core.error(RuntimeException("e"), "source", mapOf("x" to "y")); h.core.appStart("cold", 10.0)
        h.core.resource(RumPayload.Resource("https://a", "POST", 500, 1.0))
        h.core.stopView()
        h.pipeline.flush(); h.scheduler.runDue()
        h.transport.bodies.forEach { validate(JsonParser.parseString(it), "RumBatch", "batch") }
        assertTrue(h.transport.bodies.isNotEmpty())
    }

    @Test fun jsonEscaping() {
        val e = event(EventType.CUSTOM, RumPayload.Custom("q\"uote\\\n\t\u0001"))
        val parsed: JsonObject = JsonParser.parseString(e.toJson()).asJsonObject
        assertEquals("q\"uote\\\n\t\u0001", parsed.getAsJsonObject("custom").get("name").asString)
    }
}
