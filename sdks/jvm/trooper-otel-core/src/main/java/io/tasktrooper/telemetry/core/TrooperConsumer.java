package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.trace.Span;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.function.BiConsumer;

/**
 * Helpers for consumer/worker spans: process name, retry attempt, DLQ flag, and the {@code tt-attempt} message header
 * that carries the attempt number across retry topics / re-publishes (contracts.md §4 "Retry sayısı taşıma").
 *
 * <p>Retry topic producer example (Kafka):
 *
 * <pre>{@code
 * long next = TrooperConsumer.nextAttempt(record.headers().lastHeader("tt-attempt") ...);   // or nextAttempt()
 * TrooperConsumer.writeAttempt((k, v) -> producerRecord.headers().add(k, v.getBytes(UTF_8)), next);
 * }</pre>
 */
public final class TrooperConsumer {
    /** Message header name ({@code tt-attempt}): attempt number as a decimal string, 1 = first delivery. */
    public static final String ATTEMPT_HEADER = TrooperAttributes.ATTEMPT_HEADER;

    private static final ThreadLocal<Long> CURRENT_ATTEMPT = new ThreadLocal<>();

    private TrooperConsumer() {}

    /** Marks the current span (normally the consumer "process" span). Null arguments are skipped. Never throws. */
    public static void markCurrent(String process, Long attempt, Boolean dlq) {
        mark(Span.current(), process, attempt, dlq);
    }

    public static void mark(Span span, String process, Long attempt, Boolean dlq) {
        try {
            if (span == null || !span.isRecording()) {
                return;
            }
            if (process != null && !process.isBlank()) {
                span.setAttribute(TrooperAttributes.PROCESS, process);
            }
            if (attempt != null && attempt > 0) {
                span.setAttribute(TrooperAttributes.CONSUMER_ATTEMPT, attempt);
            }
            if (dlq != null) {
                span.setAttribute(TrooperAttributes.CONSUMER_DLQ, dlq);
            }
        } catch (Throwable t) {
            SdkErrors.record("consumer", t);
        }
    }

    // ---- tt-attempt header ----

    /** Writes {@code tt-attempt=<attempt>} through {@code setter} (e.g. Kafka {@code Headers.add}). Never throws. */
    public static void writeAttempt(BiConsumer<String, String> setter, long attempt) {
        if (setter == null || attempt < 1) {
            return;
        }
        try {
            setter.accept(ATTEMPT_HEADER, Long.toString(attempt));
        } catch (Throwable t) {
            SdkErrors.record("consumer", t);
        }
    }

    /** Header value bytes for Kafka ({@code headers.add("tt-attempt", attemptHeaderValue(n))}). */
    public static byte[] attemptHeaderValue(long attempt) {
        return Long.toString(Math.max(1, attempt)).getBytes(StandardCharsets.UTF_8);
    }

    /** Puts {@code tt-attempt} into a header map (RabbitMQ / Spring Messaging headers). */
    public static void writeAttempt(Map<String, Object> headers, long attempt) {
        if (headers != null) {
            writeAttempt(headers::put, attempt);
        }
    }

    /** Attempt number for the next publish: current attempt + 1 (current defaults to 1 when unknown). */
    public static long nextAttempt(Long currentAttempt) {
        return (currentAttempt == null || currentAttempt < 1 ? 1 : currentAttempt) + 1;
    }

    /** Next attempt for the message being handled on this thread (set by the Spring listener integration). */
    public static long nextAttempt() {
        return nextAttempt(currentAttempt());
    }

    /** Attempt of the message currently handled on this thread, when known (Spring listener integration). */
    public static Long currentAttempt() {
        return CURRENT_ATTEMPT.get();
    }

    /** Sets / clears ({@code null}) the attempt of the message handled on this thread. */
    public static void setCurrentAttempt(Long attempt) {
        if (attempt == null) {
            CURRENT_ATTEMPT.remove();
        } else {
            CURRENT_ATTEMPT.set(attempt);
        }
    }

    /**
     * Reads an attempt from header values: {@code tt-attempt} first, else Spring Kafka {@code kafka_deliveryAttempt},
     * else RabbitMQ {@code x-delivery-count} + 1, else RabbitMQ {@code x-death} counts + 1. Values may be strings,
     * numbers, byte arrays or (x-death) lists of maps. Returns null when none is present.
     */
    public static Long attemptFromHeaders(Map<String, ?> headers) {
        if (headers == null || headers.isEmpty()) {
            return null;
        }
        try {
            Long a = attemptValue(headers.get(ATTEMPT_HEADER));
            if (a != null) return a;
            a = attemptValue(headers.get(TrooperAttributes.KAFKA_DELIVERY_ATTEMPT_HEADER));
            if (a != null) return a;
            Long count = attemptValue(headers.get(TrooperAttributes.RABBIT_DELIVERY_COUNT_HEADER));
            if (count != null) return count + 1;
            Long deaths = xDeathCount(headers.get("x-death"));
            if (deaths != null && deaths > 0) return deaths + 1;
        } catch (Throwable t) {
            SdkErrors.record("consumer", t);
        }
        return null;
    }

    /** One header value as a number: {@link Number}, decimal string, decimal or 4-byte big-endian bytes. */
    public static Long attemptValue(Object value) {
        if (value == null) return null;
        if (value instanceof Number n) return n.longValue();
        if (value instanceof byte[] b) return kafkaDeliveryAttempt(b);
        return parseAttempt(value.toString());
    }

    /** Parses a decimal header value; null when absent or invalid. */
    public static Long parseAttempt(String value) {
        if (value == null) return null;
        String v = value.trim();
        if (v.isEmpty()) return null;
        try {
            long n = Long.parseLong(v);
            return n >= 0 ? n : null;
        } catch (NumberFormatException e) {
            return null;
        }
    }

    private static Long xDeathCount(Object xDeath) {
        if (!(xDeath instanceof List<?> list)) return null;
        long total = 0;
        for (Object entry : list) {
            if (entry instanceof Map<?, ?> m && m.get("count") instanceof Number n) {
                total += n.longValue();
            }
        }
        return total;
    }

    /** Decodes Spring Kafka's {@code kafka_deliveryAttempt} header (4-byte big-endian int, or a decimal string). */
    public static Long kafkaDeliveryAttempt(byte[] value) {
        if (value == null) {
            return null;
        }
        boolean digits = value.length > 0;
        for (byte b : value) {
            if (b < '0' || b > '9') {
                digits = false;
                break;
            }
        }
        if (value.length == 4 && !digits) {
            return (long) (((value[0] & 0xff) << 24) | ((value[1] & 0xff) << 16) | ((value[2] & 0xff) << 8) | (value[3] & 0xff));
        }
        return parseAttempt(new String(value, StandardCharsets.UTF_8));
    }
}
