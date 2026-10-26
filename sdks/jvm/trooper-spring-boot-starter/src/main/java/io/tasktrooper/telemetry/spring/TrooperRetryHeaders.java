package io.tasktrooper.telemetry.spring;

import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperAttributes;
import io.tasktrooper.telemetry.core.TrooperConsumer;

/**
 * Writes the {@code tt-attempt} header when a listener re-publishes a message to a retry topic / queue, so the next
 * consumer reports {@code trooper.consumer.attempt} (contracts.md §4). Inside a {@code @KafkaListener} /
 * {@code @RabbitListener} the current attempt is known; otherwise pass it explicitly.
 *
 * <pre>{@code
 * ProducerRecord<String, Order> retry = new ProducerRecord<>("orders.retry", key, order);
 * TrooperRetryHeaders.setNextAttempt(retry.headers());
 * kafkaTemplate.send(retry);
 *
 * rabbitTemplate.convertAndSend("orders.retry", order, TrooperRetryHeaders::setNextAttemptAmqp);
 * }</pre>
 */
public final class TrooperRetryHeaders {
    private TrooperRetryHeaders() {}

    /** Kafka: replaces {@code tt-attempt} with the current attempt + 1. */
    public static org.apache.kafka.common.header.Headers setNextAttempt(org.apache.kafka.common.header.Headers headers) {
        return setAttempt(headers, TrooperConsumer.nextAttempt());
    }

    /** Kafka: replaces {@code tt-attempt} with {@code attempt}. Never throws. */
    public static org.apache.kafka.common.header.Headers setAttempt(org.apache.kafka.common.header.Headers headers, long attempt) {
        try {
            if (headers != null) {
                headers.remove(TrooperAttributes.ATTEMPT_HEADER);
                headers.add(TrooperAttributes.ATTEMPT_HEADER, TrooperConsumer.attemptHeaderValue(attempt));
            }
        } catch (Throwable t) {
            SdkErrors.record("retry_headers", t);
        }
        return headers;
    }

    /** RabbitMQ ({@code MessagePostProcessor}-friendly): sets {@code tt-attempt} to the current attempt + 1. */
    public static org.springframework.amqp.core.Message setNextAttemptAmqp(org.springframework.amqp.core.Message message) {
        return setAttemptAmqp(message, TrooperConsumer.nextAttempt());
    }

    /** RabbitMQ: sets {@code tt-attempt} to {@code attempt}. Never throws. */
    public static org.springframework.amqp.core.Message setAttemptAmqp(org.springframework.amqp.core.Message message, long attempt) {
        try {
            if (message != null && message.getMessageProperties() != null) {
                message.getMessageProperties().setHeader(TrooperAttributes.ATTEMPT_HEADER, Long.toString(Math.max(1, attempt)));
            }
        } catch (Throwable t) {
            SdkErrors.record("retry_headers", t);
        }
        return message;
    }
}
