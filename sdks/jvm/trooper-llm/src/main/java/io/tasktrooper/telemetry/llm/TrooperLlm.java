package io.tasktrooper.telemetry.llm;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import java.util.Locale;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * LLM monitoring for the JVM (ADR-023, contracts.md §25): OTel GenAI client spans the writer turns into AI calls
 * (provider, model, tokens, cost, TTFT).
 *
 * <ul>
 *   <li>Manual: {@code LlmCall call = TrooperLlm.start("anthropic", model, "chat"); … call.firstToken(); …
 *       call.end(LlmUsage.of(in, out));} / {@code call.fail(e)}</li>
 *   <li>Anthropic Java SDK: {@code AnthropicClient client = TrooperLlm.wrap(AnthropicOkHttpClient.fromEnv());} —
 *       {@code messages().create(…)} and {@code createStreaming(…)} (tokens, cache tokens, TTFT, stop reason)</li>
 *   <li>OpenAI Java SDK: instrumented by the javaagent out of the box; without the agent {@code wrap(client)} delegates
 *       to {@code io.opentelemetry.instrumentation:opentelemetry-openai-java-1.1} if it is on the classpath</li>
 *   <li>LangChain4j: {@link TrooperChatModelListener} on the chat model's {@code listeners(…)}</li>
 *   <li>Spring AI: its Micrometer observations already are GenAI spans; the SDK normalizes them on export</li>
 * </ul>
 *
 * Content (prompts / completions) is recorded only with {@code TROOPER_LLM_CAPTURE_CONTENT=true}
 * ({@code -Dtrooper.llm.capture-content=true}, Spring {@code trooper.llm.capture-content}). Nothing here throws into
 * the application.
 */
public final class TrooperLlm {
    public static final String INSTRUMENTATION_NAME = "io.tasktrooper.llm";
    public static final String CAPTURE_CONTENT_PROPERTY = "trooper.llm.capture-content";
    public static final String CAPTURE_CONTENT_ENV = "TROOPER_LLM_CAPTURE_CONTENT";

    static final AttributeKey<String> OPERATION = AttributeKey.stringKey("gen_ai.operation.name");
    static final AttributeKey<String> PROVIDER = AttributeKey.stringKey("gen_ai.provider.name");
    static final AttributeKey<String> REQUEST_MODEL = AttributeKey.stringKey("gen_ai.request.model");

    private static final Logger LOG = Logger.getLogger(TrooperLlm.class.getName());
    private static volatile Boolean captureOverride;
    private static volatile OpenTelemetry openTelemetry;

    private TrooperLlm() {}

    /** Starts a {@code chat} call. */
    public static LlmCall start(String provider, String model) {
        return start(provider, model, "chat");
    }

    /**
     * Starts a GenAI client span {@code "<operation> <model>"} under the current context.
     *
     * @param provider {@code gen_ai.provider.name}: {@code openai}, {@code anthropic}, {@code aws.bedrock},
     *     {@code gcp.gemini}, {@code azure.ai.openai} …
     * @param model requested model ({@code gen_ai.request.model})
     * @param operation {@code chat}, {@code text_completion}, {@code embeddings}, {@code generate_content},
     *     {@code execute_tool}, {@code invoke_agent} …
     */
    public static LlmCall start(String provider, String model, String operation) {
        boolean capture = captureContent();
        try {
            String op = operation == null || operation.isBlank() ? "chat" : operation;
            Tracer tracer = otel().getTracer(INSTRUMENTATION_NAME);
            Span span = tracer.spanBuilder(model == null || model.isBlank() ? op : op + " " + model)
                    .setSpanKind(SpanKind.CLIENT)
                    .setAttribute(OPERATION, op)
                    .startSpan();
            if (provider != null && !provider.isBlank()) span.setAttribute(PROVIDER, provider.toLowerCase(Locale.ROOT));
            if (model != null && !model.isBlank()) span.setAttribute(REQUEST_MODEL, model);
            return new LlmCall(span, capture);
        } catch (Throwable t) {
            LOG.log(Level.FINE, "TaskTrooper: cannot start LLM span", t);
            return new LlmCall(Span.getInvalid(), false);
        }
    }

