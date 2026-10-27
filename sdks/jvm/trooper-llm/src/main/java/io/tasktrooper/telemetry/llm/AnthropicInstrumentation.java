package io.tasktrooper.telemetry.llm;

import io.opentelemetry.context.Scope;
import java.lang.reflect.InvocationHandler;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.ConcurrentHashMap;
import java.util.stream.Stream;

/**
 * Anthropic Java SDK ({@code com.anthropic:anthropic-java}) instrumentation without a compile or runtime dependency:
 * dynamic proxies over the SDK's public interfaces ({@code AnthropicClient}, {@code MessageService},
 * {@code StreamResponse}) and reflective reads of the response models, so any SDK version with the same accessor
 * names works. {@code messages().create(…)} and {@code messages().createStreaming(…)} (plain and structured params)
 * become GenAI {@code chat} spans; everything else is delegated untouched. Reading telemetry never throws into the
 * caller; the SDK's own exceptions are rethrown unchanged.
 */
final class AnthropicInstrumentation {
    static final String CLIENT = "com.anthropic.client.AnthropicClient";
    private static final String PROVIDER = "anthropic";
    private static final Map<String, Optional<Method>> METHODS = new ConcurrentHashMap<>();

    private AnthropicInstrumentation() {}

    static Class<?> clientInterface(Class<?> c) {
        for (Class<?> k = c; k != null; k = k.getSuperclass()) {
            for (Class<?> i : k.getInterfaces()) {
                if (i.getName().equals(CLIENT)) return i;
                Class<?> nested = clientInterface(i);
                if (nested != null) return nested;
            }
        }
        return null;
    }

    static Object wrapClient(Object client, Class<?> iface) {
        if (Proxy.isProxyClass(client.getClass()) && Proxy.getInvocationHandler(client) instanceof ClientHandler) {
            return client; // already wrapped
        }
        return Proxy.newProxyInstance(iface.getClassLoader(), new Class<?>[] {iface}, new ClientHandler(client));
    }

    private static Object proxy(Class<?> iface, InvocationHandler h) {
        return Proxy.newProxyInstance(iface.getClassLoader(), new Class<?>[] {iface}, h);
    }

    /** Invokes on the real object, unwrapping reflection's exception wrapper. */
    static Object call(Object target, Method m, Object[] args) throws Throwable {
        try {
            return m.invoke(target, args);
        } catch (InvocationTargetException e) {
            throw e.getCause();
        }
    }

    private static Object objectMethod(Object self, Object target, Method m, Object[] args) throws Throwable {
        return switch (m.getName()) {
            case "equals" -> self == args[0] || target.equals(args[0]);
            case "hashCode" -> target.hashCode();
            default -> call(target, m, args);
        };
    }

    private static final class ClientHandler implements InvocationHandler {
        private final Object target;

        ClientHandler(Object target) {
            this.target = target;
        }

        @Override
        public Object invoke(Object proxy, Method m, Object[] args) throws Throwable {
            if (m.getDeclaringClass() == Object.class) return objectMethod(proxy, target, m, args);
            Object result = call(target, m, args);
            if (result == null || !m.getReturnType().isInterface()) return result;
            try {
                if (m.getName().equals("messages") && m.getParameterCount() == 0) {
                    return proxy(m.getReturnType(), new MessagesHandler(result));
                }
                if (m.getName().equals("withOptions") && m.getReturnType().getName().equals(CLIENT)) {
                    return proxy(m.getReturnType(), new ClientHandler(result));
                }
            } catch (Throwable ignored) {
                // fall back to the unwrapped service
            }
            return result;
        }
    }

    private static final class MessagesHandler implements InvocationHandler {
        private final Object target;

        MessagesHandler(Object target) {
            this.target = target;
        }

        @Override
        public Object invoke(Object proxy, Method m, Object[] args) throws Throwable {
            if (m.getDeclaringClass() == Object.class) return objectMethod(proxy, target, m, args);
            String name = m.getName();
            boolean params = args != null && args.length >= 1 && args[0] != null
                    && args[0].getClass().getSimpleName().endsWith("MessageCreateParams");
            if (params && name.equals("create")) return create(m, args);
            if (params && name.equals("createStreaming")) return createStreaming(m, args);
            Object result = call(target, m, args);
            if (name.equals("withOptions") && result != null && m.getReturnType().isInterface()) {
                try {
                    return proxy(m.getReturnType(), new MessagesHandler(result));
                } catch (Throwable ignored) {
                    return result;
                }
            }
            return result;
        }

