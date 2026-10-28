package io.tasktrooper.telemetry.llm;

/**
 * Token usage of one LLM call ({@code gen_ai.usage.*}, contracts.md §25). {@code null} = unknown / not reported.
 *
 * @param inputTokens {@code gen_ai.usage.input_tokens}
 * @param outputTokens {@code gen_ai.usage.output_tokens}
 * @param cacheReadInputTokens {@code gen_ai.usage.cache_read.input_tokens} (prompt cache hits)
 * @param cacheCreationInputTokens {@code gen_ai.usage.cache_creation.input_tokens} (prompt cache writes)
 */
public record LlmUsage(Long inputTokens, Long outputTokens, Long cacheReadInputTokens, Long cacheCreationInputTokens) {

    public static LlmUsage of(long inputTokens, long outputTokens) {
        return new LlmUsage(inputTokens, outputTokens, null, null);
    }

    public LlmUsage withCache(Long cacheRead, Long cacheCreation) {
        return new LlmUsage(inputTokens, outputTokens, cacheRead, cacheCreation);
    }
}
