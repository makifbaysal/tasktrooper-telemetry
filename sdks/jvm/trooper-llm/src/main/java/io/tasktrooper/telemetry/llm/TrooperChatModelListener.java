package io.tasktrooper.telemetry.llm;

import dev.langchain4j.data.message.AiMessage;
import dev.langchain4j.data.message.ChatMessage;
import dev.langchain4j.data.message.SystemMessage;
import dev.langchain4j.data.message.ToolExecutionResultMessage;
import dev.langchain4j.data.message.UserMessage;
import dev.langchain4j.model.chat.listener.ChatModelErrorContext;
import dev.langchain4j.model.chat.listener.ChatModelListener;
import dev.langchain4j.model.chat.listener.ChatModelRequestContext;
import dev.langchain4j.model.chat.listener.ChatModelResponseContext;
import dev.langchain4j.model.chat.request.ChatRequest;
import dev.langchain4j.model.chat.response.ChatResponse;
import dev.langchain4j.model.output.TokenUsage;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;

/**
 * LangChain4j {@link ChatModelListener} that records every chat model call as a GenAI {@code chat} span
 * (contracts.md §25): provider, request / response model, max tokens, temperature, token usage, finish reason, errors,
 * and — with {@code TROOPER_LLM_CAPTURE_CONTENT=true} — the messages.
 *
 * <pre>{@code
 * ChatModel model = OpenAiChatModel.builder()
 *         .apiKey(key).modelName("gpt-4o-mini")
 *         .listeners(List.of(new TrooperChatModelListener()))
 *         .build();
 * }</pre>
 *
 * Requires {@code dev.langchain4j:langchain4j-core} 1.x (not a dependency of this module). The span is a child of the
 * caller's current span but is not made current itself (the listener can't scope the HTTP call), so the provider's
 * HTTP client span appears next to it. Never throws.
 */
public final class TrooperChatModelListener implements ChatModelListener {
    private static final Object CALL_KEY = TrooperChatModelListener.class.getName() + ".call";

    @Override
    public void onRequest(ChatModelRequestContext ctx) {
        try {
            ChatRequest req = ctx.chatRequest();
            String provider = provider(ctx.modelProvider() == null ? null : ctx.modelProvider().name());
            LlmCall call = TrooperLlm.start(provider, req == null ? null : req.modelName(), "chat");
            if (req != null) {
                if (req.maxOutputTokens() != null) call.maxTokens(req.maxOutputTokens());
                if (req.temperature() != null) call.temperature(req.temperature());
                if (call.captureContent()) content(call, req.messages());
            }
            ctx.attributes().put(CALL_KEY, call);
        } catch (Throwable ignored) {
            // never break the model call
        }
    }

    @Override
    public void onResponse(ChatModelResponseContext ctx) {
        try {
            if (!(ctx.attributes().get(CALL_KEY) instanceof LlmCall call)) return;
            ChatResponse res = ctx.chatResponse();
            LlmUsage usage = null;
            if (res != null) {
                call.responseModel(res.modelName());
                call.responseId(res.id());
                if (res.finishReason() != null) call.finishReasons(res.finishReason().name().toLowerCase(Locale.ROOT));
                TokenUsage tu = res.tokenUsage();
                if (tu != null) {
                    usage = new LlmUsage(toLong(tu.inputTokenCount()), toLong(tu.outputTokenCount()), null, null);
                }
                if (call.captureContent() && res.aiMessage() != null) {
                    call.outputMessages(GenAiJson.messages(List.of(new GenAiJson.Message("assistant",
                            res.aiMessage().text() == null ? List.of() : List.of(res.aiMessage().text()),
                            res.finishReason() == null ? null : res.finishReason().name().toLowerCase(Locale.ROOT)))));
                }
            }
            call.end(usage);
        } catch (Throwable ignored) {
            // never break the model call
        }
    }

    @Override
    public void onError(ChatModelErrorContext ctx) {
        try {
            if (ctx.attributes().get(CALL_KEY) instanceof LlmCall call) call.fail(ctx.error());
        } catch (Throwable ignored) {
            // never break the model call
        }
    }

    /** LangChain4j {@code ModelProvider} name → {@code gen_ai.provider.name}. */
    static String provider(String name) {
        if (name == null) return null;
        return switch (name) {
            case "OPEN_AI" -> "openai";
            case "ANTHROPIC" -> "anthropic";
            case "AMAZON_BEDROCK" -> "aws.bedrock";
            case "AZURE_OPEN_AI" -> "azure.ai.openai";
            case "GOOGLE_AI_GEMINI", "GOOGLE_GENAI" -> "gcp.gemini";
            case "GOOGLE_VERTEX_AI_GEMINI", "GOOGLE_VERTEX_AI_ANTHROPIC" -> "gcp.vertex_ai";
            case "MISTRAL_AI" -> "mistral_ai";
            case "MICROSOFT_FOUNDRY" -> "azure.ai.inference";
            case "GITHUB_MODELS" -> "github";
            case "WATSONX" -> "ibm.watsonx.ai";
            default -> name.toLowerCase(Locale.ROOT);
        };
    }

    private static void content(LlmCall call, List<ChatMessage> messages) {
        if (messages == null) return;
        List<GenAiJson.Message> in = new ArrayList<>();
        List<String> system = new ArrayList<>();
        for (ChatMessage m : messages) {
            if (m instanceof SystemMessage s) {
                system.add(s.text());
            } else if (m instanceof UserMessage u) {
                in.add(new GenAiJson.Message("user", u.hasSingleText() ? List.of(u.singleText()) : List.of(), null));
            } else if (m instanceof AiMessage a) {
                in.add(new GenAiJson.Message("assistant", a.text() == null ? List.of() : List.of(a.text()), null));
            } else if (m instanceof ToolExecutionResultMessage t) {
                in.add(new GenAiJson.Message("tool", t.text() == null ? List.of() : List.of(t.text()), null));
            }
        }
        if (!system.isEmpty()) call.systemInstructions(GenAiJson.parts(system));
        call.inputMessages(GenAiJson.messages(in));
    }

    private static Long toLong(Integer i) {
        return i == null ? null : i.longValue();
    }
}
