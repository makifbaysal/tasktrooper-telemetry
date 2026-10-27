package io.tasktrooper.telemetry.llm;

import static org.assertj.core.api.Assertions.assertThat;

import dev.langchain4j.data.message.AiMessage;
import dev.langchain4j.data.message.SystemMessage;
import dev.langchain4j.data.message.UserMessage;
import dev.langchain4j.model.ModelProvider;
import dev.langchain4j.model.chat.listener.ChatModelErrorContext;
import dev.langchain4j.model.chat.listener.ChatModelRequestContext;
import dev.langchain4j.model.chat.listener.ChatModelResponseContext;
import dev.langchain4j.model.chat.request.ChatRequest;
import dev.langchain4j.model.chat.response.ChatResponse;
import dev.langchain4j.model.output.FinishReason;
import dev.langchain4j.model.output.TokenUsage;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.sdk.trace.data.SpanData;
import java.util.HashMap;
import java.util.Map;
import org.junit.jupiter.api.Test;

class TrooperChatModelListenerTest extends LlmTestSupport {
    private final TrooperChatModelListener listener = new TrooperChatModelListener();

    private static ChatRequest request() {
        return ChatRequest.builder()
                .messages(SystemMessage.from("be brief"), UserMessage.from("hi"))
                .modelName("gpt-4o-mini")
                .maxOutputTokens(100)
                .temperature(0.3)
                .build();
    }

    @Test
    void responseBecomesChatSpan() {
        TrooperLlm.setCaptureContent(true);
        Map<Object, Object> attrs = new HashMap<>();
        ChatRequest req = request();
        listener.onRequest(new ChatModelRequestContext(req, ModelProvider.OPEN_AI, attrs));
        ChatResponse res = ChatResponse.builder()
                .aiMessage(AiMessage.from("hello"))
                .id("chatcmpl-1")
                .modelName("gpt-4o-mini-2024-07-18")
                .tokenUsage(new TokenUsage(11, 3))
                .finishReason(FinishReason.STOP)
                .build();
        listener.onResponse(new ChatModelResponseContext(res, req, ModelProvider.OPEN_AI, attrs));

        SpanData s = only();
        assertThat(s.getName()).isEqualTo("chat gpt-4o-mini");
        assertThat(str(s, "gen_ai.provider.name")).isEqualTo("openai");
        assertThat(str(s, "gen_ai.response.model")).isEqualTo("gpt-4o-mini-2024-07-18");
        assertThat(str(s, "gen_ai.response.id")).isEqualTo("chatcmpl-1");
        assertThat(lng(s, "gen_ai.usage.input_tokens")).isEqualTo(11L);
        assertThat(lng(s, "gen_ai.usage.output_tokens")).isEqualTo(3L);
        assertThat(lng(s, "gen_ai.request.max_tokens")).isEqualTo(100L);
        assertThat(attr(s, LlmCall.FINISH_REASONS)).containsExactly("stop");
        assertThat(str(s, "gen_ai.system_instructions")).isEqualTo("[{\"type\":\"text\",\"content\":\"be brief\"}]");
        assertThat(str(s, "gen_ai.input.messages")).isEqualTo("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"hi\"}]}]");
        assertThat(str(s, "gen_ai.output.messages")).contains("\"content\":\"hello\"");
    }

    @Test
    void errorEndsSpanWithErrorAndProviderIsMapped() {
        Map<Object, Object> attrs = new HashMap<>();
        listener.onRequest(new ChatModelRequestContext(request(), ModelProvider.AMAZON_BEDROCK, attrs));
        listener.onError(new ChatModelErrorContext(new RuntimeException("throttled"), request(), ModelProvider.AMAZON_BEDROCK, attrs));
        SpanData s = only();
        assertThat(str(s, "gen_ai.provider.name")).isEqualTo("aws.bedrock");
        assertThat(s.getStatus().getStatusCode()).isEqualTo(StatusCode.ERROR);
        assertThat(str(s, "gen_ai.input.messages")).isNull(); // capture off

        // response without a request context is ignored
        listener.onResponse(new ChatModelResponseContext(ChatResponse.builder().aiMessage(AiMessage.from("x")).build(),
                request(), ModelProvider.OTHER, new HashMap<>()));
        assertThat(spans()).hasSize(1);
        assertThat(TrooperChatModelListener.provider("GOOGLE_VERTEX_AI_GEMINI")).isEqualTo("gcp.vertex_ai");
        assertThat(TrooperChatModelListener.provider("OLLAMA")).isEqualTo("ollama");
    }
}
