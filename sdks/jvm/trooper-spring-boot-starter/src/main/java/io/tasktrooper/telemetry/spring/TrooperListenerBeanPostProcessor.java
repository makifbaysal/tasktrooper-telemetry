package io.tasktrooper.telemetry.spring;

import java.lang.annotation.Annotation;
import java.lang.reflect.Method;
import org.springframework.aop.framework.autoproxy.AbstractBeanFactoryAwareAdvisingPostProcessor;
import org.springframework.aop.support.StaticMethodMatcherPointcutAdvisor;
import org.springframework.core.annotation.AnnotatedElementUtils;
import org.springframework.util.ClassUtils;

/**
 * Proxies beans with {@code @KafkaListener} / {@code @RabbitListener} methods so each invocation marks the current
 * consumer span with {@code trooper.process} (listener id) and {@code trooper.consumer.attempt}.
 */
public final class TrooperListenerBeanPostProcessor extends AbstractBeanFactoryAwareAdvisingPostProcessor {
    static final Class<? extends Annotation> KAFKA_LISTENER = load("org.springframework.kafka.annotation.KafkaListener");
    static final Class<? extends Annotation> RABBIT_LISTENER = load("org.springframework.amqp.rabbit.annotation.RabbitListener");

    public TrooperListenerBeanPostProcessor() {
        setBeforeExistingAdvisors(true);
        setProxyTargetClass(true);
        StaticMethodMatcherPointcutAdvisor advisor = new StaticMethodMatcherPointcutAdvisor(new TrooperListenerInterceptor()) {
            @Override
            public boolean matches(Method method, Class<?> targetClass) {
                return isListener(method);
            }
        };
        this.advisor = advisor;
    }

    static boolean isListener(Method method) {
        return (KAFKA_LISTENER != null && AnnotatedElementUtils.hasAnnotation(method, KAFKA_LISTENER))
                || (RABBIT_LISTENER != null && AnnotatedElementUtils.hasAnnotation(method, RABBIT_LISTENER));
    }

    @SuppressWarnings("unchecked")
    private static Class<? extends Annotation> load(String name) {
        try {
            ClassLoader cl = TrooperListenerBeanPostProcessor.class.getClassLoader();
            return ClassUtils.isPresent(name, cl) ? (Class<? extends Annotation>) ClassUtils.forName(name, cl) : null;
        } catch (Throwable t) {
            return null;
        }
    }
}
