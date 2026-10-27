package com.tasktrooper.android.replay.internal

import android.content.res.ColorStateList
import android.graphics.drawable.ColorDrawable
import android.graphics.drawable.Drawable
import android.graphics.drawable.DrawableContainer
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.InsetDrawable
import android.graphics.drawable.LayerDrawable
import android.os.Build
import android.text.InputType
import android.text.method.PasswordTransformationMethod
import android.view.View
import android.view.ViewGroup
import android.webkit.WebView
import android.widget.AbsListView
import android.widget.Button
import android.widget.CompoundButton
import android.widget.EditText
import android.widget.HorizontalScrollView
import android.widget.ImageButton
import android.widget.ImageView
import android.widget.ScrollView
import android.widget.TextView
import com.tasktrooper.android.replay.R
import com.tasktrooper.android.spi.ReplayNodeSink
import com.tasktrooper.android.spi.ReplayViewMapper
import java.lang.reflect.Method

/**
 * Turns window View hierarchies into a flat, back-to-front wireframe (contracts.md §23). Main thread only.
 * No pixels are read: text is masked (length kept), inputs and passwords always, images are boxes.
 *
 * Bounded: stops after [maxNodes] nodes or [budgetNs] of traversal (result is then `truncated`).
 * Rotation/scale transforms are ignored (translation is applied).
 */
