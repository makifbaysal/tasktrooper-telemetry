package io.tasktrooper.telemetry.spring;

import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.util.List;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.context.annotation.Bean;

/** Captures spans in memory next to the regular configuration. */
@TestConfiguration(proxyBeanMethods = false)
public class TestTelemetry {
    public static final InMemorySpanExporter EXPORTER = InMemorySpanExporter.create();

    @Bean
    AutoConfigurationCustomizerProvider inMemoryExporter() {
        return c -> c.addTracerProviderCustomizer((b, p) -> b.addSpanProcessor(SimpleSpanProcessor.create(EXPORTER)));
    }

    static SpanData span(String nameContains) {
        List<SpanData> spans = EXPORTER.getFinishedSpanItems();
        return spans.stream().filter(s -> s.getName().contains(nameContains)).findFirst()
                .orElseThrow(() -> new AssertionError("no span '" + nameContains + "' in " + spans.stream().map(SpanData::getName).toList()));
    }
}
