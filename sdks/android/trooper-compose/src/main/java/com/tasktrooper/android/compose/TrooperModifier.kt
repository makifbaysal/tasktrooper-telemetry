package com.tasktrooper.android.compose

import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.waitForUpOrCancellation
import androidx.compose.ui.Modifier
import androidx.compose.ui.input.pointer.PointerEventPass
import androidx.compose.ui.input.pointer.pointerInput
import com.tasktrooper.android.TaskTrooper

/** Records a RUM `ACTION` named [name] when the element is tapped. Does not consume the gesture. */
fun Modifier.trooperAction(name: String, target: String? = null): Modifier = this.pointerInput(name, target) {
    awaitEachGesture {
        awaitFirstDown(requireUnconsumed = false, pass = PointerEventPass.Initial)
        val up = waitForUpOrCancellation(pass = PointerEventPass.Initial)
        if (up != null) TaskTrooper.addAction(name, target)
    }
}
