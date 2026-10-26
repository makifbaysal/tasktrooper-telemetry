package io.tasktrooper.telemetry.kotlin

import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.OpenTelemetry
import io.opentelemetry.api.common.AttributeKey
import io.opentelemetry.api.common.Attributes
import io.opentelemetry.api.trace.Span
import io.opentelemetry.api.trace.SpanKind
import io.opentelemetry.api.trace.StatusCode
import io.opentelemetry.context.Context
import io.opentelemetry.extension.kotlin.asContextElement
import io.tasktrooper.telemetry.core.SdkErrors
import io.tasktrooper.telemetry.core.TrooperConfig
import io.tasktrooper.telemetry.core.TrooperSdk
import kotlinx.coroutines.withContext
import kotlin.coroutines.CoroutineContext

/** Options for [Trooper.init]; unset values fall back to `TROOPER_*` env vars / `-Dtrooper.*`. */
class TrooperOptions {
    var dsn: String? = null
    var service: String? = null
    var environment: String? = null
    var version: String? = null
    var sampleRate: Double? = null
    var enabled: Boolean? = null

    internal fun toConfig(): TrooperConfig {
        val system = TrooperConfig.fromSystem()
        return TrooperConfig.builder()
            .apply { if (dsn != null) dsn(dsn) else dsn(system.dsn()) }
            .service(service ?: system.service())
            .environment(environment ?: system.environment())
            .version(version ?: system.version())
            .sampleRate(sampleRate ?: system.sampleRate())
            .enabled(enabled ?: system.enabled())
            .build()
    }
}

/** Kotlin-friendly entry point. */
object Trooper {
    private const val INSTRUMENTATION = "io.tasktrooper.telemetry.kotlin"

    @Volatile
    private var otel: OpenTelemetry? = null

    /** The SDK set by [init], else the global one (e.g. the javaagent's). */
    var openTelemetry: OpenTelemetry
        get() = otel ?: GlobalOpenTelemetry.get()
        set(value) {
            otel = value
        }

    /** Initializes the OTel SDK with TaskTrooper defaults and registers it globally. Never throws. */
    fun init(block: TrooperOptions.() -> Unit = {}): OpenTelemetry {
        val sdk = try {
            TrooperSdk.init(TrooperOptions().apply(block).toConfig())
        } catch (t: Throwable) {
            SdkErrors.record("init", t)
            OpenTelemetry.noop()
        }
        otel = sdk
        return sdk
    }

    /** Records [error] on the current span (or a short `exception` span if there is none) and marks it as failed. */
    fun captureError(error: Throwable, attributes: Map<String, String> = emptyMap()) {
        try {
            val attrs = Attributes.builder().apply { attributes.forEach { (k, v) -> put(AttributeKey.stringKey(k), v) } }.build()
            val current = Span.current()
            if (current.spanContext.isValid && current.isRecording) {
                current.recordException(error, attrs)
                current.setStatus(StatusCode.ERROR, error.message ?: error.javaClass.simpleName)
            } else {
                val span = openTelemetry.getTracer(INSTRUMENTATION).spanBuilder("exception").startSpan()
                span.recordException(error, attrs)
                span.setStatus(StatusCode.ERROR, error.message ?: error.javaClass.simpleName)
                span.end()
            }
        } catch (t: Throwable) {
            SdkErrors.record("capture_error", t) // never break the host
        }
    }

    /** Coroutine context element carrying the current OTel context (use with `launch`/`async` on other dispatchers). */
    fun contextElement(context: Context = Context.current()): CoroutineContext = context.asContextElement()
}

/** `trooper.captureError(e)` style access. */
val trooper: Trooper get() = Trooper

/**
 * Runs [block] inside a new span that stays current across suspension points. Exceptions are recorded and rethrown.
 */
suspend fun <T> withSpan(
    name: String,
    kind: SpanKind = SpanKind.INTERNAL,
    attributes: Attributes = Attributes.empty(),
    block: suspend (Span) -> T,
): T {
    val span = Trooper.openTelemetry.getTracer("io.tasktrooper.telemetry.kotlin")
        .spanBuilder(name).setSpanKind(kind).setAllAttributes(attributes).startSpan()
    return try {
        withContext(Context.current().with(span).asContextElement()) { block(span) }
    } catch (t: Throwable) {
        span.recordException(t)
        span.setStatus(StatusCode.ERROR)
        throw t
    } finally {
        span.end()
    }
}
