package com.tasktrooper.android.compose

import org.junit.Assert.assertEquals
import org.junit.Test

class RouteNamesTest {
    @Test fun routeTemplates() {
        val cases = mapOf(
            "home" to "home",
            "product/{id}" to "product/:id",
            "user/{userId}/orders/{orderId}" to "user/:userId/orders/:orderId",
            "search?query={query}&page={page}" to "search",
            "com.example.app.ProductRoute/{id}" to "ProductRoute/:id",
            "com.example.app.Home" to "Home",
            "" to null,
            null to null,
        )
        for ((route, expected) in cases) assertEquals("route=$route", expected, routeToViewName(route))
    }
}
