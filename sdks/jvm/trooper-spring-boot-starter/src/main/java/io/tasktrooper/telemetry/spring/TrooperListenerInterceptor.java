package io.tasktrooper.telemetry.spring;

import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperAttributes;
import io.tasktrooper.telemetry.core.TrooperConsumer;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.util.Map;
import org.aopalliance.intercept.MethodInterceptor;
import org.aopalliance.intercept.MethodInvocation;
import org.springframework.core.annotation.AnnotatedElementUtils;
import org.springframework.util.ClassUtils;

/** Marks the current consumer span for a listener invocation. Never throws on its own account. */
class TrooperListenerInterceptor implements MethodInterceptor {
    private static final boolean SPRING_RETRY = ClassUtils.isPresent(
            "org.springframework.retry.support.RetrySynchronizationManager", TrooperListenerInterceptor.class.getClassLoader());
    private static final boolean KAFKA_CLIENTS = ClassUtils.isPresent(
            "org.apache.kafka.clients.consumer.ConsumerRecord", TrooperListenerInterceptor.class.getClassLoader());
    private static final boolean SPRING_AMQP = ClassUtils.isPresent(
            "org.springframework.amqp.core.Message", TrooperListenerInterceptor.class.getClassLoader());
    private static final boolean SPRING_MESSAGING = ClassUtils.isPresent(
            "org.springframework.messaging.Message", TrooperListenerInterceptor.class.getClassLoader());

    @Override
    public Object invoke(MethodInvocation invocation) throws Throwable {
        Long previous = TrooperConsumer.currentAttempt();
        try {
            Long attempt = attempt(invocation.getArguments());
            TrooperConsumer.markCurrent(processName(invocation.getMethod()), attempt, null);
            TrooperConsumer.setCurrentAttempt(attempt);
        } catch (Throwable t) {
            SdkErrors.record("spring_listener", t); // never break message handling
        }
        try {
            return invocation.proceed();
        } finally {
            TrooperConsumer.setCurrentAttempt(previous);
        }
    }

    static String processName(Method method) {
        if (TrooperListenerBeanPostProcessor.KAFKA_LISTENER != null) {
            Map<String, Object> a = AnnotatedElementUtils.getMergedAnnotationAttributes(method, TrooperListenerBeanPostProcessor.KAFKA_LISTENER);
            if (a != null) {
                String id = firstNonBlank((String) a.get("id"), (String) a.get("groupId"));
                if (id == null && a.get("topics") instanceof String[] topics && topics.length > 0) id = topics[0];
                return id != null ? id : method.getDeclaringClass().getSimpleName() + "." + method.getName();
            }
        }
        if (TrooperListenerBeanPostProcessor.RABBIT_LISTENER != null) {
            Map<String, Object> a = AnnotatedElementUtils.getMergedAnnotationAttributes(method, TrooperListenerBeanPostProcessor.RABBIT_LISTENER);
            if (a != null) {
                String id = (String) a.get("id");
                if ((id == null || id.isBlank()) && a.get("queues") instanceof String[] queues && queues.length > 0) id = queues[0];
                return id != null && !id.isBlank() ? id : method.getDeclaringClass().getSimpleName() + "." + method.getName();
            }
        }
        return null;
    }

    /**
     * Attempt of the delivered message: {@code tt-attempt} header, else Spring Kafka {@code kafka_deliveryAttempt}, else
     * RabbitMQ {@code x-delivery-count} / {@code x-death}, else Spring Retry's retry count.
     */
    static Long attempt(Object[] args) {
        if (args != null) {
            for (Object arg : args) {
                Long a = KAFKA_CLIENTS ? KafkaSupport.deliveryAttempt(arg) : null;
                if (a == null && SPRING_AMQP) a = AmqpSupport.deliveryAttempt(arg);
                if (a == null && SPRING_MESSAGING) a = MessagingSupport.deliveryAttempt(arg);
                if (a != null) return a;
            }
        }
        if (SPRING_RETRY) {
            return RetrySupport.attempt();
        }
        return null;
    }

    private static String firstNonBlank(String... values) {
        for (String v : values) if (v != null && !v.isBlank()) return v;
        return null;
    }

    static final class KafkaSupport {
        static Long deliveryAttempt(Object arg) {
            if (arg instanceof org.apache.kafka.clients.consumer.ConsumerRecord<?, ?> record) {
                var tt = record.headers().lastHeader(TrooperAttributes.ATTEMPT_HEADER);
                Long a = tt == null ? null : TrooperConsumer.parseAttempt(new String(tt.value(), StandardCharsets.UTF_8));
                if (a != null) return a;
                var header = record.headers().lastHeader(TrooperAttributes.KAFKA_DELIVERY_ATTEMPT_HEADER);
                return header == null ? null : TrooperConsumer.kafkaDeliveryAttempt(header.value());
            }
            return null;
        }
    }

    static final class AmqpSupport {
        static Long deliveryAttempt(Object arg) {
            if (arg instanceof org.springframework.amqp.core.Message message && message.getMessageProperties() != null) {
                return TrooperConsumer.attemptFromHeaders(message.getMessageProperties().getHeaders());
            }
            return null;
        }
    }

    static final class MessagingSupport {
        static Long deliveryAttempt(Object arg) {
            if (arg instanceof org.springframework.messaging.Message<?> message) {
                return TrooperConsumer.attemptFromHeaders(message.getHeaders());
            }
            return null;
        }
    }

    static final class RetrySupport {
        static Long attempt() {
            var ctx = org.springframework.retry.support.RetrySynchronizationManager.getContext();
            return ctx == null ? null : (long) ctx.getRetryCount() + 1;
        }
    }
}
