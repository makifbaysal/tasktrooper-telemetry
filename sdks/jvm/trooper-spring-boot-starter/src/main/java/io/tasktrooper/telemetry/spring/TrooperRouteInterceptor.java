package io.tasktrooper.telemetry.spring;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.context.Context;
import io.opentelemetry.instrumentation.api.instrumenter.LocalRootSpan;
import io.tasktrooper.telemetry.core.RouteTemplates;
import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperAttributes;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import org.springframework.web.servlet.HandlerInterceptor;
import org.springframework.web.servlet.HandlerMapping;

/**
 * Sets {@code trooper.route} on the server span as soon as Spring MVC has matched the handler, so spans started inside
 * the controller already carry {@code trooper.entry.route}.
 */
public class TrooperRouteInterceptor implements HandlerInterceptor {
    @Override
    public boolean preHandle(HttpServletRequest request, HttpServletResponse response, Object handler) {
        try {
            Object pattern = request.getAttribute(HandlerMapping.BEST_MATCHING_PATTERN_ATTRIBUTE);
            if (pattern != null) {
                Span root = LocalRootSpan.fromContextOrNull(Context.current());
                if (root == null) {
                    root = Span.current();
                }
                String route = RouteTemplates.normalize(pattern.toString());
                if (route != null) {
                    root.setAttribute(TrooperAttributes.ROUTE, route);
                }
            }
        } catch (Throwable t) {
            SdkErrors.record("spring_route", t); // never break request handling
        }
        return true;
    }
}
