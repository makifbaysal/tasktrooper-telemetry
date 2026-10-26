package com.tasktrooper.android.internal

import android.app.Activity
import androidx.fragment.app.Fragment
import androidx.fragment.app.FragmentActivity
import androidx.fragment.app.FragmentManager

/**
 * Fragment views. androidx.fragment is a compileOnly dependency: this class is only loaded after
 * [ActivityTracker.FRAGMENTS_AVAILABLE] confirmed the classes exist at runtime.
 */
internal class FragmentTracker(private val core: RumCore) : FragmentManager.FragmentLifecycleCallbacks() {

    override fun onFragmentResumed(fm: FragmentManager, f: Fragment) {
        Internal.guard("fragment.resumed", Unit) { if (isTracked(f)) core.startView(viewName(f), f) }
    }

    override fun onFragmentPaused(fm: FragmentManager, f: Fragment) {
        Internal.guard("fragment.paused", Unit) { if (isTracked(f)) core.stopView(f) }
    }

    companion object {
        private val IGNORED = setOf(
            "androidx.navigation.fragment.NavHostFragment",
            "com.bumptech.glide.manager.SupportRequestManagerFragment",
        )

        fun register(activity: Activity, core: RumCore): Boolean {
            val fa = activity as? FragmentActivity ?: return false
            fa.supportFragmentManager.registerFragmentLifecycleCallbacks(FragmentTracker(core), true)
            return true
        }

        /** Container/host fragments and dialogs are not screens. */
        fun isTracked(f: Fragment): Boolean =
            f.javaClass.name !in IGNORED && f !is androidx.fragment.app.DialogFragment && f.isVisible

        fun viewName(f: Fragment): String = f.javaClass.simpleName.ifEmpty { f.javaClass.name }
    }
}
