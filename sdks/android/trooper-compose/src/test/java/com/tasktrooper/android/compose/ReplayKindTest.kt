package com.tasktrooper.android.compose

import androidx.compose.ui.semantics.Role
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ReplayKindTest {
    private fun kind(role: Role? = null, input: Boolean = false, text: Boolean = false, scroll: Boolean = false, desc: Boolean = false, w: Float = 100f, h: Float = 40f) =
        replayKind(role, input, text, scroll, desc, w, h)

    @Test fun mapsRolesToWireframeKinds() {
        assertEquals("input", kind(input = true, text = true))
        assertEquals("button", kind(Role.Button))
        assertEquals("button", kind(Role.Tab))
        assertEquals("switch", kind(Role.Switch))
        assertEquals("switch", kind(Role.Checkbox))
        assertEquals("switch", kind(Role.RadioButton))
        assertEquals("image", kind(Role.Image, w = 200f, h = 120f))
        assertEquals("icon", kind(Role.Image, w = 24f, h = 24f))
        assertEquals("icon", kind(desc = true, w = 24f, h = 24f))
        assertEquals("list", kind(scroll = true))
        assertEquals("text", kind(text = true))
    }

    @Test fun plainContainersAreNotDrawn() {
        assertNull(kind())
    }
}
