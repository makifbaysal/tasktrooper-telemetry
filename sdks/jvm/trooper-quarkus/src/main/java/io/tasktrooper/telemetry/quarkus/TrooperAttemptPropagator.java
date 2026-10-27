package io.tasktrooper.telemetry.quarkus;

import io.opentelemetry.context.Context;
import io.opentelemetry.context.ContextKey;
import io.opentelemetry.context.propagation.TextMapGetter;
import io.opentelemetry.context.propagation.TextMapPropagator;
import io.opentelemetry.context.propagation.TextMapSetter;
import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperAttributes;
import io.tasktrooper.telemetry.core.TrooperConsumer;
import java.nio.charset.StandardCharsets;
import java.util.Collection;
import java.util.function.Function;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Reads the delivery attempt of an incoming message while the trace context is extracted, so the consumer span started
 * from that context gets {@code trooper.consumer.attempt} (see {@link TrooperAttemptSpanProcessor}).
 *
 * <p>SmallRye Reactive Messaging (Kafka / RabbitMQ connectors) extracts the parent context through the configured
 * propagators right before it starts — and immediately ends — the consumer span, so neither an
 * {@code IncomingInterceptor} nor the {@code @Incoming} method can still annotate it. Reading the headers in the
 * propagator is the one hook that sees both the message headers and the span start.
 *
 * <p>Order (same as the Spring starter): {@code tt-attempt} → Spring Kafka {@code kafka_deliveryAttempt} → RabbitMQ
 * {@code x-delivery-count} + 1 → RabbitMQ {@code x-death} counts + 1 → SmallRye Kafka {@code delayed-retry-count} + 1.
 * Injection is delegated unchanged. Never throws.
 */
public final class TrooperAttemptPropagator implements TextMapPropagator {
    /** Attempt of the message whose headers produced this context; {@code 0} = none (shadows an outer value). */
    static final ContextKey<Long> ATTEMPT = ContextKey.named("trooper-consumer-attempt");

    static final String SMALLRYE_DELAYED_RETRY_COUNT = "delayed-retry-count";
    private static final Pattern X_DEATH_COUNT = Pattern.compile("count=(\\d+)");

    private final TextMapPropagator delegate;

    private TrooperAttemptPropagator(TextMapPropagator delegate) {
        this.delegate = delegate;
    }

    /** Wraps {@code delegate}; extraction additionally records the message attempt in the context. */
    public static TextMapPropagator wrap(TextMapPropagator delegate) {
        if (delegate instanceof TrooperAttemptPropagator) {
            return delegate;
        }
        return new TrooperAttemptPropagator(delegate == null ? TextMapPropagator.noop() : delegate);
    }

    /** Attempt recorded in {@code context} by a previous extraction, or null. */
    public static Long attemptFrom(Context context) {
        Long a = context == null ? null : context.get(ATTEMPT);
        return a == null || a < 1 ? null : a;
    }

    @Override
    public Collection<String> fields() {
        return delegate.fields();
    }

    @Override
    public <C> void inject(Context context, C carrier, TextMapSetter<C> setter) {
        delegate.inject(context, carrier, setter);
    }

    @Override
    public <C> Context extract(Context context, C carrier, TextMapGetter<C> getter) {
        Context ctx = delegate.extract(context, carrier, getter);
        if (carrier == null || getter == null) {
            return ctx;
        }
        try {
            Long attempt = attempt(key -> getter.get(carrier, key));
            if (attempt != null) {
                return ctx.with(ATTEMPT, attempt);
            }
            if (ctx.get(ATTEMPT) != null) {
                return ctx.with(ATTEMPT, 0L); // a new message without headers must not inherit the outer attempt
            }
        } catch (Throwable t) {
            SdkErrors.record("quarkus_attempt", t);
        }
        return ctx;
    }

    /** Attempt from string header values (as OTel text map getters expose them); null when none is present. */
    static Long attempt(Function<String, String> header) {
        Long a = TrooperConsumer.parseAttempt(header.apply(TrooperAttributes.ATTEMPT_HEADER));
        if (a != null && a > 0) return a;
        a = kafkaDeliveryAttempt(header.apply(TrooperAttributes.KAFKA_DELIVERY_ATTEMPT_HEADER));
        if (a != null && a > 0) return a;
        Long count = TrooperConsumer.parseAttempt(header.apply(TrooperAttributes.RABBIT_DELIVERY_COUNT_HEADER));
        if (count != null) return count + 1;
        Long deaths = xDeathCount(header.apply("x-death"));
        if (deaths != null && deaths > 0) return deaths + 1;
        Long delayed = TrooperConsumer.parseAttempt(header.apply(SMALLRYE_DELAYED_RETRY_COUNT));
        if (delayed != null && delayed > 0) return delayed + 1;
        return null;
    }

    /**
     * Spring Kafka writes {@code kafka_deliveryAttempt} as a 4-byte big-endian int; the Kafka text map getter decodes
     * header bytes as UTF-8, so small values arrive as four chars. Decimal strings are accepted too.
     */
    static Long kafkaDeliveryAttempt(String v) {
        if (v == null) return null;
        Long a = TrooperConsumer.parseAttempt(v);
        if (a != null) return a;
        if (v.length() == 4 && v.chars().allMatch(c -> c < 128)) {
            byte[] b = v.getBytes(StandardCharsets.ISO_8859_1);
            return (long) (((b[0] & 0xff) << 24) | ((b[1] & 0xff) << 16) | ((b[2] & 0xff) << 8) | (b[3] & 0xff));
        }
        return null;
    }

    /** Sum of {@code count} entries in an {@code x-death} header rendered as text ({@code [{count=2, ...}, ...]}). */
    static Long xDeathCount(String v) {
        if (v == null || v.isEmpty()) return null;
        long total = 0;
        Matcher m = X_DEATH_COUNT.matcher(v);
        while (m.find()) {
            try {
                total += Long.parseLong(m.group(1));
            } catch (NumberFormatException ignored) {
                // overflow: ignore entry
            }
        }
        return total;
    }

    @Override
    public String toString() {
        return "TrooperAttemptPropagator{" + delegate + "}";
    }
}
