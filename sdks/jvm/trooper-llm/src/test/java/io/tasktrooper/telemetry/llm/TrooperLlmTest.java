package io.tasktrooper.telemetry.llm;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatCode;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.trace.data.SpanData;
import java.util.List;
import org.junit.jupiter.api.Test;

class TrooperLlmTest extends LlmTestSupport {

    @Test
    void manualCallBecomesGenAiClientSpan() throws Exception {
        LlmCall call = TrooperLlm.start("Anthropic", "claude-sonnet-4-5", "chat")
                .maxTokens(1024).temperature(0.2).conversationId("conv-1");
        try (Scope ignored = call.makeCurrent()) {
            Thread.sleep(5);
            call.firstToken();
            call.firstToken(); // only the first one counts
            call.prompt("my secret prompt").completion("answer");
            call.responseModel("claude-sonnet-4-5-20250929").responseId("msg_1").finishReasons("end_turn");
            call.end(LlmUsage.of(120, 40).withCache(100L, 0L));
        }
        call.end(LlmUsage.of(1, 1)); // ignored

        SpanData s = only();
        assertThat(s.getName()).isEqualTo("chat claude-sonnet-4-5");
        assertThat(s.getKind()).isEqualTo(SpanKind.CLIENT);
        assertThat(s.getInstrumentationScopeInfo().getName()).isEqualTo(TrooperLlm.INSTRUMENTATION_NAME);
        assertThat(str(s, "gen_ai.operation.name")).isEqualTo("chat");
        assertThat(str(s, "gen_ai.provider.name")).isEqualTo("anthropic");
        assertThat(str(s, "gen_ai.request.model")).isEqualTo("claude-sonnet-4-5");
        assertThat(str(s, "gen_ai.response.model")).isEqualTo("claude-sonnet-4-5-20250929");
        assertThat(str(s, "gen_ai.response.id")).isEqualTo("msg_1");
        assertThat(str(s, "gen_ai.conversation.id")).isEqualTo("conv-1");
        assertThat(attr(s, LlmCall.FINISH_REASONS)).containsExactly("end_turn");
        assertThat(lng(s, "gen_ai.request.max_tokens")).isEqualTo(1024L);
        assertThat(attr(s, LlmCall.TEMPERATURE)).isEqualTo(0.2);
        assertThat(lng(s, "gen_ai.usage.input_tokens")).isEqualTo(120L);
        assertThat(lng(s, "gen_ai.usage.output_tokens")).isEqualTo(40L);
        assertThat(lng(s, "gen_ai.usage.cache_read.input_tokens")).isEqualTo(100L);
        assertThat(lng(s, "gen_ai.usage.cache_creation.input_tokens")).isEqualTo(0L);
        assertThat(attr(s, LlmCall.TTFT_MS)).isGreaterThanOrEqualTo(4.0);
        // content capture is off by default
        assertThat(str(s, "gen_ai.input.messages")).isNull();
        assertThat(str(s, "gen_ai.output.messages")).isNull();
    }

    @Test
    void contentIsRecordedOnlyWhenEnabled() {
        TrooperLlm.setCaptureContent(true);
        LlmCall call = TrooperLlm.start("openai", "gpt-4o");
        call.prompt("say \"hi\"\n").completion("hi").systemInstructions(GenAiJson.parts(List.of("be brief")));
        call.end();
        SpanData s = only();
        assertThat(str(s, "gen_ai.input.messages"))
                .isEqualTo("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"say \\\"hi\\\"\\n\"}]}]");
        assertThat(str(s, "gen_ai.output.messages"))
                .isEqualTo("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"hi\"}]}]");
        assertThat(str(s, "gen_ai.system_instructions")).isEqualTo("[{\"type\":\"text\",\"content\":\"be brief\"}]");
    }

    @Test
    void captureFollowsSystemProperty() {
        System.setProperty(TrooperLlm.CAPTURE_CONTENT_PROPERTY, "true");
        try {
            assertThat(TrooperLlm.captureContent()).isTrue();
            TrooperLlm.setCaptureContent(false);
            assertThat(TrooperLlm.captureContent()).isFalse();
        } finally {
            System.clearProperty(TrooperLlm.CAPTURE_CONTENT_PROPERTY);
        }
    }

    @Test
    void failureIsRecordedAndNothingThrows() {
        LlmCall call = TrooperLlm.start("openai", "gpt-4o", "embeddings");
        call.fail(new IllegalStateException("rate limited"));
        assertThatCode(() -> {
            call.firstToken();
            call.end();
            call.fail(null);
            call.close();
        }).doesNotThrowAnyException();
        SpanData s = only();
        assertThat(s.getName()).isEqualTo("embeddings gpt-4o");
        assertThat(s.getStatus().getStatusCode()).isEqualTo(StatusCode.ERROR);
        assertThat(str(s, "error.type")).isEqualTo("java.lang.IllegalStateException");
        assertThat(attr(s, LlmCall.TTFT_MS)).isNull();

        // null inputs are tolerated
        TrooperLlm.start(null, null, null).end();
        assertThat(spans()).hasSize(2);
        assertThat(spans().get(1).getAttributes().get(AttributeKey.stringKey("gen_ai.operation.name"))).isEqualTo("chat");
    }

    @Test
    void unknownClientsAreReturnedUnchanged() {
        Object o = new Object();
        assertThat(TrooperLlm.wrap(o)).isSameAs(o);
        assertThat(TrooperLlm.<Object>wrap(null)).isNull();
    }
}
