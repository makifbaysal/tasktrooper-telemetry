package com.tasktrooper.android.replay.internal

import java.util.Locale

/**
 * One wireframe node (contracts.md §23): screen coordinates in dp, back-to-front order.
 * Colors are ARGB ints; serialized as `#rrggbb` or `#rrggbbaa`.
 */
internal class WireNode(
    @JvmField val id: Int,
    @JvmField val x: Float,
    @JvmField val y: Float,
    @JvmField val w: Float,
    @JvmField val h: Float,
    @JvmField val kind: String,
    @JvmField val bg: Int? = null,
    @JvmField val fg: Int? = null,
    @JvmField val radius: Float = 0f,
    @JvmField val border: Int? = null,
    @JvmField val text: String? = null,
    @JvmField val masked: Boolean = false,
    @JvmField val alpha: Float = 1f,
    /** Visible part [x, y, w, h] in dp when the node is clipped by an ancestor; null when fully visible. */
    @JvmField val clip: FloatArray? = null,
) {
    fun write(j: Json) {
        j.beginObject()
        j.name("id").value(id.toLong())
        j.name("x").value(round1(x)).name("y").value(round1(y)).name("w").value(round1(w))
            .name("h").value(round1(h))
        j.name("kind").value(kind)
        if (bg != null) j.name("bg").value(Colors.hex(bg))
        if (fg != null) j.name("fg").value(Colors.hex(fg or OPAQUE)) // contract: fg/border are #rrggbb
        if (radius > 0f) j.name("radius").value(round1(radius))
        if (border != null) j.name("border").value(Colors.hex(border or OPAQUE))
        if (text != null) j.name("text").value(text)
        if (masked) j.name("masked").value(true)
        if (alpha < 0.999f) j.name("alpha").value(Math.round(alpha * 100) / 100.0)
        val c = clip
        if (c != null) {
            j.name("clip").beginArray()
            for (v in c) j.value(round1(v))
            j.endArray()
        }
        j.endObject()
    }

    companion object {
        private const val OPAQUE = -0x1000000
        fun round1(v: Float): Double = Math.round(v * 10.0) / 10.0
    }
}

internal object Colors {
    /** `#rrggbb` when opaque, else `#rrggbbaa` (contracts.md §23). */
    fun hex(argb: Int): String {
        val a = (argb ushr 24) and 0xff
        val rgb = argb and 0xffffff
        return if (a == 0xff) String.format(Locale.ROOT, "#%06x", rgb)
        else String.format(Locale.ROOT, "#%06x%02x", rgb, a)
    }

    fun withAlpha(argb: Int, alpha: Float): Int {
        val a = (((argb ushr 24) and 0xff) * alpha.coerceIn(0f, 1f)).toInt()
        return (a shl 24) or (argb and 0xffffff)
    }

    fun isTransparent(argb: Int) = (argb ushr 24) == 0
}

/** Text masking: every non-whitespace character becomes `•`, so length and word shape are preserved. */
internal object Masking {
    const val MAX_TEXT = 300

    fun mask(s: CharSequence): String {
        val n = minOf(s.length, MAX_TEXT)
        val sb = StringBuilder(n)
        for (i in 0 until n) {
            val c = s[i]
            sb.append(if (Character.isWhitespace(c)) c else '•')
        }
        return sb.toString()
    }

    /** Returns (text, masked). Inputs and password fields are always masked; otherwise [maskAll] unless [unmask]. */
    fun apply(raw: CharSequence?, input: Boolean, password: Boolean, maskAll: Boolean, unmask: Boolean): Pair<String?, Boolean> {
        if (raw.isNullOrEmpty()) return null to false
        val mustMask = input || password || (maskAll && !unmask)
        return if (mustMask) mask(raw) to true else raw.subSequence(0, minOf(raw.length, MAX_TEXT)).toString() to false
    }
}

/** Minimal JSON writer (no org.json: it is a stub in JVM unit tests). */
internal class Json(private val sb: StringBuilder = StringBuilder(1024)) {
    private var needComma = false

    fun beginObject() = apply { comma(); sb.append('{'); needComma = false }
    fun endObject() = apply { sb.append('}'); needComma = true }
    fun beginArray() = apply { comma(); sb.append('['); needComma = false }
    fun endArray() = apply { sb.append(']'); needComma = true }
    fun name(n: String) = apply { comma(); string(n); sb.append(':'); needComma = false }
    fun value(v: String) = apply { comma(); string(v); needComma = true }
    fun value(v: Long) = apply { comma(); sb.append(v); needComma = true }
    fun value(v: Boolean) = apply { comma(); sb.append(v); needComma = true }
    fun value(v: Double) = apply {
        comma()
        if (v.isNaN() || v.isInfinite()) sb.append('0')
        else if (v == Math.floor(v) && Math.abs(v) < 1e15) sb.append(v.toLong())
        else sb.append(v)
        needComma = true
    }
    fun raw(json: String) = apply { comma(); sb.append(json); needComma = true }

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
                else -> if (c < ' ' || c == ' ' || c == ' ') {
                    sb.append("\\u").append(String.format(Locale.ROOT, "%04x", c.code))
                } else sb.append(c)
            }
        }
        sb.append('"')
    }

    val length: Int get() = sb.length
    override fun toString(): String = sb.toString()
}

/** Wireframe event JSON (contracts.md §23 `events[]`). */
internal object WireEvents {
    fun screen(ts: Long, widthDp: Int, heightDp: Int, density: Float, name: String?): String {
        val w = Json(StringBuilder(96))
        w.beginObject().name("type").value("screen").name("ts").value(ts)
            .name("width").value(widthDp.toLong()).name("height").value(heightDp.toLong())
            .name("density").value(Math.round(density * 100) / 100.0)
        if (!name.isNullOrEmpty()) w.name("name").value(name)
        return w.endObject().toString()
    }

    fun frame(ts: Long, nodes: List<WireNode>): String {
        val w = Json(StringBuilder(64 + nodes.size * 96))
        w.beginObject().name("type").value("frame").name("ts").value(ts).name("nodes").beginArray()
        for (n in nodes) n.write(w)
        return w.endArray().endObject().toString()
    }

    /** The nodes part only, used to detect unchanged frames. */
    fun nodesKey(nodes: List<WireNode>): String {
        val w = Json(StringBuilder(nodes.size * 96))
        w.beginArray()
        for (n in nodes) n.write(w)
        return w.endArray().toString()
    }

    fun touch(ts: Long, x: Float, y: Float, phase: String): String =
        Json(StringBuilder(80)).beginObject().name("type").value("touch").name("ts").value(ts)
            .name("x").value(WireNode.round1(x)).name("y").value(WireNode.round1(y)).name("phase").value(phase)
            .endObject().toString()

    fun keyboard(ts: Long, visible: Boolean): String =
        Json(StringBuilder(56)).beginObject().name("type").value("keyboard").name("ts").value(ts)
            .name("visible").value(visible).endObject().toString()
}
