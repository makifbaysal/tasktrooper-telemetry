package io.tasktrooper.telemetry.ktor

import io.ktor.server.application.createApplicationPlugin
import io.ktor.server.application.install
import io.ktor.server.routing.RoutingNode
import io.ktor.server.routing.RoutingRoot
import io.opentelemetry.api.OpenTelemetry
import io.opentelemetry.api.trace.Span
import io.opentelemetry.context.Context
import io.opentelemetry.instrumentation.api.instrumenter.LocalRootSpan
import io.opentelemetry.instrumentation.ktor.v3_0.KtorServerTelemetry
import io.tasktrooper.telemetry.core.RouteTemplates
import io.tasktrooper.telemetry.core.SdkErrors
import io.tasktrooper.telemetry.core.TrooperAttributes
import io.tasktrooper.telemetry.kotlin.Trooper

/** `install(TaskTrooper) { dsn = ...; service = ... }` */
class TaskTrooperConfig {
    var dsn: String? = null
    var service: String? = null
    var environment: String? = null
    var version: String? = null
    var sampleRate: Double? = null
    /** `false` disables telemetry (default: `TROOPER_ENABLED` / `-Dtrooper.enabled`, else `true`). */
    var enabled: Boolean? = null

    /** Use an existing SDK (javaagent, tests, custom setup) instead of initializing one. */
    var openTelemetry: OpenTelemetry? = null
}

/**
 * Ktor 3 server plugin: initializes the SDK with TaskTrooper defaults, installs OTel's `KtorServerTelemetry` and sets
 * `trooper.route` from the matched Ktor route as soon as routing resolves it.
 */
val TaskTrooper = createApplicationPlugin("TaskTrooper", ::TaskTrooperConfig) {
    val cfg = pluginConfig
    val otel = cfg.openTelemetry?.also { Trooper.openTelemetry = it } ?: Trooper.init {
        dsn = cfg.dsn
        service = cfg.service
        environment = cfg.environment
        version = cfg.version
        sampleRate = cfg.sampleRate
        enabled = cfg.enabled
    }
    if (cfg.openTelemetry != null) SdkErrors.bind(otel)
    application.install(KtorServerTelemetry) { setOpenTelemetry(otel) }
    application.monitor.subscribe(RoutingRoot.RoutingCallStarted) { call ->
        try {
            val route = routeTemplate(call.route) ?: return@subscribe
            val span = LocalRootSpan.fromContextOrNull(Context.current()) ?: Span.current()
            span.setAttribute(TrooperAttributes.ROUTE, route)
        } catch (t: Throwable) {
            SdkErrors.record("ktor_route", t) // never break request handling
        }
    }
}

/** `/users/{id}/(method:GET)` → `/users/:id` */
internal fun routeTemplate(node: RoutingNode): String? {
    val segments = ArrayDeque<String>()
    var current: RoutingNode? = node
    while (current != null) {
        val s = current.selector.toString()
        if (s.isNotEmpty() && !s.startsWith("(") && s != "/") {
            segments.addFirst(s.trim('/'))
        }
        current = current.parent
    }
    return RouteTemplates.normalize("/" + segments.filter { it.isNotEmpty() }.joinToString("/"))
}
