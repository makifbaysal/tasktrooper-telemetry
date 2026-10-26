package io.tasktrooper.telemetry.ktor

import io.ktor.client.plugins.api.createClientPlugin
import io.opentelemetry.api.baggage.propagation.W3CBaggagePropagator
import io.opentelemetry.context.Context
import io.opentelemetry.context.propagation.TextMapSetter
import io.ktor.http.HeadersBuilder
import io.tasktrooper.telemetry.core.SdkErrors
import io.tasktrooper.telemetry.core.TrooperBaggagePropagator

class TaskTrooperClientConfig {
    /** This service's name, sent as W3C baggage `tt.svc` so the callee records `trooper.caller`. */
    var service: String? = null
}

private object HeadersSetter : TextMapSetter<HeadersBuilder> {
    override fun set(carrier: HeadersBuilder?, key: String, value: String) {
        carrier?.set(key, value)
    }
}

/**
 * Ktor client plugin adding the `baggage` header with `tt.svc=<service>` (plus any current baggage). For client spans
 * and `traceparent`, also install OTel's `KtorClientTelemetry`.
 */
val TaskTrooperClient = createClientPlugin("TaskTrooperClient", ::TaskTrooperClientConfig) {
    val service = pluginConfig.service
    val propagator = if (service.isNullOrBlank()) W3CBaggagePropagator.getInstance() else TrooperBaggagePropagator.create(service)
    onRequest { request, _ ->
        try {
            propagator.inject(Context.current(), request.headers, HeadersSetter)
        } catch (t: Throwable) {
            SdkErrors.record("ktor_client", t) // never break the call
        }
    }
}
