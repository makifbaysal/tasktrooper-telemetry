package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.baggage.Baggage;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.trace.ReadWriteSpan;
import io.opentelemetry.sdk.trace.ReadableSpan;
import io.opentelemetry.sdk.trace.internal.ExtendedSpanProcessor;
import io.tasktrooper.telemetry.core.profiling.ProfileRoutes;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

/**
 * Adds the {@code trooper.*} attributes (contracts.md §4):
 *
 * <ul>
 *   <li>{@code trooper.route} on server spans, from {@code http.route} (normalized, {@code {id}} → {@code :id}), or
 *       the full gRPC method name ({@code /pkg.Service/Method})</li>
 *   <li>{@code trooper.entry.route} + {@code trooper.entry.method} on every span of a request: the entry (local root)
 *       span's route and HTTP method ({@code GRPC} for gRPC), or the consumer process name and {@code CONSUME}</li>
 *   <li>{@code trooper.caller} on entry server spans, from the W3C baggage member {@code tt.svc}</li>
 *   <li>{@code trooper.process} on consumer spans (explicit value, else consumer group, else destination)</li>
 *   <li>{@code trooper.consumer.attempt} from the captured {@code tt-attempt}, Spring Kafka
 *       {@code kafka_deliveryAttempt} or RabbitMQ {@code x-delivery-count} header</li>
 * </ul>
 *
 * <p>Entry route propagation: each span is mapped to its local root span while it is open; a child reads the root's
 * route when it starts and again when it ends (routes are often resolved after the server span started). Never throws.
 *
 * <p>While the profiler runs, entry spans (and children running on other threads) are also reported to
 * {@link ProfileRoutes} so profile samples can be attributed to the endpoint.
 */
public final class TrooperSpanProcessor implements ExtendedSpanProcessor {
    private static final int MAX_TRACKED = 200_000;

    private final Map<String, ReadableSpan> roots = new ConcurrentHashMap<>();

    @Override
    public void onStart(Context parentContext, ReadWriteSpan span) {
        try {
            Span parent = Span.fromContext(parentContext);
            SpanContext psc = parent.getSpanContext();
            boolean localRoot = !psc.isValid() || psc.isRemote();
            ReadableSpan root;
            if (localRoot) {
                root = span;
            } else {
                root = roots.get(key(psc));
                if (root == null && parent instanceof ReadableSpan rs) {
                    root = rs; // parent already ended or untracked: best effort
                }
            }
            if (root != null) {
                if (roots.size() > MAX_TRACKED) {
                    roots.clear(); // leak guard for spans that are never ended
                }
                roots.put(key(span.getSpanContext()), root);
            }

            SpanKind kind = span.getKind();
            if (ProfileRoutes.isActive()) {
                if (localRoot && (kind == SpanKind.SERVER || kind == SpanKind.CONSUMER)) {
                    ReadableSpan entrySpan = span;
                    ProfileRoutes.entryStarted(key(span.getSpanContext()),
                            () -> new String[] {entryRouteOf(entrySpan), entryMethodOf(entrySpan)});
                } else if (!localRoot && root != null) {
                    ProfileRoutes.childStarted(key(span.getSpanContext()), key(root.getSpanContext()));
                }
            }
            if (localRoot && kind == SpanKind.SERVER) {
                String caller = Baggage.fromContext(parentContext).getEntryValue(TrooperAttributes.BAGGAGE_SERVICE);
                if (caller != null && !caller.isBlank()) {
                    span.setAttribute(TrooperAttributes.CALLER, caller);
                }
            }
            if (kind == SpanKind.CONSUMER) {
                String process = processOf(span);
                if (process != null && span.getAttribute(TrooperAttributes.PROCESS) == null) {
                    span.setAttribute(TrooperAttributes.PROCESS, process);
                }
            }
            if (!localRoot && root != null) {
                String entry = entryRouteOf(root);
                if (entry != null) {
                    span.setAttribute(TrooperAttributes.ENTRY_ROUTE, entry);
                    String method = entryMethodOf(root);
                    if (method != null) {
                        span.setAttribute(TrooperAttributes.ENTRY_METHOD, method);
                    }
                }
            }
        } catch (Throwable t) {
            SdkErrors.record("span_processor", t);
        }
    }

    @Override
    public boolean isStartRequired() {
        return true;
    }

    @Override
    public void onEnding(ReadWriteSpan span) {
        try {
            SpanKind kind = span.getKind();
            if (kind == SpanKind.SERVER) {
                String route = routeOf(span);
                if (route != null) {
                    span.setAttribute(TrooperAttributes.ROUTE, route);
                }
            } else if (kind == SpanKind.CONSUMER) {
                String process = processOf(span);
                if (process != null) {
                    span.setAttribute(TrooperAttributes.PROCESS, process);
                }
                if (span.getAttribute(TrooperAttributes.CONSUMER_ATTEMPT) == null) {
                    Long attempt = capturedAttempt(span);
                    if (attempt != null) {
                        span.setAttribute(TrooperAttributes.CONSUMER_ATTEMPT, attempt);
                    }
                }
            }
            ReadableSpan root = roots.get(key(span.getSpanContext()));
            if (root != null) {
                String entry = entryRouteOf(root);
                if (entry != null && (root == span || span.getAttribute(TrooperAttributes.ENTRY_ROUTE) == null)) {
                    span.setAttribute(TrooperAttributes.ENTRY_ROUTE, entry);
                }
                String method = entryMethodOf(root);
                if (entry != null && method != null && (root == span || span.getAttribute(TrooperAttributes.ENTRY_METHOD) == null)) {
                    span.setAttribute(TrooperAttributes.ENTRY_METHOD, method);
                }
            }
            if (ProfileRoutes.isActive()) {
                ProfileRoutes.spanEnding(key(span.getSpanContext()));
            }
        } catch (Throwable t) {
            SdkErrors.record("span_processor", t);
        }
    }

