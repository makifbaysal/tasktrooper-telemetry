package io.tasktrooper.examples.ktor

import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.request.get
import io.ktor.client.statement.bodyAsText
import io.ktor.server.application.install
import io.ktor.server.engine.embeddedServer
import io.ktor.server.netty.Netty
import io.ktor.server.response.respondText
import io.ktor.server.routing.get
import io.ktor.server.routing.routing
import io.opentelemetry.instrumentation.ktor.v3_0.KtorClientTelemetry
import io.tasktrooper.telemetry.kotlin.Trooper
import io.tasktrooper.telemetry.kotlin.trooper
import io.tasktrooper.telemetry.kotlin.withSpan
import io.tasktrooper.telemetry.ktor.TaskTrooper
import io.tasktrooper.telemetry.ktor.TaskTrooperClient

fun main() {
    embeddedServer(Netty, port = 8080) {
        install(TaskTrooper) {
            dsn = System.getenv("TROOPER_DSN") ?: "http://tt_dev_ingest_key@localhost:4318/1"
            service = "users-api"
            environment = "dev"
        }
        val http = HttpClient(CIO) {
            install(KtorClientTelemetry) { setOpenTelemetry(Trooper.openTelemetry) }
            install(TaskTrooperClient) { service = "users-api" }
        }
        routing {
            get("/v1/users/{id}") {
                val profile = withSpan("load-profile") {
                    runCatching { http.get("http://localhost:8080/health").bodyAsText() }
                        .onFailure { trooper.captureError(it) }
                        .getOrDefault("?")
                }
                call.respondText("user ${call.parameters["id"]} ($profile)")
            }
            get("/health") { call.respondText("ok") }
        }
    }.start(wait = true)
}
