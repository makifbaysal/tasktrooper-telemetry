package io.tasktrooper.telemetry.core;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.util.List;
import java.util.function.Consumer;
import org.junit.jupiter.api.Test;

class GenAiSpansTest {

    private static SpanData export(Boolean capture, Consumer<Tracer> body) {
        InMemorySpanExporter mem = InMemorySpanExporter.create();
        try (SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(GenAiSpans.wrap(mem, capture))).build()) {
            body.accept(provider.get("t"));
            List<SpanData> spans = mem.getFinishedSpanItems();
            assertThat(spans).hasSize(1);
            return spans.get(0);
        }
    }

    @Test
    void springAiChatObservationIsMappedToContractNames() {
        // what Spring AI's ChatModel observation looks like through the Micrometer → OTel bridge: all strings
        SpanData s = export(null, t -> t.spanBuilder("chat gpt-4o").setSpanKind(SpanKind.CLIENT)
                .setAttribute("gen_ai.operation.name", "chat")
                .setAttribute("gen_ai.system", "azure-openai")
                .setAttribute("gen_ai.request.model", "gpt-4o")
                .setAttribute("gen_ai.response.model", "gpt-4o-2024-08-06")
                .setAttribute("gen_ai.usage.input_tokens", "120")
                .setAttribute("gen_ai.usage.output_tokens", "33")
                .setAttribute("gen_ai.request.temperature", "0.7")
                .setAttribute("gen_ai.response.finish_reasons", "[\"STOP\"]")
                .setAttribute("http.url", "kept")
                .startSpan().end());
        Attributes a = s.getAttributes();
        assertThat(a.get(AttributeKey.stringKey("gen_ai.provider.name"))).isEqualTo("azure.ai.openai");
        assertThat(a.get(AttributeKey.longKey("gen_ai.usage.input_tokens"))).isEqualTo(120L);
        assertThat(a.get(AttributeKey.longKey("gen_ai.usage.output_tokens"))).isEqualTo(33L);
        assertThat(a.get(AttributeKey.stringKey("gen_ai.usage.input_tokens"))).isNull();
        assertThat(a.get(AttributeKey.doubleKey("gen_ai.request.temperature"))).isEqualTo(0.7);
        assertThat(a.get(AttributeKey.stringArrayKey("gen_ai.response.finish_reasons"))).containsExactly("STOP");
        assertThat(a.get(AttributeKey.stringKey("gen_ai.operation.name"))).isEqualTo("chat");
        assertThat(a.get(AttributeKey.stringKey("http.url"))).isEqualTo("kept");
    }

    @Test
    void frameworkObservationsAreNotLlmCalls() {
        SpanData client = export(null, t -> t.spanBuilder("spring_ai chat_client").startSpan()
                .setAttribute("gen_ai.operation.name", "framework")
                .setAttribute("gen_ai.system", "spring_ai")
                .setAttribute("spring.ai.kind", "chat_client")
                .end());
        assertThat(client.getAttributes().get(AttributeKey.stringKey("gen_ai.operation.name"))).isNull();
        assertThat(client.getAttributes().get(AttributeKey.stringKey("gen_ai.system"))).isNull();
        assertThat(client.getAttributes().get(AttributeKey.stringKey("gen_ai.provider.name"))).isNull();
        assertThat(client.getAttributes().get(AttributeKey.stringKey("spring.ai.kind"))).isEqualTo("chat_client");

        SpanData tool = export(null, t -> t.spanBuilder("tool_call weather").startSpan()
                .setAttribute("gen_ai.operation.name", "framework")
                .setAttribute("gen_ai.system", "spring_ai")
                .setAttribute("spring.ai.kind", "tool_call")
                .setAttribute("spring.ai.tool.definition.name", "weather")
                .end());
        assertThat(tool.getAttributes().get(AttributeKey.stringKey("gen_ai.operation.name"))).isEqualTo("execute_tool");
        assertThat(tool.getAttributes().get(AttributeKey.stringKey("gen_ai.tool.name"))).isEqualTo("weather");

        SpanData embed = export(null, t -> t.spanBuilder("embedding").startSpan()
                .setAttribute("gen_ai.operation.name", "embedding")
                .setAttribute("gen_ai.system", "openai")
                .setAttribute("gen_ai.usage.prompt_tokens", 9L)
                .end());
        assertThat(embed.getAttributes().get(AttributeKey.stringKey("gen_ai.operation.name"))).isEqualTo("embeddings");
        assertThat(embed.getAttributes().get(AttributeKey.stringKey("gen_ai.provider.name"))).isEqualTo("openai");
        assertThat(embed.getAttributes().get(AttributeKey.longKey("gen_ai.usage.input_tokens"))).isEqualTo(9L);
        assertThat(embed.getAttributes().get(AttributeKey.longKey("gen_ai.usage.prompt_tokens"))).isNull();
    }

    @Test
    void contentIsStrippedWhenCaptureIsOff() {
        SpanData s = export(false, t -> {
            Span span = t.spanBuilder("chat").startSpan()
                    .setAttribute("gen_ai.operation.name", "chat")
                    .setAttribute("gen_ai.provider.name", "openai")
                    .setAttribute("gen_ai.input.messages", "[{\"role\":\"user\"}]")
                    .setAttribute("gen_ai.output.messages", "[]")
                    .setAttribute("gen_ai.prompt.0.content", "secret");
            span.addEvent("gen_ai.content.prompt", Attributes.of(AttributeKey.stringKey("gen_ai.prompt"), "secret"));
            span.addEvent("retry");
            span.end();
        });
        assertThat(s.getAttributes().asMap().keySet()).extracting(AttributeKey::getKey)
                .containsExactlyInAnyOrder("gen_ai.operation.name", "gen_ai.provider.name");
        assertThat(s.getEvents()).extracting(e -> e.getName()).containsExactly("retry");
        assertThat(s.getTotalRecordedEvents()).isEqualTo(1);
    }

    @Test
    void springAiPromptEventsBecomeMessagesUnlessDisabled() {
        SpanData s = export(true, t -> {
            Span span = t.spanBuilder("chat").startSpan()
                    .setAttribute("gen_ai.operation.name", "chat")
                    .setAttribute("gen_ai.system", "anthropic");
            span.addEvent("gen_ai.content.prompt", Attributes.of(AttributeKey.stringArrayKey("gen_ai.prompt"), List.of("hello")));
            span.addEvent("gen_ai.content.completion", Attributes.of(AttributeKey.stringKey("gen_ai.completion"), "hi!"));
            span.end();
        });
        assertThat(s.getAttributes().get(AttributeKey.stringKey("gen_ai.input.messages"))).isEqualTo("hello");
        assertThat(s.getAttributes().get(AttributeKey.stringKey("gen_ai.output.messages"))).isEqualTo("hi!");
        assertThat(s.getAttributes().get(AttributeKey.stringKey("gen_ai.provider.name"))).isEqualTo("anthropic");
    }

    @Test
    void nonGenAiSpansPassThroughUnchanged() {
        SpanData s = export(false, t -> t.spanBuilder("GET /x").setAttribute("http.route", "/x").startSpan().end());
        assertThat(s.getClass().getSimpleName()).isNotEqualTo("Normalized");
        assertThat(GenAiSpans.splitList("STOP, 'LENGTH'")).containsExactly("STOP", "LENGTH");
        assertThat(GenAiSpans.provider("Bedrock_Converse")).isEqualTo("aws.bedrock");
    }
}
