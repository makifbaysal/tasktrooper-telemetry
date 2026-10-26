package io.tasktrooper.telemetry.spring;

import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider;
import io.tasktrooper.telemetry.core.SdkErrors;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.beans.factory.SmartInitializingSingleton;
import io.tasktrooper.telemetry.core.TrooperAutoConfigurationCustomizerProvider;
import org.springframework.boot.autoconfigure.AutoConfiguration;
import org.springframework.boot.autoconfigure.condition.ConditionalOnClass;
import org.springframework.boot.autoconfigure.condition.ConditionalOnMissingBean;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.boot.autoconfigure.condition.ConditionalOnWebApplication;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.core.env.Environment;
import org.springframework.web.servlet.config.annotation.InterceptorRegistry;
import org.springframework.web.servlet.config.annotation.WebMvcConfigurer;

/**
 * Registers the TaskTrooper customizations with the OpenTelemetry Spring Boot starter (which applies
 * {@link AutoConfigurationCustomizerProvider} beans), the Spring MVC route interceptor and the
 * {@code @KafkaListener}/{@code @RabbitListener} process naming.
 */
@AutoConfiguration(beforeName = "io.opentelemetry.instrumentation.spring.autoconfigure.OpenTelemetryAutoConfiguration")
@ConditionalOnProperty(name = "trooper.enabled", havingValue = "true", matchIfMissing = true)
public class TrooperAutoConfiguration {

    @Bean
    @ConditionalOnMissingBean(TrooperAutoConfigurationCustomizerProvider.class)
    public TrooperAutoConfigurationCustomizerProvider trooperAutoConfigurationCustomizerProvider(Environment environment) {
        return new TrooperAutoConfigurationCustomizerProvider(() -> TrooperEnvironmentPostProcessor.config(environment));
    }

    /** Registers {@code trooper.sdk.errors} on the application's OpenTelemetry once it exists. */
    @Bean
    @ConditionalOnClass(name = "io.opentelemetry.api.OpenTelemetry")
    public SmartInitializingSingleton trooperSdkErrorsBinder(ObjectProvider<OpenTelemetry> openTelemetry) {
        return () -> {
            try {
                openTelemetry.ifAvailable(SdkErrors::bind);
            } catch (Throwable t) {
                SdkErrors.record("spring_init", t);
            }
        };
    }

    @Bean
    public static TrooperListenerBeanPostProcessor trooperListenerBeanPostProcessor() {
        return new TrooperListenerBeanPostProcessor();
    }

    @Configuration(proxyBeanMethods = false)
    @ConditionalOnWebApplication(type = ConditionalOnWebApplication.Type.SERVLET)
    @ConditionalOnClass(name = "org.springframework.web.servlet.HandlerInterceptor")
    static class WebMvc {
        @Bean
        WebMvcConfigurer trooperRouteWebMvcConfigurer() {
            return new WebMvcConfigurer() {
                @Override
                public void addInterceptors(InterceptorRegistry registry) {
                    registry.addInterceptor(new TrooperRouteInterceptor());
                }
            };
        }
    }
}
