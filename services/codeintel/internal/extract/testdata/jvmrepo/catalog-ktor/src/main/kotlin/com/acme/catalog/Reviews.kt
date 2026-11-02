package com.acme.catalog

import io.ktor.server.routing.*

fun Route.reviewRoutes() {
    route("/reviews") {
        get("/{productId}") { }
        post { }
    }
}
