package io.tasktrooper.telemetry.llm;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Scope;
import java.util.Arrays;
import java.util.List;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * One LLM call as an OTel GenAI client span (contracts.md §25), created by {@link TrooperLlm#start}. All methods are
 * safe to call from any thread, never throw, and are no-ops after {@link #end}. Content setters
 * ({@link #inputMessages}, {@link #prompt} …) record only when content capture is on
 * ({@code TROOPER_LLM_CAPTURE_CONTENT=true}).
 *
 * <pre>{@code
 * LlmCall call = TrooperLlm.start("anthropic", "claude-sonnet-4-5", "chat");
 * try (Scope s = call.makeCurrent()) {
 *     for (Chunk c : stream) { call.firstToken(); ... }
 *     call.end(LlmUsage.of(in, out));
 * } catch (RuntimeException e) {
 *     call.fail(e);
 *     throw e;
 * }
 * }</pre>
 */
public final class LlmCall implements AutoCloseable {
    static final AttributeKey<String> RESPONSE_MODEL = AttributeKey.stringKey("gen_ai.response.model");
    static final AttributeKey<String> RESPONSE_ID = AttributeKey.stringKey("gen_ai.response.id");
    static final AttributeKey<List<String>> FINISH_REASONS = AttributeKey.stringArrayKey("gen_ai.response.finish_reasons");
    static final AttributeKey<Long> MAX_TOKENS = AttributeKey.longKey("gen_ai.request.max_tokens");
    static final AttributeKey<Double> TEMPERATURE = AttributeKey.doubleKey("gen_ai.request.temperature");
    static final AttributeKey<String> CONVERSATION_ID = AttributeKey.stringKey("gen_ai.conversation.id");
    static final AttributeKey<Long> INPUT_TOKENS = AttributeKey.longKey("gen_ai.usage.input_tokens");
    static final AttributeKey<Long> OUTPUT_TOKENS = AttributeKey.longKey("gen_ai.usage.output_tokens");
    static final AttributeKey<Long> CACHE_READ = AttributeKey.longKey("gen_ai.usage.cache_read.input_tokens");
    static final AttributeKey<Long> CACHE_CREATION = AttributeKey.longKey("gen_ai.usage.cache_creation.input_tokens");
    static final AttributeKey<Double> TTFT_MS = AttributeKey.doubleKey("trooper.llm.ttft_ms");
    static final AttributeKey<String> INPUT_MESSAGES = AttributeKey.stringKey("gen_ai.input.messages");
    static final AttributeKey<String> OUTPUT_MESSAGES = AttributeKey.stringKey("gen_ai.output.messages");
    static final AttributeKey<String> SYSTEM_INSTRUCTIONS = AttributeKey.stringKey("gen_ai.system_instructions");
    static final AttributeKey<String> ERROR_TYPE = AttributeKey.stringKey("error.type");

    private final Span span;
    private final long startNanos;
    private final boolean capture;
    private final AtomicBoolean firstToken = new AtomicBoolean();
    private final AtomicBoolean ended = new AtomicBoolean();

    LlmCall(Span span, boolean capture) {
        this.span = span;
        this.capture = capture;
        this.startNanos = System.nanoTime();
    }

    /** The underlying span (for custom attributes). */
    public Span span() {
        return span;
    }

    /** Makes the span current so the provider's HTTP client span nests under it (not counted twice). */
    public Scope makeCurrent() {
        try {
            return span.makeCurrent();
        } catch (Throwable t) {
            return Scope.noop();
        }
    }

    /** Whether prompts / completions are recorded for this call. */
    public boolean captureContent() {
        return capture;
    }

    public LlmCall maxTokens(long maxTokens) {
        return set(() -> span.setAttribute(MAX_TOKENS, maxTokens));
    }

    public LlmCall temperature(double temperature) {
        return set(() -> span.setAttribute(TEMPERATURE, temperature));
    }

    public LlmCall conversationId(String id) {
        return set(() -> { if (id != null) span.setAttribute(CONVERSATION_ID, id); });
    }

    public LlmCall attribute(String key, String value) {
        return set(() -> { if (key != null && value != null) span.setAttribute(key, value); });
    }

    public LlmCall attribute(String key, long value) {
        return set(() -> { if (key != null) span.setAttribute(key, value); });
    }

    /** Marks the first streamed token / chunk: records {@code trooper.llm.ttft_ms} once. */
    public LlmCall firstToken() {
        if (firstToken.compareAndSet(false, true) && !ended.get()) {
            double ms = (System.nanoTime() - startNanos) / 1_000_000.0;
            set(() -> span.setAttribute(TTFT_MS, ms));
        }
        return this;
    }

    public LlmCall responseModel(String model) {
        return set(() -> { if (model != null && !model.isBlank()) span.setAttribute(RESPONSE_MODEL, model); });
    }

    public LlmCall responseId(String id) {
        return set(() -> { if (id != null && !id.isBlank()) span.setAttribute(RESPONSE_ID, id); });
    }

    public LlmCall finishReasons(String... reasons) {
        return set(() -> {
            if (reasons != null && reasons.length > 0) {
                span.setAttribute(FINISH_REASONS, Arrays.stream(reasons).filter(r -> r != null).toList());
            }
        });
    }

    /** {@code gen_ai.input.messages} (GenAI JSON message array) — only with content capture. */
    public LlmCall inputMessages(String json) {
        return content(INPUT_MESSAGES, json);
    }

    /** {@code gen_ai.output.messages} (GenAI JSON message array) — only with content capture. */
    public LlmCall outputMessages(String json) {
        return content(OUTPUT_MESSAGES, json);
    }

    /** {@code gen_ai.system_instructions} (GenAI JSON parts array) — only with content capture. */
    public LlmCall systemInstructions(String json) {
        return content(SYSTEM_INSTRUCTIONS, json);
    }

    /** Convenience: a single user text message as {@code gen_ai.input.messages}. */
    public LlmCall prompt(String userText) {
        return capture ? inputMessages(GenAiJson.messages(List.of(new GenAiJson.Message("user", userText)))) : this;
    }

    /** Convenience: a single assistant text message as {@code gen_ai.output.messages}. */
    public LlmCall completion(String assistantText) {
        return capture ? outputMessages(GenAiJson.messages(List.of(new GenAiJson.Message("assistant", assistantText)))) : this;
    }

    /** Records usage and ends the span. */
    public void end(LlmUsage usage) {
        if (!ended.compareAndSet(false, true)) return;
        try {
            if (usage != null) {
                if (usage.inputTokens() != null) span.setAttribute(INPUT_TOKENS, usage.inputTokens());
                if (usage.outputTokens() != null) span.setAttribute(OUTPUT_TOKENS, usage.outputTokens());
                if (usage.cacheReadInputTokens() != null) span.setAttribute(CACHE_READ, usage.cacheReadInputTokens());
                if (usage.cacheCreationInputTokens() != null) span.setAttribute(CACHE_CREATION, usage.cacheCreationInputTokens());
            }
            span.end();
        } catch (Throwable ignored) {
            // never break the caller
        }
    }

    public void end() {
        end(null);
    }

    /** Records the error and ends the span. */
    public void fail(Throwable error) {
        if (ended.get()) return;
        try {
            if (error != null) {
                span.recordException(error);
                span.setAttribute(ERROR_TYPE, error.getClass().getName());
                span.setStatus(StatusCode.ERROR, String.valueOf(error.getMessage()));
            } else {
                span.setStatus(StatusCode.ERROR);
            }
        } catch (Throwable ignored) {
            // never break the caller
        }
        end(null);
    }

    /** Same as {@link #end()}; lets the call be used in try-with-resources. */
    @Override
    public void close() {
        end(null);
    }

    boolean isEnded() {
        return ended.get();
    }

    private LlmCall content(AttributeKey<String> key, String json) {
        if (!capture || json == null) return this;
        return set(() -> span.setAttribute(key, json));
    }

    private LlmCall set(Runnable r) {
        if (ended.get()) return this;
        try {
            r.run();
        } catch (Throwable ignored) {
            // never break the caller
        }
        return this;
    }
}