        private Object create(Method m, Object[] args) throws Throwable {
            LlmCall call = start(args[0]);
            Object result;
            try (Scope ignored = call.makeCurrent()) {
                result = call(target, m, args);
            } catch (Throwable t) {
                call.fail(t);
                throw t;
            }
            try {
                recordMessage(call, result);
                if (call.captureContent()) {
                    call.outputMessages(GenAiJson.messages(List.of(new GenAiJson.Message("assistant",
                            texts(get(result, "content")), str(get(result, "stopReason"))))));
                }
                call.end(usage(get(result, "usage")));
            } catch (Throwable t) {
                call.end();
            }
            return result;
        }

        private Object createStreaming(Method m, Object[] args) throws Throwable {
            LlmCall call = start(args[0]);
            Object result;
            try (Scope ignored = call.makeCurrent()) {
                result = call(target, m, args);
            } catch (Throwable t) {
                call.fail(t);
                throw t;
            }
            if (result == null || !m.getReturnType().isInterface()) {
                call.end();
                return result;
            }
            try {
                return proxy(m.getReturnType(), new StreamHandler(result, new StreamObserver(call)));
            } catch (Throwable t) {
                call.end();
                return result;
            }
        }
    }

    /** Proxies {@code StreamResponse}: {@code stream()}, {@code iterator()} (3.x) and {@code close()}. */
    private static final class StreamHandler implements InvocationHandler {
        private final Object target;
        private final StreamObserver observer;

        StreamHandler(Object target, StreamObserver observer) {
            this.target = target;
            this.observer = observer;
        }

        @Override
        public Object invoke(Object proxy, Method m, Object[] args) throws Throwable {
            if (m.getDeclaringClass() == Object.class) return objectMethod(proxy, target, m, args);
            switch (m.getName()) {
                case "stream" -> {
                    Object s = call(target, m, args);
                    if (s instanceof Stream<?> stream) {
                        return stream.peek(observer::onEvent).onClose(observer::finish);
                    }
                    return s;
                }
                case "iterator" -> {
                    Object it = call(target, m, args);
                    if (it instanceof Iterator<?> iterator) return new ObservedIterator<>(iterator, observer);
                    return it;
                }
                case "close" -> {
                    try {
                        return call(target, m, args);
                    } finally {
                        observer.finish();
                    }
                }
                default -> {
                    return call(target, m, args);
                }
            }
        }
    }

    private record ObservedIterator<T>(Iterator<T> delegate, StreamObserver observer) implements Iterator<T> {
        @Override
        public boolean hasNext() {
            try {
                boolean more = delegate.hasNext();
                if (!more) observer.finish();
                return more;
            } catch (RuntimeException e) {
                observer.fail(e);
                throw e;
            }
        }

        @Override
        public T next() {
            T v = delegate.next();
            observer.onEvent(v);
            return v;
        }
    }

    /** Reads {@code RawMessageStreamEvent}s: start (model, id, input usage), first delta (TTFT), message delta (output usage, stop reason). */
    static final class StreamObserver {
        private final LlmCall call;
        private Long input;
        private Long output;
        private Long cacheRead;
        private Long cacheCreation;
        private String stopReason;
        private final StringBuilder text = new StringBuilder();

        StreamObserver(LlmCall call) {
            this.call = call;
        }

        synchronized void onEvent(Object event) {
            try {
                if (event == null || call.isEnded()) return;
                if (bool(event, "isMessageStart")) {
                    Object message = get(get(event, "messageStart"), "message");
                    recordMessage(call, message);
                    Object u = get(message, "usage");
                    input = num(get(u, "inputTokens"), input);
                    output = num(get(u, "outputTokens"), output);
                    cacheRead = num(get(u, "cacheReadInputTokens"), cacheRead);
                    cacheCreation = num(get(u, "cacheCreationInputTokens"), cacheCreation);
                } else if (bool(event, "isContentBlockDelta") || bool(event, "isContentBlockStart")) {
                    call.firstToken();
                    if (call.captureContent() && bool(event, "isContentBlockDelta")) {
                        Object textDelta = get(get(get(event, "contentBlockDelta"), "delta"), "text");
                        Object t = get(textDelta, "text");
                        if (t instanceof String s && text.length() < 64 * 1024) text.append(s);
                    }
                } else if (bool(event, "isMessageDelta")) {
                    Object delta = get(event, "messageDelta");
                    Object u = get(delta, "usage");
                    output = num(get(u, "outputTokens"), output);
                    input = num(get(u, "inputTokens"), input);
                    cacheRead = num(get(u, "cacheReadInputTokens"), cacheRead);
                    cacheCreation = num(get(u, "cacheCreationInputTokens"), cacheCreation);
                    String reason = str(get(get(delta, "delta"), "stopReason"));
                    if (reason != null) stopReason = reason;
                } else if (bool(event, "isMessageStop")) {
                    finish(); // fully consumed: don't wait for close()
                }
            } catch (Throwable ignored) {
                // telemetry only
            }
        }

