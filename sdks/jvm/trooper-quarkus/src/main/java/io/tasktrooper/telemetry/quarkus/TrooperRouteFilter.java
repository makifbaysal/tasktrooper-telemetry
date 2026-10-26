package io.tasktrooper.telemetry.quarkus;

import io.opentelemetry.api.trace.Span;
import io.tasktrooper.telemetry.core.RouteTemplates;
import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperAttributes;
import jakarta.ws.rs.Path;
import jakarta.ws.rs.container.ContainerRequestContext;
import jakarta.ws.rs.container.ContainerRequestFilter;
import jakarta.ws.rs.container.ResourceInfo;
import jakarta.ws.rs.core.Context;
import jakarta.ws.rs.ext.Provider;
import java.lang.reflect.Method;
import java.net.URI;

/**
 * Sets {@code trooper.route} on the server span from the matched JAX-RS {@code @Path} template (base path + class +
 * method), so spans created by the resource method carry {@code trooper.entry.route}.
 */
@Provider
public class TrooperRouteFilter implements ContainerRequestFilter {
    @Context
    ResourceInfo resourceInfo;

    @Override
    public void filter(ContainerRequestContext request) {
        try {
            if (resourceInfo == null || resourceInfo.getResourceMethod() == null) {
                return;
            }
            URI base = request.getUriInfo() == null ? null : request.getUriInfo().getBaseUri();
            String route = template(base == null ? null : base.getPath(), resourceInfo.getResourceClass(), resourceInfo.getResourceMethod());
            if (route != null) {
                Span.current().setAttribute(TrooperAttributes.ROUTE, route);
            }
        } catch (Throwable t) {
            SdkErrors.record("quarkus_route", t); // never break request handling
        }
    }

    static String template(String basePath, Class<?> resourceClass, Method method) {
        Path classPath = resourceClass == null ? null : resourceClass.getAnnotation(Path.class);
        Path methodPath = method.getAnnotation(Path.class);
        String path = RouteTemplates.join(classPath == null ? "" : classPath.value(), methodPath == null ? "" : methodPath.value());
        String base = basePath == null || basePath.equals("/") ? "" : basePath;
        return base.isEmpty() ? path : RouteTemplates.join(base, path);
    }
}
