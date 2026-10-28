package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.AttributeType;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.trace.data.DelegatingSpanData;
import io.opentelemetry.sdk.trace.data.EventData;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.util.ArrayList;
import java.util.Collection;
import java.util.List;
import java.util.Locale;
import java.util.Map;

/**
 * Normalizes GenAI spans to the attribute names the writer reads (contracts.md §25) before export. Spans without
 * {@code gen_ai.operation.name} / {@code gen_ai.system} / {@code spring.ai.kind} pass through untouched (three
 * attribute lookups).
 *
 * <ul>
 *   <li>{@code gen_ai.system} → {@code gen_ai.provider.name} (when missing), with Spring AI / vendor values mapped
 *       ({@code azure-openai} → {@code azure.ai.openai}, {@code bedrock_converse} → {@code aws.bedrock},
 *       {@code vertex_ai} → {@code gcp.vertex_ai}, {@code google_genai} → {@code gcp.gemini} …)</li>
 *   <li>Spring AI framework observations ({@code gen_ai.operation.name=framework}: ChatClient, advisors, vector
 *       stores) lose {@code gen_ai.operation.name} so they don't count as extra LLM calls; Spring AI tool calls become
 *       {@code execute_tool} + {@code gen_ai.tool.name}; {@code embedding} → {@code embeddings}</li>
 *   <li>Micrometer-bridged values arrive as strings: token counts / {@code max_tokens} become longs, temperature /
 *       top_p doubles, {@code gen_ai.response.finish_reasons} a string array; legacy
 *       {@code gen_ai.usage.prompt_tokens|completion_tokens} → {@code input|output_tokens}</li>
 *   <li>Content: {@code captureContent == FALSE} strips prompt / completion attributes and content events (even when a
 *       library recorded them); otherwise Spring AI's {@code gen_ai.prompt} / {@code gen_ai.completion} (attribute or
 *       {@code gen_ai.content.*} event) are copied to {@code gen_ai.input.messages} / {@code gen_ai.output.messages}</li>
 * </ul>
 *
 * Never throws: on any error the original span is exported.
 */
public final class GenAiSpans {
    static final AttributeKey<String> OPERATION = AttributeKey.stringKey("gen_ai.operation.name");
    static final AttributeKey<String> PROVIDER = AttributeKey.stringKey("gen_ai.provider.name");
    static final AttributeKey<String> SYSTEM = AttributeKey.stringKey("gen_ai.system");
    static final AttributeKey<String> SPRING_AI_KIND = AttributeKey.stringKey("spring.ai.kind");
    static final AttributeKey<String> SPRING_AI_TOOL_NAME = AttributeKey.stringKey("spring.ai.tool.definition.name");
    static final AttributeKey<String> TOOL_NAME = AttributeKey.stringKey("gen_ai.tool.name");
    static final String INPUT_MESSAGES = "gen_ai.input.messages";
    static final String OUTPUT_MESSAGES = "gen_ai.output.messages";
    static final String SYSTEM_INSTRUCTIONS = "gen_ai.system_instructions";

    private static final List<String> LONG_KEYS = List.of(
            "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.total_tokens",
            "gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_creation.input_tokens",
            "gen_ai.request.max_tokens", "gen_ai.request.top_k", "gen_ai.request.seed", "gen_ai.request.choice.count");
    private static final List<String> DOUBLE_KEYS = List.of(
            "gen_ai.request.temperature", "gen_ai.request.top_p", "gen_ai.request.frequency_penalty",
            "gen_ai.request.presence_penalty", "trooper.llm.ttft_ms");
    private static final Map<String, String> RENAMED = Map.of(
            "gen_ai.usage.prompt_tokens", "gen_ai.usage.input_tokens",
            "gen_ai.usage.completion_tokens", "gen_ai.usage.output_tokens");
    private static final Map<String, String> PROVIDERS = Map.ofEntries(
            Map.entry("azure-openai", "azure.ai.openai"),
            Map.entry("azure_openai", "azure.ai.openai"),
            Map.entry("az.ai.openai", "azure.ai.openai"),
            Map.entry("az.ai.inference", "azure.ai.inference"),
            Map.entry("bedrock_converse", "aws.bedrock"),
            Map.entry("bedrock", "aws.bedrock"),
            Map.entry("aws_bedrock", "aws.bedrock"),
            Map.entry("vertex_ai", "gcp.vertex_ai"),
            Map.entry("vertexai", "gcp.vertex_ai"),
            Map.entry("google_genai", "gcp.gemini"),
            Map.entry("gemini", "gcp.gemini"),
            Map.entry("mistral", "mistral_ai"),
            Map.entry("oci_genai", "oci.genai"),
            Map.entry("minimax", "minimax"),
            Map.entry("zhipuai", "zhipu_ai"));

    private GenAiSpans() {}

