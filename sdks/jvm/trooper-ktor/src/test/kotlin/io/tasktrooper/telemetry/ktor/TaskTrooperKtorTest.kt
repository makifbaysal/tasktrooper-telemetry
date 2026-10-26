package io.tasktrooper.telemetry.ktor

import io.ktor.client.request.get
import io.ktor.client.request.header
import io.ktor.client.statement.bodyAsText
import io.ktor.server.application.install
import io.ktor.server.response.respondText
import io.ktor.server.routing.get
import io.ktor.server.routing.route
import io.ktor.server.routing.routing
import io.ktor.server.testing.testApplication
import io.opentelemetry.api.trace.SpanKind
import io.opentelemetry.api.trace.propagation.W3CTraceContextPropagator
import io.opentelemetry.context.propagation.ContextPropagators
import io.opentelemetry.context.propagation.TextMapPropagator
import io.opentelemetry.sdk.OpenTelemetrySdk
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter
import io.opentelemetry.sdk.trace.SdkTracerProvider
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor
import io.tasktrooper.telemetry.core.TrooperAttributes
import io.tasktrooper.telemetry.core.TrooperBaggagePropagator
import io.tasktrooper.telemetry.core.TrooperSampler
import io.tasktrooper.telemetry.core.TrooperSpanProcessor
import io.tasktrooper.telemetry.kotlin.withSpan
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.Test

class TaskTrooperKtorTest {
    private val exporter = InMemorySpanExporter.create()
    private val sdk = OpenTelemetrySdk.builder()
        .setTracerProvider(
            SdkTracerProvider.builder()
                .setSampler(TrooperSampler.create(1.0))
                .addSpanProcessor(TrooperSpanProcessor())
                .addSpanProcessor(SimpleSpanProcessor.create(exporter))
                .build(),
        )
        .setPropagators(
            ContextPropagators.create(
                TextMapPropagator.composite(W3CTraceContextPropagator.getInstance(), TrooperBaggagePropagator.create("users-api")),
            ),
        )
        .build()

    @Test
    fun `route template, entry route through coroutines and caller`() = testApplication {
        application {
            install(TaskTrooper) { openTelemetry = sdk }
            routing {
                route("/v1") {
                    get("/users/{id}") {
                        withContext(Dispatchers.IO) { withSpan("load-user") { } }
                        call.respondText("ok")
                    }
                }
            }
        }
        val res = client.get("/v1/users/42") { header("baggage", "tt.svc=gateway") }
        assertThat(res.bodyAsText()).isEqualTo("ok")

        val server = exporter.finishedSpanItems.first { it.kind == SpanKind.SERVER }
        assertThat(server.attributes.get(TrooperAttributes.ROUTE)).isEqualTo("/v1/users/:id")
        assertThat(server.attributes.get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/users/:id")
        assertThat(server.attributes.get(TrooperAttributes.CALLER)).isEqualTo("gateway")
        assertThat(server.attributes.get(TrooperAttributes.SAMPLE_RATE)).isEqualTo(1.0)
        val child = exporter.finishedSpanItems.first { it.name == "load-user" }
        assertThat(child.parentSpanId).isEqualTo(server.spanId)
        assertThat(child.attributes.get(TrooperAttributes.ENTRY_ROUTE)).isEqualTo("/v1/users/:id")
    }

    @Test
    fun `client plugin sends service baggage`() = testApplication {
        application {
            routing { get("/echo") { call.respondText(call.request.headers["baggage"] ?: "") } }
        }
        val http = createClient { install(TaskTrooperClient) { service = "users-api" } }
        assertThat(http.get("/echo").bodyAsText()).isEqualTo("tt.svc=users-api")
    }
}