internal class ViewMapper(
    private val maskAllText: Boolean,
    private val composeMapper: ReplayViewMapper? = null,
    private val maxNodes: Int = 1500,
    private val budgetNs: Long = 12_000_000L,
    private val nanoTime: () -> Long = System::nanoTime,
    /** Class handed to [composeMapper] (tests substitute their own). */
    private val composeViewClass: String = ReplayViewMapper.COMPOSE_VIEW,
) : ReplayNodeSink {
    class Result(val nodes: List<WireNode>, val truncated: Boolean, val costNs: Long)

    private var density = 1f
    private var nodes = ArrayList<WireNode>()
    private var started = 0L
    private var checks = 0
    private var truncated = false
    private val loc = IntArray(2)

    // Context of the View currently handed to the compose mapper (sink callbacks).
    private var curClipL = 0f; private var curClipT = 0f; private var curClipR = 0f; private var curClipB = 0f
    private var curAlpha = 1f
    private var curUnmask = false

    // Background scratch (avoids allocating per view).
    private var bgColor: Int? = null
    private var bgRadius = 0f
    private var bgBorder: Int? = null

    /** [roots] back to front (e.g. activity window, then dialogs). */
    fun map(roots: List<View>, density: Float): Result {
        this.density = if (density > 0f) density else 1f
        nodes = ArrayList(256)
        started = nanoTime(); checks = 0; truncated = false
        for (root in roots) {
            if (!hasBudget) break
            Guard.run("replay.mapRoot", Unit) {
                root.getLocationOnScreen(loc)
                val l = loc[0].toFloat(); val t = loc[1].toFloat()
                visit(root, l, t, l, t, l + root.width, t + root.height, 1f, false, 0)
            }
        }
        return Result(nodes, truncated, nanoTime() - started)
    }

    override val hasBudget: Boolean
        get() {
            if (truncated) return false
            if (nodes.size >= maxNodes) { truncated = true; return false }
            if ((++checks and 31) == 0 && nanoTime() - started > budgetNs) { truncated = true; return false }
            return true
        }

    private fun visit(
        v: View, left: Float, top: Float, cl: Float, ct: Float, cr: Float, cb: Float,
        parentAlpha: Float, parentUnmask: Boolean, depth: Int,
    ) {
        if (depth > MAX_DEPTH || !hasBudget) return
        if (v.visibility != View.VISIBLE) return
        val alpha = parentAlpha * v.alpha
        if (alpha <= 0.01f) return
        val w = v.width; val h = v.height
        if (w <= 0 || h <= 0) return
        val right = left + w; val bottom = top + h
        val vl = maxOf(left, cl); val vt = maxOf(top, ct); val vr = minOf(right, cr); val vb = minOf(bottom, cb)
        if (vr <= vl || vb <= vt) return
        val unmask = when (v.getTag(R.id.trooper_unmask)) { true -> true; false -> false; else -> parentUnmask }

        emitView(v, left, top, right, bottom, vl, vt, vr, vb, alpha, unmask)
        if (v is WebView) return // content is a black box (never recorded)

        val cm = composeMapper
        if (cm != null && v.javaClass.name == composeViewClass) {
            curClipL = vl; curClipT = vt; curClipR = vr; curClipB = vb; curAlpha = alpha; curUnmask = unmask
            Guard.run("replay.compose", false) { cm.map(v, this) }
        }
        if (v !is ViewGroup) return
        val clip = v.clipChildren
        val nl = if (clip) vl else cl; val nt = if (clip) vt else ct
        val nr = if (clip) vr else cr; val nb = if (clip) vb else cb
        val ox = left - v.scrollX; val oy = top - v.scrollY
        val n = v.childCount
        var hasZ = false
        for (i in 0 until n) if (v.getChildAt(i)?.z?.let { it != 0f } == true) { hasZ = true; break }
        if (!hasZ) {
            for (i in 0 until n) {
                val c = v.getChildAt(i) ?: continue
                visit(c, ox + c.left + c.translationX, oy + c.top + c.translationY, nl, nt, nr, nb, alpha, unmask, depth + 1)
            }
        } else {
            // Elevation draws above siblings (stable for equal z), like ViewGroup's preorder list.
            val ordered = (0 until n).mapNotNull { v.getChildAt(it) }.sortedBy { it.z }
            for (c in ordered) {
                visit(c, ox + c.left + c.translationX, oy + c.top + c.translationY, nl, nt, nr, nb, alpha, unmask, depth + 1)
            }
        }
    }

    private fun emitView(
        v: View, left: Float, top: Float, right: Float, bottom: Float,
        vl: Float, vt: Float, vr: Float, vb: Float, alpha: Float, unmask: Boolean,
    ) {
        readBackground(v)
        var kind: String
        var text: CharSequence? = null
        var input = false
        var password = false
        var fg: Int? = null
        when (v) {
            is WebView -> kind = "webview"
            is EditText -> {
                kind = "input"
                fg = v.currentTextColor
                val t = v.text
                if (!t.isNullOrEmpty()) { text = t; input = true } else text = v.hint
            }
            is TextView -> {
                kind = when (v) { is CompoundButton -> "switch"; is Button -> "button"; else -> "text" }
                fg = v.currentTextColor
                text = v.text
                password = isPassword(v)
                if (kind == "text" && text.isNullOrEmpty() && bgColor == null) return
            }
            is ImageButton -> kind = "button"
            is ImageView -> {
                val small = (right - left) / density <= ICON_MAX_DP && (bottom - top) / density <= ICON_MAX_DP
                kind = if (small) "icon" else "image"
            }
            else -> {
                kind = if (isList(v)) "list" else "view"
                // Plain containers without a visible background add nothing to the wireframe.
                if (kind == "view" && bgColor == null && bgBorder == null) return
            }
        }
        val (shown, masked) = Masking.apply(text, input, password, maskAllText, unmask)
        add(System.identityHashCode(v), left, top, right, bottom, vl, vt, vr, vb, kind, bgColor, fg, bgRadius, bgBorder, shown, masked, alpha)
    }

    private fun add(
        id: Int, left: Float, top: Float, right: Float, bottom: Float, vl: Float, vt: Float, vr: Float, vb: Float,
        kind: String, bg: Int?, fg: Int?, radiusPx: Float, border: Int?, text: String?, masked: Boolean, alpha: Float,
    ) {
        val d = density
        val clipped = vl > left + 0.5f || vt > top + 0.5f || vr < right - 0.5f || vb < bottom - 0.5f
        nodes.add(WireNode(
            id = id, x = left / d, y = top / d, w = (right - left) / d, h = (bottom - top) / d, kind = kind,
            bg = bg?.takeUnless { Colors.isTransparent(it) }, fg = fg, radius = radiusPx / d, border = border,
            text = text, masked = masked, alpha = alpha,
            clip = if (clipped) floatArrayOf(vl / d, vt / d, (vr - vl) / d, (vb - vt) / d) else null,
        ))
    }

    // ------------------------------------------------------------------ compose / custom mapper sink

    override fun node(
        id: Int, left: Float, top: Float, right: Float, bottom: Float, kind: String, text: String?, input: Boolean,
        unmask: Boolean?, bg: Int?, fg: Int?, radiusPx: Float,
    ) {
        if (!hasBudget || right <= left || bottom <= top) return
        val vl = maxOf(left, curClipL); val vt = maxOf(top, curClipT); val vr = minOf(right, curClipR); val vb = minOf(bottom, curClipB)
        if (vr <= vl || vb <= vt) return
        val (shown, masked) = Masking.apply(text, input, false, maskAllText, unmask ?: curUnmask)
        // Offset keeps semantics ids apart from View identity hashes.
        add(id xor COMPOSE_ID_SALT, left, top, right, bottom, vl, vt, vr, vb, kind, bg, fg, radiusPx, null, shown, masked, curAlpha)
    }

    // ------------------------------------------------------------------ helpers

    private fun isPassword(v: TextView): Boolean {
        if (v.transformationMethod is PasswordTransformationMethod) return true
        val type = v.inputType
        val variation = type and InputType.TYPE_MASK_VARIATION
        return when (type and InputType.TYPE_MASK_CLASS) {
            InputType.TYPE_CLASS_TEXT -> variation == InputType.TYPE_TEXT_VARIATION_PASSWORD ||
                variation == InputType.TYPE_TEXT_VARIATION_WEB_PASSWORD || variation == InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD
            InputType.TYPE_CLASS_NUMBER -> variation == InputType.TYPE_NUMBER_VARIATION_PASSWORD
            else -> false
        }
    }

    private fun isList(v: View): Boolean {
        if (v is AbsListView || v is ScrollView || v is HorizontalScrollView) return true
        return listClasses.getOrPut(v.javaClass) { classChain(v.javaClass).any { it in LIST_CLASS_NAMES } }
    }

    /** Fills [bgColor] / [bgRadius] / [bgBorder] from the View's background (best effort, never throws). */
    private fun readBackground(v: View) {
        bgColor = null; bgRadius = 0f; bgBorder = null
        Guard.run("replay.bg", Unit) {
            if (!readDrawable(v.background, v, 0)) readCardView(v)
        }
    }

    private fun readDrawable(d: Drawable?, v: View, depth: Int): Boolean {
        if (d == null || depth > 4) return false
        when {
            d is ColorDrawable -> { bgColor = d.color; return true }
            d is GradientDrawable -> {
                if (Build.VERSION.SDK_INT >= 24) {
                    bgColor = d.color?.let { stateColor(it, v) }?.let { Colors.withAlpha(it, d.alpha / 255f) }
                    bgRadius = d.cornerRadii?.firstOrNull() ?: d.cornerRadius
                }
                return bgColor != null
            }
            d is LayerDrawable -> { // includes RippleDrawable
                for (i in 0 until d.numberOfLayers) {
                    if (d.getId(i) == android.R.id.mask) continue
                    if (readDrawable(d.getDrawable(i), v, depth + 1)) return true
                }
                return false
            }
            d is InsetDrawable -> return readDrawable(d.drawable, v, depth + 1)
            d is DrawableContainer -> return readDrawable(d.current, v, depth + 1)
            Build.VERSION.SDK_INT >= 23 && d is android.graphics.drawable.DrawableWrapper -> return readDrawable(d.drawable, v, depth + 1)
        }
        if (isMaterialShape(d.javaClass)) {
            val fill = invoke(d, "getFillColor") as? ColorStateList
            bgColor = fill?.let { Colors.withAlpha(stateColor(it, v), d.alpha / 255f) }
            bgRadius = (invoke(d, "getTopLeftCornerResolvedSize") as? Float) ?: 0f
            val strokeW = (invoke(d, "getStrokeWidth") as? Float) ?: 0f
            if (strokeW > 0f) bgBorder = (invoke(d, "getStrokeColor") as? ColorStateList)?.let { stateColor(it, v) }
            return bgColor != null || bgBorder != null
        }
        return false
    }

    private fun readCardView(v: View) {
        if (!cardClasses.getOrPut(v.javaClass) { classChain(v.javaClass).any { it == CARD_VIEW } }) return
        bgColor = (invoke(v, "getCardBackgroundColor") as? ColorStateList)?.let { stateColor(it, v) }
        bgRadius = (invoke(v, "getRadius") as? Float) ?: 0f
    }

    private fun stateColor(csl: ColorStateList, v: View): Int = csl.getColorForState(v.drawableState, csl.defaultColor)

    private fun isMaterialShape(c: Class<*>): Boolean = shapeClasses.getOrPut(c) { classChain(c).any { it == MATERIAL_SHAPE } }

    private fun invoke(target: Any, name: String): Any? {
        val key = target.javaClass.name + "#" + name
        val m = if (methods.containsKey(key)) methods[key] else {
            val found = try { target.javaClass.getMethod(name).apply { isAccessible = true } } catch (_: Throwable) { null }
            methods[key] = found; found
        }
        return try { m?.invoke(target) } catch (_: Throwable) { null }
    }

    private fun classChain(c: Class<*>): Sequence<String> = generateSequence(c) { it.superclass }.map { it.name }

    companion object {
        const val MAX_DEPTH = 96
        const val ICON_MAX_DP = 48f
        const val COMPOSE_ID_SALT = 0x5a000000
        private const val MATERIAL_SHAPE = "com.google.android.material.shape.MaterialShapeDrawable"
        private const val CARD_VIEW = "androidx.cardview.widget.CardView"
        private val LIST_CLASS_NAMES = setOf(
            "androidx.recyclerview.widget.RecyclerView", "androidx.core.widget.NestedScrollView",
            "androidx.viewpager2.widget.ViewPager2", "androidx.viewpager.widget.ViewPager",
        )
        // Class-level caches (main thread only).
        private val listClasses = HashMap<Class<*>, Boolean>()
        private val cardClasses = HashMap<Class<*>, Boolean>()
        private val shapeClasses = HashMap<Class<*>, Boolean>()
        private val methods = HashMap<String, Method?>()
    }
}
