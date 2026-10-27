package com.tasktrooper.android.replay.internal

import android.app.Application
import android.graphics.Color
import android.graphics.drawable.ColorDrawable
import android.graphics.drawable.GradientDrawable
import android.text.InputType
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.CheckBox
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import androidx.test.core.app.ApplicationProvider
import com.tasktrooper.android.replay.trooperMask
import com.tasktrooper.android.replay.trooperUnmask
import com.tasktrooper.android.spi.ReplayNodeSink
import com.tasktrooper.android.spi.ReplayViewMapper
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [35])
class ViewMapperTest {
    private val ctx: Application = ApplicationProvider.getApplicationContext()

    private fun <T : View> T.at(l: Int, t: Int, w: Int, h: Int): T = apply {
        layoutParams = FrameLayout.LayoutParams(w, h).apply { leftMargin = l; topMargin = t }
    }

    private fun layout(root: View, w: Int = 1080, h: Int = 2400): View {
        root.measure(View.MeasureSpec.makeMeasureSpec(w, View.MeasureSpec.EXACTLY), View.MeasureSpec.makeMeasureSpec(h, View.MeasureSpec.EXACTLY))
        root.layout(0, 0, w, h)
        return root
    }

    private fun map(root: View, maskAll: Boolean = true, density: Float = 3f, mapper: ReplayViewMapper? = null) =
        ViewMapper(maskAll, mapper).map(listOf(root), density)

    private fun List<WireNode>.byId(v: View) = firstOrNull { it.id == System.identityHashCode(v) }

    @Test fun mapsKindsBoundsAndMasking() {
        val root = FrameLayout(ctx).apply { background = ColorDrawable(Color.WHITE) }
        val title = TextView(ctx).apply { text = "Hello World"; setTextColor(Color.RED) }.at(30, 60, 300, 90)
        val email = EditText(ctx).apply { setText("me@example.com") }.at(30, 200, 600, 120)
        val hint = EditText(ctx).apply { this.hint = "Email" }.at(30, 400, 600, 120)
        val button = Button(ctx).apply { text = "Pay now" }.at(30, 600, 300, 120)
        val check = CheckBox(ctx).apply { text = "Remember" }.at(30, 800, 300, 120)
        val photo = ImageView(ctx).at(0, 1000, 600, 600)
        val icon = ImageView(ctx).at(900, 60, 72, 72)
        val card = View(ctx).apply {
            background = GradientDrawable().apply { setColor(Color.BLUE); cornerRadius = 24f }
        }.at(600, 1000, 300, 300)
        val empty = FrameLayout(ctx).at(0, 2000, 300, 300) // no background → not drawn
        listOf(title, email, hint, button, check, photo, icon, card, empty).forEach { root.addView(it) }
        val r = map(layout(root))
        val nodes = r.nodes

        assertFalse(r.truncated)
        assertEquals("view", nodes[0].kind)
        assertEquals(Color.WHITE, nodes[0].bg)
        assertEquals(360f, nodes[0].w, 0.01f) // 1080 px / density 3

        val t = nodes.byId(title)!!
        assertEquals("text", t.kind)
        assertEquals(10f, t.x, 0.01f); assertEquals(20f, t.y, 0.01f); assertEquals(100f, t.w, 0.01f); assertEquals(30f, t.h, 0.01f)
        assertEquals("••••• •••••", t.text); assertTrue(t.masked)
        assertEquals(Color.RED, t.fg)

        val e = nodes.byId(email)!!
        assertEquals("input", e.kind); assertEquals("•".repeat(14), e.text); assertTrue(e.masked)
        assertEquals("•••••", nodes.byId(hint)!!.text)
        assertEquals("button", nodes.byId(button)!!.kind)
        assertEquals("switch", nodes.byId(check)!!.kind)
        assertEquals("image", nodes.byId(photo)!!.kind)
        assertEquals("icon", nodes.byId(icon)!!.kind)
        val c = nodes.byId(card)!!
        assertEquals("view", c.kind); assertEquals(Color.BLUE, c.bg); assertEquals(8f, c.radius, 0.01f)
        assertNull(nodes.byId(empty))
        // Draw order: parent before children, siblings in order.
        assertTrue(nodes.indexOf(t) < nodes.indexOf(e))
    }

    @Test fun unmaskTagIsInheritedButInputsStayMasked() {
        val root = FrameLayout(ctx)
        val group = LinearLayout(ctx).apply { orientation = LinearLayout.VERTICAL; trooperUnmask() }.at(0, 0, 1080, 1000)
        val label = TextView(ctx).apply { text = "Total: 42" }
        val secret = TextView(ctx).apply { text = "Card 4242"; trooperMask() }
        val field = EditText(ctx).apply { setText("typed") }
        val pwd = TextView(ctx).apply { text = "hunter2"; inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD }
        listOf(label, secret, field, pwd).forEach { group.addView(it, LinearLayout.LayoutParams(1080, 100)) }
        root.addView(group)
        val nodes = map(layout(root)).nodes
        assertEquals("Total: 42", nodes.byId(label)!!.text); assertFalse(nodes.byId(label)!!.masked)
        assertEquals("•••• ••••", nodes.byId(secret)!!.text)
        assertEquals("•••••", nodes.byId(field)!!.text)
        assertTrue(nodes.byId(pwd)!!.masked)
    }