    /**
     * Instruments an LLM client and returns the instrumented instance (same interface), or {@code client} unchanged
     * when it is not supported. Supported: Anthropic Java SDK {@code AnthropicClient}; OpenAI Java SDK
     * {@code OpenAIClient} via the upstream OTel library instrumentation when present (left alone under the javaagent,
     * which instruments it already).
     */
    @SuppressWarnings("unchecked")
    public static <T> T wrap(T client) {
        if (client == null) return null;
        try {
            Class<?> anthropic = AnthropicInstrumentation.clientInterface(client.getClass());
            if (anthropic != null) {
                return (T) AnthropicInstrumentation.wrapClient(client, anthropic);
            }
            if (implementsInterface(client.getClass(), "com.openai.client.OpenAIClient")) {
                return (T) wrapOpenAi(client);
            }
        } catch (Throwable t) {
            LOG.log(Level.FINE, "TaskTrooper: cannot instrument " + client.getClass().getName(), t);
        }
        return client;
    }

    /**
     * Overrides the content capture setting ({@code null} = read {@code trooper.llm.capture-content} /
     * {@code TROOPER_LLM_CAPTURE_CONTENT} again). The Spring Boot starter sets it from the application config.
     */
    public static void setCaptureContent(Boolean capture) {
        captureOverride = capture;
    }

    /** Whether prompts / completions are recorded (default false). */
    public static boolean captureContent() {
        Boolean o = captureOverride;
        if (o != null) return o;
        try {
            String v = System.getProperty(CAPTURE_CONTENT_PROPERTY);
            if (v == null || v.isBlank()) v = System.getenv(CAPTURE_CONTENT_ENV);
            return v != null && Boolean.parseBoolean(v.trim());
        } catch (Throwable t) {
            return false;
        }
    }

    /** Uses {@code otel} instead of {@link GlobalOpenTelemetry} ({@code null} resets). */
    public static void setOpenTelemetry(OpenTelemetry otel) {
        openTelemetry = otel;
    }

    static OpenTelemetry otel() {
        OpenTelemetry o = openTelemetry;
        return o != null ? o : GlobalOpenTelemetry.get();
    }

    private static Object wrapOpenAi(Object client) throws Exception {
        if (javaagentPresent()) return client; // the agent's openai-java instrumentation already covers it
        Class<?> telemetry;
        try {
            telemetry = Class.forName("io.opentelemetry.instrumentation.openai.v1_1.OpenAITelemetry", true, client.getClass().getClassLoader());
        } catch (ClassNotFoundException e) {
            LOG.log(Level.INFO, "TaskTrooper: add io.opentelemetry.instrumentation:opentelemetry-openai-java-1.1 to trace the "
                    + "OpenAI Java SDK without the javaagent (or use TrooperLlm.start)");
            return client;
        }
        Object instance = telemetry.getMethod("create", OpenTelemetry.class).invoke(null, otel());
        for (java.lang.reflect.Method m : telemetry.getMethods()) {
            if (m.getName().equals("wrap") && m.getParameterCount() == 1 && m.getParameterTypes()[0].isInstance(client)) {
                return m.invoke(instance, client);
            }
        }
        return client;
    }

    static boolean javaagentPresent() {
        try {
            Class.forName("io.opentelemetry.javaagent.bootstrap.AgentInitializer", false, null);
            return true;
        } catch (Throwable t) {
            return false;
        }
    }

    static boolean implementsInterface(Class<?> c, String name) {
        for (Class<?> k = c; k != null; k = k.getSuperclass()) {
            for (Class<?> i : k.getInterfaces()) {
                if (i.getName().equals(name) || implementsInterface(i, name)) return true;
            }
        }
        return false;
    }
}
