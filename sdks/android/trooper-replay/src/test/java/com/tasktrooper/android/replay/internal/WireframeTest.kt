package com.tasktrooper.android.replay.internal

import com.google.gson.JsonParser
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class WireframeTest {
    @Test fun maskKeepsLengthAndWhitespace() {
        assertEquals("•••• •••", Masking.mask("John Doe"))
        assertEquals("••\n•", Masking.mask("ab\nc"))
        assertEquals(Masking.MAX_TEXT, Masking.mask("x".repeat(1000)).length)
    }

    @Test fun maskingRules() {
        assertEquals("•••" to true, Masking.apply("abc", input = false, password = false, maskAll = true, unmask = false))
        assertEquals("abc" to false, Masking.apply("abc", input = false, password = false, maskAll = true, unmask = true))
        assertEquals("abc" to false, Masking.apply("abc", input = false, password = false, maskAll = false, unmask = false))
        // Inputs and passwords are always masked, even when unmasked.
        assertEquals("•••" to true, Masking.apply("abc", input = true, password = false, maskAll = false, unmask = true))
        assertEquals("•••" to true, Masking.apply("abc", input = false, password = true, maskAll = false, unmask = true))
        assertEquals(null to false, Masking.apply("", input = true, password = false, maskAll = true, unmask = false))
    }

    @Test fun colorsAreHex() {
        assertEquals("#ff0000", Colors.hex(0xFFFF0000.toInt()))
        assertEquals("#00ff0080", Colors.hex(0x8000FF00.toInt()))
        assertEquals("#123456", Colors.hex(0xFF123456.toInt()))
        assertTrue(Colors.isTransparent(0x00FFFFFF))
        assertEquals(0x80FF0000.toInt(), Colors.withAlpha(0xFFFF0000.toInt(), 128 / 255f))
    }

    @Test fun nodeJsonOmitsDefaults() {
        val j = Json()
        WireNode(7, 1.04f, 2f, 3.333f, 4f, "button", bg = 0xFF336699.toInt(), fg = 0x80000000.toInt(), radius = 8f,
            text = "••", masked = true, alpha = 0.5f, clip = floatArrayOf(1f, 2f, 3f, 2f)).write(j)
        val o = JsonParser.parseString(j.toString()).asJsonObject
        assertEquals(7, o["id"].asInt)
        assertEquals(1.0, o["x"].asDouble, 0.0)
        assertEquals(3.3, o["w"].asDouble, 0.0)
        assertEquals("button", o["kind"].asString)
        assertEquals("#336699", o["bg"].asString)
        assertEquals("#000000", o["fg"].asString) // fg is always #rrggbb
        assertEquals(8.0, o["radius"].asDouble, 0.0)
        assertTrue(o["masked"].asBoolean)
        assertEquals(0.5, o["alpha"].asDouble, 0.0)
        assertEquals(4, o.getAsJsonArray("clip").size())

        val plain = Json().also { WireNode(1, 0f, 0f, 1f, 1f, "view").write(it) }.toString()
        val p = JsonParser.parseString(plain).asJsonObject
        for (k in listOf("bg", "fg", "radius", "border", "text", "masked", "alpha", "clip")) assertNull(k, p[k])
        assertFalse(plain.contains("null"))
    }

    @Test fun eventsSerialize() {
        val s = JsonParser.parseString(WireEvents.screen(5, 390, 844, 2.625f, "Home")).asJsonObject
        assertEquals("screen", s["type"].asString); assertEquals(2.63, s["density"].asDouble, 0.0)
        val k = JsonParser.parseString(WireEvents.keyboard(6, true)).asJsonObject
        assertEquals("keyboard", k["type"].asString); assertTrue(k["visible"].asBoolean)
        val t = JsonParser.parseString(WireEvents.touch(7, 1.25f, 2f, "move")).asJsonObject
        assertEquals(1.3, t["x"].asDouble, 0.0); assertEquals("move", t["phase"].asString)
        val f = JsonParser.parseString(WireEvents.frame(8, listOf(WireNode(1, 0f, 0f, 1f, 1f, "view")))).asJsonObject
        assertEquals(1, f.getAsJsonArray("nodes").size())
    }
}