    /** Wraps an exporter so GenAI spans are normalized first. */
    public static SpanExporter wrap(SpanExporter delegate, Boolean captureContent) {
        return new Exporter(delegate, captureContent);
    }

    /** Normalized copy of {@code span}, or {@code span} itself when nothing changes. */
    public static SpanData normalize(SpanData span, Boolean captureContent) {
        try {
            Attributes attrs = span.getAttributes();
            String op = attrs.get(OPERATION);
            String system = attrs.get(SYSTEM);
            String springKind = attrs.get(SPRING_AI_KIND);
            if (op == null && system == null && springKind == null) {
                return span;
            }
            AttributesBuilder b = Attributes.builder();
            boolean strip = Boolean.FALSE.equals(captureContent);
            boolean[] changed = {false};
            attrs.forEach((k, v) -> {
                String name = k.getKey();
                if (strip && isContentKey(name)) {
                    changed[0] = true;
                    return;
                }
                String target = RENAMED.get(name);
                if (target != null) {
                    changed[0] = true;
                    if (attrs.get(AttributeKey.stringKey(target)) == null && attrs.get(AttributeKey.longKey(target)) == null) {
                        Long n = toLong(v);
                        if (n != null) b.put(AttributeKey.longKey(target), n);
                    }
                    return;
                }
                if (v instanceof String s && k.getType() == AttributeType.STRING) {
                    if (LONG_KEYS.contains(name)) {
                        Long n = toLong(s);
                        if (n != null) {
                            b.put(AttributeKey.longKey(name), n);
                            changed[0] = true;
                            return;
                        }
                    } else if (DOUBLE_KEYS.contains(name)) {
                        try {
                            b.put(AttributeKey.doubleKey(name), Double.parseDouble(s.trim()));
                            changed[0] = true;
                            return;
                        } catch (NumberFormatException ignored) {
                            // keep the string
                        }
                    } else if ("gen_ai.response.finish_reasons".equals(name)) {
                        b.put(AttributeKey.stringArrayKey(name), splitList(s));
                        changed[0] = true;
                        return;
                    }
                }
                putRaw(b, k, v);
            });

            // operation name
            String newOp = op;
            if ("tool_call".equals(springKind)) {
                newOp = "execute_tool";
                String tool = attrs.get(SPRING_AI_TOOL_NAME);
                if (tool != null && attrs.get(TOOL_NAME) == null) b.put(TOOL_NAME, tool);
            } else if ("framework".equals(op) || (springKind != null && !"chat_model".equals(springKind)
                    && !"embedding_model".equals(springKind) && !"image_model".equals(springKind))) {
                newOp = null; // ChatClient / advisor / vector store observation: not an LLM call
            } else if ("embedding".equals(op)) {
                newOp = "embeddings";
            }
            if (!java.util.Objects.equals(newOp, op)) {
                changed[0] = true;
                if (newOp == null) {
                    b.removeIf(k -> k.getKey().equals(OPERATION.getKey()));
                } else {
                    b.put(OPERATION, newOp);
                }
            }
            // provider
            if (attrs.get(PROVIDER) == null && system != null && newOp != null) {
                String provider = provider(system);
                if (provider != null) {
                    b.put(PROVIDER, provider);
                    changed[0] = true;
                }
            }
            if (newOp == null && "spring_ai".equals(system)) {
                b.removeIf(k -> k.getKey().equals(SYSTEM.getKey()));
                changed[0] = true;
            }

            // content
            List<EventData> events = span.getEvents();
            List<EventData> keptEvents = events;
            if (strip) {
                keptEvents = new ArrayList<>(events.size());
                for (EventData e : events) {
                    if (isContentEvent(e)) {
                        changed[0] = true;
                    } else {
                        keptEvents.add(e);
                    }
                }
            } else if (newOp != null) {
                String in = firstNonNull(asText(attrs, "gen_ai.prompt"), eventText(events, "gen_ai.content.prompt", "gen_ai.prompt"));
                String out = firstNonNull(asText(attrs, "gen_ai.completion"), eventText(events, "gen_ai.content.completion", "gen_ai.completion"));
                if (in != null && attrs.get(AttributeKey.stringKey(INPUT_MESSAGES)) == null) {
                    b.put(INPUT_MESSAGES, in);
                    changed[0] = true;
                }
                if (out != null && attrs.get(AttributeKey.stringKey(OUTPUT_MESSAGES)) == null) {
                    b.put(OUTPUT_MESSAGES, out);
                    changed[0] = true;
                }
            }
            if (!changed[0]) return span;
            return new Normalized(span, b.build(), keptEvents);
        } catch (Throwable t) {
            SdkErrors.record("genai", t);
            return span;
        }
    }

    static String provider(String system) {
        String s = system.trim().toLowerCase(Locale.ROOT);
        if (s.isEmpty() || "spring_ai".equals(s)) return null;
        return PROVIDERS.getOrDefault(s, s);
    }

