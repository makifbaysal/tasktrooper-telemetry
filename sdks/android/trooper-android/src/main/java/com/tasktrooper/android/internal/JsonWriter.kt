package com.tasktrooper.android.internal

/**
 * Minimal allocation-light JSON writer. We avoid org.json (Android stub in unit tests) and any
 * third-party dependency in the core module.
 */
internal class JsonWriter(private val sb: StringBuilder = StringBuilder(256)) {
    private var needComma = false

    fun beginObject(): JsonWriter = apply { comma(); sb.append('{'); needComma = false }
    fun endObject(): JsonWriter = apply { sb.append('}'); needComma = true }
    fun beginArray(): JsonWriter = apply { comma(); sb.append('['); needComma = false }
    fun endArray(): JsonWriter = apply { sb.append(']'); needComma = true }

    fun name(name: String): JsonWriter = apply {
        comma(); string(name); sb.append(':'); needComma = false
    }

    fun value(v: String): JsonWriter = apply { comma(); string(v); needComma = true }
    fun value(v: Long): JsonWriter = apply { comma(); sb.append(v); needComma = true }
    fun value(v: Boolean): JsonWriter = apply { comma(); sb.append(v); needComma = true }
    fun value(v: Double): JsonWriter = apply {
        comma()
        if (v.isNaN() || v.isInfinite()) sb.append('0')
        else if (v == Math.floor(v) && Math.abs(v) < 1e15) sb.append(v.toLong())
        else sb.append(v)
        needComma = true
    }

    /** Appends an already-serialized JSON value verbatim. */
    fun raw(json: String): JsonWriter = apply { comma(); sb.append(json); needComma = true }

    // Helpers that skip proto3 default values to keep payloads small.
    fun field(name: String, v: String?) = apply { if (!v.isNullOrEmpty()) name(name).value(v) }
    fun field(name: String, v: Long) = apply { if (v != 0L) name(name).value(v) }
    fun field(name: String, v: Int) = apply { if (v != 0) name(name).value(v.toLong()) }
    fun field(name: String, v: Double) = apply { if (v != 0.0) name(name).value(v) }
    fun field(name: String, v: Boolean) = apply { if (v) name(name).value(true) }
    fun field(name: String, map: Map<String, String>?) = apply {
        if (!map.isNullOrEmpty()) {
            name(name).beginObject()
            for ((k, v) in map) name(k).value(v)
            endObject()
        }
    }

    private fun comma() { if (needComma) sb.append(',') }

    private fun string(s: String) {
        sb.append('"')
        for (c in s) {
            when (c) {
                '"' -> sb.append("\\\"")
                '\\' -> sb.append("\\\\")
                '\n' -> sb.append("\\n")
                '\r' -> sb.append("\\r")
                '\t' -> sb.append("\\t")
                '\b' -> sb.append("\\b")
                '\u000C' -> sb.append("\\f")
                else -> if (c < ' ' || c == ' ' || c == ' ') {
                    sb.append("\\u").append(String.format("%04x", c.code))
                } else sb.append(c)
            }
        }
        sb.append('"')
    }

    override fun toString(): String = sb.toString()
}
