package io.tasktrooper.telemetry.kotlin

import io.opentelemetry.api.trace.StatusCode
import io.opentelemetry.sdk.OpenTelemetrySdk
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter
import io.opentelemetry.sdk.trace.SdkTracerProvider
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor
import io.tasktrooper.telemetry.core.TrooperSpanProcessor
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.Test

class TrooperKotlinTest {
    private val exporter = InMemorySpanExporter.create()

    init {
        Trooper.openTelemetry = OpenTelemetrySdk.builder()
            .setTracerProvider(
                SdkTracerProvider.builder()
                    .addSpanProcessor(TrooperSpanProcessor())
                    .addSpanProcessor(SimpleSpanProcessor.create(exporter))
                    .build(),
            ).build()
    }

    @Test
    fun `withSpan keeps context across dispatchers`(): Unit = runBlocking {
        withSpan("parent") { parent ->
            withContext(Dispatchers.IO) {
                delay(5)
                withSpan("child") { }
            }
            parent.setAttribute("done", true)
        }
        val spans = exporter.finishedSpanItems
        val parent = spans.first { it.name == "parent" }
        val child = spans.first { it.name == "child" }
        assertThat(child.parentSpanId).isEqualTo(parent.spanId)
        assertThat(child.traceId).isEqualTo(parent.traceId)
    }

    @Test
    fun `captureError marks the current span`(): Unit = runBlocking {
        val result = runCatching {
            withSpan("job") { trooper.captureError(IllegalStateException("boom")) }
        }
        assertThat(result.isSuccess).isTrue()
        val job = exporter.finishedSpanItems.first { it.name == "job" }
        assertThat(job.status.statusCode).isEqualTo(StatusCode.ERROR)
        assertThat(job.events.map { it.name }).contains("exception")
    }

    @Test
    fun `captureError without span creates one`() {
        Trooper.captureError(RuntimeException("x"))
        assertThat(exporter.finishedSpanItems.map { it.name }).contains("exception")
    }
}