    static boolean isContentKey(String name) {
        return name.equals(INPUT_MESSAGES) || name.equals(OUTPUT_MESSAGES) || name.equals(SYSTEM_INSTRUCTIONS)
                || name.equals("gen_ai.prompt") || name.equals("gen_ai.completion")
                || name.startsWith("gen_ai.prompt.") || name.startsWith("gen_ai.completion.")
                || name.equals("gen_ai.tool.call.arguments") || name.equals("gen_ai.tool.call.result");
    }

    private static boolean isContentEvent(EventData e) {
        String n = e.getName();
        if (n.startsWith("gen_ai.content.") || n.equals("gen_ai.client.inference.operation.details")
                || n.endsWith(".message") && n.startsWith("gen_ai.") || n.equals("gen_ai.choice")) {
            return true;
        }
        boolean[] content = {false};
        e.getAttributes().forEach((k, v) -> content[0] |= isContentKey(k.getKey()));
        return content[0];
    }

    private static String asText(Attributes attrs, String key) {
        String s = attrs.get(AttributeKey.stringKey(key));
        if (s != null) return s;
        List<String> list = attrs.get(AttributeKey.stringArrayKey(key));
        return list == null || list.isEmpty() ? null : String.join("\n", list);
    }

    private static String eventText(List<EventData> events, String eventName, String key) {
        for (EventData e : events) {
            if (e.getName().equals(eventName)) {
                String s = asText(e.getAttributes(), key);
                if (s != null) return s;
            }
        }
        return null;
    }

    private static String firstNonNull(String a, String b) {
        return a != null ? a : b;
    }

    static List<String> splitList(String s) {
        String t = s.trim();
        if (t.startsWith("[") && t.endsWith("]")) t = t.substring(1, t.length() - 1);
        List<String> out = new ArrayList<>();
        for (String p : t.split(",")) {
            String v = p.trim();
            if (v.length() >= 2 && (v.startsWith("\"") && v.endsWith("\"") || v.startsWith("'") && v.endsWith("'"))) {
                v = v.substring(1, v.length() - 1);
            }
            if (!v.isEmpty()) out.add(v);
        }
        return out;
    }

    private static Long toLong(Object v) {
        if (v instanceof Long l) return l;
        if (v instanceof Number n) return n.longValue();
        if (v instanceof String s) {
            try {
                return Long.parseLong(s.trim());
            } catch (NumberFormatException e) {
                try {
                    double d = Double.parseDouble(s.trim());
                    return Double.isFinite(d) ? (long) d : null;
                } catch (NumberFormatException ignored) {
                    return null;
                }
            }
        }
        return null;
    }

    @SuppressWarnings("unchecked")
    private static void putRaw(AttributesBuilder b, AttributeKey<?> k, Object v) {
        b.put((AttributeKey<Object>) k, v);
    }

    private static final class Normalized extends DelegatingSpanData {
        private final Attributes attributes;
        private final List<EventData> events;

        Normalized(SpanData delegate, Attributes attributes, List<EventData> events) {
            super(delegate);
            this.attributes = attributes;
            this.events = events;
        }

        @Override
        public Attributes getAttributes() {
            return attributes;
        }

        @Override
        public int getTotalAttributeCount() {
            return Math.max(attributes.size(), super.getTotalAttributeCount() - (super.getAttributes().size() - attributes.size()));
        }

        @Override
        public List<EventData> getEvents() {
            return events;
        }

        @Override
        public int getTotalRecordedEvents() {
            return super.getTotalRecordedEvents() - (super.getEvents().size() - events.size());
        }
    }

    private static final class Exporter implements SpanExporter {
        private final SpanExporter delegate;
        private final Boolean captureContent;

        Exporter(SpanExporter delegate, Boolean captureContent) {
            this.delegate = delegate;
            this.captureContent = captureContent;
        }

        @Override
        public CompletableResultCode export(Collection<SpanData> spans) {
            List<SpanData> out = null;
            int i = 0;
            for (SpanData s : spans) {
                SpanData n = normalize(s, captureContent);
                if (n != s && out == null) {
                    out = new ArrayList<>(spans.size());
                    int j = 0;
                    for (SpanData prev : spans) {
                        if (j++ >= i) break;
                        out.add(prev);
                    }
                }
                if (out != null) out.add(n);
                i++;
            }
            return delegate.export(out != null ? out : spans);
        }

        @Override
        public CompletableResultCode flush() {
            return delegate.flush();
        }

        @Override
        public CompletableResultCode shutdown() {
            return delegate.shutdown();
        }

        @Override
        public void close() {
            delegate.close();
        }

        @Override
        public String toString() {
            return "GenAiSpanExporter{" + delegate + "}";
        }
    }
}
