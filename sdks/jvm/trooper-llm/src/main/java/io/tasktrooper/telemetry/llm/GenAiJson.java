package io.tasktrooper.telemetry.llm;

import java.util.List;

/**
 * Minimal JSON for the GenAI message attributes ({@code gen_ai.input.messages}, {@code gen_ai.output.messages},
 * {@code gen_ai.system_instructions}) — text parts only. No JSON library dependency.
 */
public final class GenAiJson {
    private GenAiJson() {}

    /** One message part list entry: {@code {"role":…,"parts":[{"type":"text","content":…}]}}. */
    public record Message(String role, List<String> texts, String finishReason) {
        public Message(String role, String text) {
            this(role, text == null ? List.of() : List.of(text), null);
        }
    }

    /** {@code [{"role":"user","parts":[{"type":"text","content":"…"}]}, …]} */
    public static String messages(List<Message> messages) {
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < messages.size(); i++) {
            Message m = messages.get(i);
            if (i > 0) sb.append(',');
            sb.append("{\"role\":");
            str(sb, m.role() == null ? "user" : m.role());
            sb.append(",\"parts\":");
            parts(sb, m.texts());
            if (m.finishReason() != null) {
                sb.append(",\"finish_reason\":");
                str(sb, m.finishReason());
            }
            sb.append('}');
        }
        return sb.append(']').toString();
    }

    /** {@code [{"type":"text","content":"…"}, …]} (system instructions). */
    public static String parts(List<String> texts) {
        StringBuilder sb = new StringBuilder();
        parts(sb, texts);
        return sb.toString();
    }

    private static void parts(StringBuilder sb, List<String> texts) {
        sb.append('[');
        for (int i = 0; i < texts.size(); i++) {
            if (i > 0) sb.append(',');
            sb.append("{\"type\":\"text\",\"content\":");
            str(sb, texts.get(i));
            sb.append('}');
        }
        sb.append(']');
    }

    static void str(StringBuilder sb, String s) {
        sb.append('"');
        if (s != null) {
            for (int i = 0; i < s.length(); i++) {
                char c = s.charAt(i);
                switch (c) {
                    case '"' -> sb.append("\\\"");
                    case '\\' -> sb.append("\\\\");
                    case '\n' -> sb.append("\\n");
                    case '\r' -> sb.append("\\r");
                    case '\t' -> sb.append("\\t");
                    case '\b' -> sb.append("\\b");
                    case '\f' -> sb.append("\\f");
                    default -> {
                        if (c < 0x20) {
                            sb.append(String.format("\\u%04x", (int) c));
                        } else {
                            sb.append(c);
                        }
                    }
                }
            }
        }
        sb.append('"');
    }
}
