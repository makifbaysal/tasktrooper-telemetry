package com.tasktrooper.android.compose

import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.navigation.NavController
import androidx.navigation.NavDestination
import com.tasktrooper.android.TaskTrooper

/**
 * Tracks Navigation destinations as RUM views. The view name is the route *template*
 * (`product/{id}` → `product/:id`), never the concrete arguments, to keep cardinality low.
 */
@Composable
fun TrooperNavigationTracking(navController: NavController) {
    DisposableEffect(navController) {
        val listener = TrooperDestinationListener()
        navController.addOnDestinationChangedListener(listener)
        onDispose {
            navController.removeOnDestinationChangedListener(listener)
            TaskTrooper.stopView(listener.current)
        }
    }
}

/** Non-Compose variant: `navController.addOnDestinationChangedListener(TrooperDestinationListener())`. */
class TrooperDestinationListener : NavController.OnDestinationChangedListener {
    var current: String? = null
        private set

    override fun onDestinationChanged(controller: NavController, destination: NavDestination, arguments: android.os.Bundle?) {
        try {
            val name = routeToViewName(destination.route) ?: destination.label?.toString() ?: return
            if (name == current) return
            current = name
            TaskTrooper.startView(name)
        } catch (_: Throwable) {
        }
    }
}