    @Override
    public boolean isOnEndingRequired() {
        return true;
    }

    @Override
    public void onEnd(ReadableSpan span) {
        String k = key(span.getSpanContext());
        roots.remove(k);
        if (ProfileRoutes.isActive()) {
            ProfileRoutes.spanEnded(k);
        }
    }

    @Override
    public boolean isEndRequired() {
        return true;
    }

    /** Number of open spans currently tracked (for tests / diagnostics). */
    public int trackedSpans() {
        return roots.size();
    }

    private static String key(SpanContext sc) {
        return sc.getTraceId() + sc.getSpanId();
    }

    static String routeOf(ReadableSpan span) {
        String r = span.getAttribute(TrooperAttributes.ROUTE);
        if (r != null) {
            return r;
        }
        if (isGrpc(span)) {
            return RouteTemplates.grpc(span.getAttribute(TrooperAttributes.RPC_SERVICE), span.getAttribute(TrooperAttributes.RPC_METHOD));
        }
        return RouteTemplates.normalize(span.getAttribute(TrooperAttributes.HTTP_ROUTE));
    }

    private static boolean isGrpc(ReadableSpan span) {
        String system = span.getAttribute(TrooperAttributes.RPC_SYSTEM);
        return system != null && system.toLowerCase(java.util.Locale.ROOT).startsWith("grpc");
    }

    /** HTTP method of a server entry ({@code GRPC} for gRPC), {@code CONSUME} for consumers, else an explicit value. */
    static String entryMethodOf(ReadableSpan root) {
        String explicit = root.getAttribute(TrooperAttributes.ENTRY_METHOD);
        switch (root.getKind()) {
            case CONSUMER:
                return TrooperAttributes.METHOD_CONSUME;
            case SERVER: {
                if (isGrpc(root)) {
                    return TrooperAttributes.METHOD_GRPC;
                }
                String m = root.getAttribute(TrooperAttributes.HTTP_REQUEST_METHOD);
                if (m == null) m = root.getAttribute(TrooperAttributes.HTTP_METHOD_OLD);
                if (m == null || m.isBlank()) return explicit;
                return "_OTHER".equals(m) ? m : m.toUpperCase(java.util.Locale.ROOT);
            }
            default:
                return explicit;
        }
    }

    /**
     * Attempt from captured headers: {@code tt-attempt} (decimal), else Spring Kafka {@code kafka_deliveryAttempt},
     * else RabbitMQ {@code x-delivery-count} + 1.
     */
    static Long capturedAttempt(ReadableSpan span) {
        for (var k : TrooperAttributes.MESSAGING_HEADER_ATTEMPT) {
            Long a = TrooperConsumer.parseAttempt(first(span.getAttribute(k)));
            if (a != null) return a;
        }
        Long a = deliveryAttempt(span.getAttribute(TrooperAttributes.MESSAGING_HEADER_DELIVERY_ATTEMPT));
        if (a != null) return a;
        for (var k : TrooperAttributes.MESSAGING_HEADER_RABBIT_DELIVERY_COUNT) {
            Long c = TrooperConsumer.parseAttempt(first(span.getAttribute(k)));
            if (c != null) return c + 1;
        }
        return null;
    }

    private static String first(List<String> values) {
        return values == null || values.isEmpty() ? null : values.get(0);
    }

    static String processOf(ReadableSpan span) {
        String p = span.getAttribute(TrooperAttributes.PROCESS);
        if (p == null) p = span.getAttribute(TrooperAttributes.MESSAGING_CONSUMER_GROUP);
        if (p == null) p = span.getAttribute(TrooperAttributes.MESSAGING_KAFKA_CONSUMER_GROUP);
        if (p == null) p = span.getAttribute(TrooperAttributes.MESSAGING_DESTINATION);
        return p;
    }

    static String entryRouteOf(ReadableSpan root) {
        String explicit = root.getAttribute(TrooperAttributes.ENTRY_ROUTE);
        if (explicit != null && root.getKind() != SpanKind.SERVER && root.getKind() != SpanKind.CONSUMER) {
            return explicit;
        }
        return switch (root.getKind()) {
            case SERVER -> routeOf(root);
            case CONSUMER -> processOf(root);
            default -> explicit;
        };
    }

    /**
     * Spring Kafka writes the attempt as a 4-byte big-endian int; OTel captures header values as UTF-8 strings, so
     * small values arrive as four chars. Plain decimal strings are accepted too.
     */
    static Long deliveryAttempt(List<String> values) {
        if (values == null || values.isEmpty() || values.get(0) == null) {
            return null;
        }
        String v = values.get(0);
        try {
            return Long.parseLong(v.trim());
        } catch (NumberFormatException ignored) {
            // fall through to binary decoding
        }
        byte[] b = v.getBytes(StandardCharsets.ISO_8859_1);
        if (v.length() == 4 && v.chars().allMatch(c -> c < 256)) {
            return (long) (((b[0] & 0xff) << 24) | ((b[1] & 0xff) << 16) | ((b[2] & 0xff) << 8) | (b[3] & 0xff));
        }
        return null;
    }
}
