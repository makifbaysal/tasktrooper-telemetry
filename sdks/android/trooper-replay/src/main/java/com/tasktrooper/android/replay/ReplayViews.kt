@file:JvmName("TrooperReplayViews")

package com.tasktrooper.android.replay

import android.view.View

/**
 * Records the text of this View and its subtree unmasked in session replay. Inputs and password fields
 * stay masked. Same as `view.setTag(R.id.trooper_unmask, true)`.
 */
@JvmOverloads
fun View.trooperUnmask(unmask: Boolean = true) {
    setTag(R.id.trooper_unmask, unmask)
}

/** Forces masking for this subtree (e.g. inside an unmasked parent). */
fun View.trooperMask() {
    setTag(R.id.trooper_unmask, false)
}
