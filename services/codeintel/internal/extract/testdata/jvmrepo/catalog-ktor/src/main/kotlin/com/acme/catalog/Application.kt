package com.acme.catalog

import io.ktor.server.application.*
import io.ktor.server.response.*
import io.ktor.server.routing.*
import redis.clients.jedis.JedisPool

const val VERSION = "v1"

fun Application.module() {
    val greeting = "get(\"/in-string\") { }"
    routing {
        get("/health") { call.respondText("ok") }
        route("/$VERSION") {
            route("/products") {
                get {
                    call.respond(listOf<String>())
                }
                get("{id}") { call.respondText("${call.parameters["id"]}") }
                post { }
                delete("/{id}") { }
            }
            reviewRoutes()
        }
        // get("/commented") { }
    }
}