    @Test fun maskAllTextFalseRecordsText() {
        val root = FrameLayout(ctx)
        val label = TextView(ctx).apply { text = "Visible" }.at(0, 0, 300, 100)
        root.addView(label)
        val n = map(layout(root), maskAll = false).nodes.byId(label)!!
        assertEquals("Visible", n.text); assertFalse(n.masked)
    }

    @Test fun skipsInvisibleTransparentAndClipped() {
        val root = FrameLayout(ctx)
        val gone = TextView(ctx).apply { text = "x"; visibility = View.GONE }.at(0, 0, 100, 100)
        val invisible = TextView(ctx).apply { text = "x"; visibility = View.INVISIBLE }.at(0, 0, 100, 100)
        val transparent = TextView(ctx).apply { text = "x"; alpha = 0f }.at(0, 0, 100, 100)
        val half = TextView(ctx).apply { text = "x"; alpha = 0.5f }.at(0, 0, 100, 100)
        val scroll = ScrollView(ctx).at(0, 300, 1080, 300)
        val inner = LinearLayout(ctx).apply { orientation = LinearLayout.VERTICAL }
        val visibleRow = TextView(ctx).apply { text = "row" }
        val partRow = TextView(ctx).apply { text = "row" }
        val hiddenRow = TextView(ctx).apply { text = "row" }
        inner.addView(visibleRow, LinearLayout.LayoutParams(1080, 200))
        inner.addView(partRow, LinearLayout.LayoutParams(1080, 200))
        inner.addView(hiddenRow, LinearLayout.LayoutParams(1080, 200))
        scroll.addView(inner, ViewGroup.LayoutParams(1080, 600))
        listOf(gone, invisible, transparent, half, scroll).forEach { root.addView(it) }
        val nodes = map(layout(root)).nodes

        assertNull(nodes.byId(gone)); assertNull(nodes.byId(invisible)); assertNull(nodes.byId(transparent))
        assertEquals(0.5f, nodes.byId(half)!!.alpha, 0.01f)
        assertEquals("list", nodes.byId(scroll)!!.kind)
        assertNull(nodes.byId(visibleRow)!!.clip)
        val clip = nodes.byId(partRow)!!.clip
        assertNotNull(clip)
        assertEquals(100f / 3f, clip!![3], 0.1f) // only 100 px of the second row is inside the 300 px viewport
        assertNull(nodes.byId(hiddenRow))
    }

    @Test fun stopsAtNodeBudget() {
        val root = LinearLayout(ctx).apply { orientation = LinearLayout.VERTICAL }
        repeat(50) { root.addView(TextView(ctx).apply { text = "row $it" }, LinearLayout.LayoutParams(1080, 10)) }
        val r = ViewMapper(true, maxNodes = 20).map(listOf(layout(root)), 3f)
        assertTrue(r.truncated)
        assertEquals(20, r.nodes.size)
    }

    @Test fun customMapperNodesAreClippedMaskedAndInheritUnmask() {
        val composeLike = object : ReplayViewMapper {
            override fun map(view: View, sink: ReplayNodeSink): Boolean {
                sink.node(1, 0f, 0f, 300f, 90f, "text", text = "Secret")
                sink.node(2, 0f, 100f, 300f, 190f, "text", text = "Public", unmask = true)
                sink.node(3, 0f, 200f, 300f, 290f, "input", text = "typed", input = true, unmask = true)
                sink.node(4, 0f, 250f, 300f, 400f, "button") // partly outside the 300 px host
                sink.node(5, 0f, 500f, 300f, 600f, "text", text = "gone") // fully outside
                return true
            }
        }
        class HostView(c: android.content.Context) : FrameLayout(c)
        val root = FrameLayout(ctx)
        val host = HostView(ctx).at(0, 0, 300, 300)
        root.addView(host)
        val nodes = ViewMapper(true, composeLike, composeViewClass = HostView::class.java.name).map(listOf(layout(root)), 3f).nodes
        val byId = nodes.associateBy { it.id xor ViewMapper.COMPOSE_ID_SALT }
        assertEquals("••••••", byId[1]!!.text)
        assertEquals("Public", byId[2]!!.text)
        assertEquals("•••••", byId[3]!!.text)
        assertEquals("input", byId[3]!!.kind)
        assertEquals(50f / 3f, byId[4]!!.clip!![3], 0.1f)
        assertNull(byId[5])

        // Unmask on the host View is inherited by mapper nodes that do not decide themselves.
        host.trooperUnmask()
        val again = ViewMapper(true, composeLike, composeViewClass = HostView::class.java.name).map(listOf(root), 3f).nodes
        assertEquals("Secret", again.first { (it.id xor ViewMapper.COMPOSE_ID_SALT) == 1 }.text)
    }
}
