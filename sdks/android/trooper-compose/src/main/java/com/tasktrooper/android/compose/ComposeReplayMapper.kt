package com.tasktrooper.android.compose

import android.view.View
import androidx.compose.ui.Modifier
import androidx.compose.ui.node.RootForTest
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.SemanticsActions
import androidx.compose.ui.semantics.SemanticsNode
import androidx.compose.ui.semantics.SemanticsProperties
import androidx.compose.ui.semantics.SemanticsPropertyKey
import androidx.compose.ui.semantics.getOrNull
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.state.ToggleableState
import com.tasktrooper.android.spi.ReplayNodeSink
import com.tasktrooper.android.spi.ReplayViewMapper

/** Semantics flag read by session replay: true = record text in this subtree unmasked, false = force masking. */
val TrooperUnmaskKey = SemanticsPropertyKey<Boolean>("TrooperUnmask", mergePolicy = { parent, _ -> parent })

/**
 * Records the text of this composable and its children unmasked in session replay (`trooper-replay`).
 * Text fields and password fields stay masked.
 */
fun Modifier.trooperUnmask(unmask: Boolean = true): Modifier = this.semantics { this[TrooperUnmaskKey] = unmask }


/**
 * Session replay wireframe for Jetpack Compose (loaded reflectively by `trooper-replay`): walks the
 * unmerged semantics tree of an `AndroidComposeView` (public `RootForTest.semanticsOwner`) and emits nodes
 * with window bounds, role-derived kind and text. Colors are not available from semantics.
 */
class ComposeReplayMapper : ReplayViewMapper {
    private val onScreen = IntArray(2)
    private val inWindow = IntArray(2)

    override fun map(view: View, sink: ReplayNodeSink): Boolean {
        val owner = (view as? RootForTest)?.semanticsOwner ?: return false
        view.getLocationOnScreen(onScreen)
        view.getLocationInWindow(inWindow)
        val dx = (onScreen[0] - inWindow[0]).toFloat()
        val dy = (onScreen[1] - inWindow[1]).toFloat()
        val density = view.resources.displayMetrics.density.takeIf { it > 0f } ?: 1f
        walk(owner.unmergedRootSemanticsNode, sink, dx, dy, density, null, 0)
        return true
    }

    private fun walk(node: SemanticsNode, sink: ReplayNodeSink, dx: Float, dy: Float, density: Float, inherited: Boolean?, depth: Int) {
        if (depth > MAX_DEPTH || !sink.hasBudget) return
        val c = node.config
        val unmask = c.getOrNull(TrooperUnmaskKey) ?: inherited
        val b = node.boundsInWindow
        if (b.width > 0f && b.height > 0f) {
            val editable = c.getOrNull(SemanticsProperties.EditableText)
            val isInput = editable != null || c.getOrNull(SemanticsActions.SetText) != null
            val text = c.getOrNull(SemanticsProperties.Text)
            val kind = replayKind(
                role = c.getOrNull(SemanticsProperties.Role),
                isInput = isInput,
                hasText = !text.isNullOrEmpty(),
                isScrollable = c.getOrNull(SemanticsProperties.VerticalScrollAxisRange) != null ||
                    c.getOrNull(SemanticsProperties.HorizontalScrollAxisRange) != null ||
                    c.getOrNull(SemanticsProperties.CollectionInfo) != null,
                hasDescription = !c.getOrNull(SemanticsProperties.ContentDescription).isNullOrEmpty(),
                widthDp = b.width / density, heightDp = b.height / density,
            )
            if (kind != null) {
                val shown: String? = when {
                    isInput -> editable?.text
                    text != null -> text.joinToString(" ") { it.text }
                    else -> null
                }
                sink.node(
                    id = node.id, left = b.left + dx, top = b.top + dy, right = b.right + dx, bottom = b.bottom + dy,
                    kind = kind, text = shown,
                    input = isInput || c.getOrNull(SemanticsProperties.Password) != null, unmask = unmask,
                    checked = if (kind == "switch") {
                        replayChecked(c.getOrNull(SemanticsProperties.ToggleableState), c.getOrNull(SemanticsProperties.Selected))
                    } else null,
                )
            }
        }
        for (child in node.children) walk(child, sink, dx, dy, density, unmask, depth + 1)
    }

    private companion object {
        const val MAX_DEPTH = 96
    }
}

/** contracts.md §23 node kind for a semantics node; null = not drawn (children are still visited). */
internal fun replayKind(
    role: Role?, isInput: Boolean, hasText: Boolean, isScrollable: Boolean, hasDescription: Boolean,
    widthDp: Float, heightDp: Float,
): String? = when {
    isInput -> "input"
    role == Role.Button || role == Role.Tab || role == Role.DropdownList -> "button"
    role == Role.Checkbox || role == Role.Switch || role == Role.RadioButton -> "switch"
    role == Role.Image || (hasDescription && !hasText) -> if (widthDp <= 48f && heightDp <= 48f) "icon" else "image"
    isScrollable -> "list"
    hasText -> "text"
    else -> null
}

/** `checked` for a `switch` node: toggleable state (switch/checkbox; indeterminate = off) or selection (radio). */
internal fun replayChecked(state: ToggleableState?, selected: Boolean?): Boolean? = when (state) {
    ToggleableState.On -> true
    ToggleableState.Off, ToggleableState.Indeterminate -> false
    null -> selected
}
