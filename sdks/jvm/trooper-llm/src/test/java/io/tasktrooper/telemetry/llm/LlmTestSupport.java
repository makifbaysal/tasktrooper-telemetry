package io.tasktrooper.telemetry.llm;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.util.List;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;

abstract class LlmTestSupport {
    protected InMemorySpanExporter exporter;
    protected OpenTelemetrySdk sdk;

    @BeforeEach
    void setUpOtel() {
        exporter = InMemorySpanExporter.create();
        sdk = OpenTelemetrySdk.builder()
                .setTracerProvider(SdkTracerProvider.builder().addSpanProcessor(SimpleSpanProcessor.create(exporter)).build())
                .build();
        TrooperLlm.setOpenTelemetry(sdk);
        TrooperLlm.setCaptureContent(null);
    }

    @AfterEach
    void tearDownOtel() {
        TrooperLlm.setOpenTelemetry(null);
        TrooperLlm.setCaptureContent(null);
        sdk.close();
    }

    protected List<SpanData> spans() {
        return exporter.getFinishedSpanItems();
    }

    protected SpanData only() {
        List<SpanData> s = spans();
        if (s.size() != 1) throw new AssertionError("expected one span, got " + s);
        return s.get(0);
    }

    static <T> T attr(SpanData s, AttributeKey<T> key) {
        return s.getAttributes().get(key);
    }

    static String str(SpanData s, String key) {
        return s.getAttributes().get(AttributeKey.stringKey(key));
    }

    static Long lng(SpanData s, String key) {
        return s.getAttributes().get(AttributeKey.longKey(key));
    }
}