        synchronized void finish() {
            if (call.isEnded()) return;
            try {
                if (stopReason != null) call.finishReasons(stopReason);
                if (call.captureContent()) {
                    call.outputMessages(GenAiJson.messages(List.of(
                            new GenAiJson.Message("assistant", List.of(text.toString()), stopReason))));
                }
            } catch (Throwable ignored) {
                // telemetry only
            }
            call.end(new LlmUsage(input, output, cacheRead, cacheCreation));
        }

        synchronized void fail(Throwable t) {
            call.fail(t);
        }
    }

    // ---- request / response reading (reflective) ----

    private static LlmCall start(Object params) {
        Object model = get(params, "model");
        if (model == null) model = get(get(params, "rawParams"), "model");
        LlmCall call = TrooperLlm.start(PROVIDER, str(model), "chat");
        try {
            Object max = get(params, "maxTokens");
            if (max instanceof Number n) call.maxTokens(n.longValue());
            Object temp = get(params, "temperature");
            if (temp instanceof Number n) call.temperature(n.doubleValue());
            if (call.captureContent()) {
                Object system = get(params, "system");
                if (system != null) {
                    List<String> sys = new ArrayList<>();
                    if (bool(system, "isString")) {
                        sys.add(str(get(system, "asString")));
                    } else if (bool(system, "isTextBlockParams")) {
                        Object blocks = get(system, "asTextBlockParams");
                        if (blocks instanceof List<?> l) for (Object b : l) addText(sys, get(b, "text"));
                    }
                    if (!sys.isEmpty()) call.systemInstructions(GenAiJson.parts(sys));
                }
                Object messages = get(params, "messages");
                if (messages instanceof List<?> list) {
                    List<GenAiJson.Message> in = new ArrayList<>();
                    for (Object mp : list) {
                        Object content = get(mp, "content");
                        List<String> texts = new ArrayList<>();
                        if (bool(content, "isString")) {
                            addText(texts, get(content, "asString"));
                        } else if (bool(content, "isBlockParams")) {
                            texts.addAll(texts(get(content, "asBlockParams")));
                        }
                        in.add(new GenAiJson.Message(str(get(mp, "role")), texts, null));
                    }
                    call.inputMessages(GenAiJson.messages(in));
                }
            }
        } catch (Throwable ignored) {
            // telemetry only
        }
        return call;
    }

    private static void recordMessage(LlmCall call, Object message) {
        if (message == null) return;
        call.responseModel(str(get(message, "model")));
        call.responseId(str(get(message, "id")));
        String reason = str(get(message, "stopReason"));
        if (reason != null) call.finishReasons(reason);
    }

    private static LlmUsage usage(Object u) {
        if (u == null) return null;
        return new LlmUsage(num(get(u, "inputTokens"), null), num(get(u, "outputTokens"), null),
                num(get(u, "cacheReadInputTokens"), null), num(get(u, "cacheCreationInputTokens"), null));
    }

    /** Text of {@code ContentBlock} / {@code ContentBlockParam} lists (text blocks only). */
    private static List<String> texts(Object blocks) {
        List<String> out = new ArrayList<>();
        if (blocks instanceof List<?> list) {
            for (Object b : list) {
                if (bool(b, "isText")) addText(out, get(get(b, "asText"), "text"));
            }
        }
        return out;
    }

    private static void addText(List<String> out, Object text) {
        if (text instanceof String s) out.add(s);
    }

    /** {@code target.name()}, unwrapping {@code Optional}; null when missing or failing. */
    static Object get(Object target, String name) {
        if (target == null) return null;
        Method m = method(target.getClass(), name);
        if (m == null) return null;
        try {
            Object v = m.invoke(target);
            if (v instanceof Optional<?> o) return o.orElse(null);
            return v;
        } catch (Throwable t) {
            return null;
        }
    }

    private static boolean bool(Object target, String name) {
        return Boolean.TRUE.equals(get(target, name));
    }

    /** SDK enums ({@code Model}, {@code StopReason}, {@code Role}) expose {@code asString()}. */
    static String str(Object v) {
        if (v == null) return null;
        if (v instanceof String s) return s;
        Object s = get(v, "asString");
        return s instanceof String str ? str : v.toString();
    }

    private static Long num(Object v, Long fallback) {
        return v instanceof Number n ? Long.valueOf(n.longValue()) : fallback;
    }

    private static Method method(Class<?> c, String name) {
        return METHODS.computeIfAbsent(c.getName() + '#' + name + '@' + System.identityHashCode(c), k -> {
            try {
                return Optional.of(c.getMethod(name));
            } catch (Throwable t) {
                return Optional.empty();
            }
        }).orElse(null);
    }
}
