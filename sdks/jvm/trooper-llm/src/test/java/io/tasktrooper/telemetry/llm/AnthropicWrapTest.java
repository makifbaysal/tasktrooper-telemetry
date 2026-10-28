package io.tasktrooper.telemetry.llm;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import com.anthropic.client.AnthropicClient;
import com.anthropic.client.okhttp.AnthropicOkHttpClient;
import com.anthropic.core.http.StreamResponse;
import com.anthropic.models.messages.Message;
import com.anthropic.models.messages.MessageCreateParams;
import com.anthropic.models.messages.RawMessageStreamEvent;
import com.sun.net.httpserver.HttpServer;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.sdk.trace.data.SpanData;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** The real Anthropic Java SDK against a local fake of the Messages API. */
class AnthropicWrapTest extends LlmTestSupport {
    private static final String MESSAGE = """
            {"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929",
             "content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn","stop_sequence":null,
             "usage":{"input_tokens":12,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}""";
    private static final String STREAM = """
            event: message_start
            data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":1,"cache_read_input_tokens":4}}}

            event: content_block_start
            data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

            event: content_block_delta
            data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

            event: content_block_delta
            data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

            event: content_block_stop
            data: {"type":"content_block_stop","index":0}

            event: message_delta
            data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":7}}

            event: message_stop
            data: {"type":"message_stop"}

            """;

    private HttpServer server;
    private final List<String> requests = new CopyOnWriteArrayList<>();
    private final AtomicInteger status = new AtomicInteger(200);
    private AnthropicClient client;

    @BeforeEach
    void start() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/messages", ex -> {
            String body = new String(ex.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
            requests.add(body);
            boolean stream = body.contains("\"stream\":true");
            byte[] out = (status.get() != 200
                    ? "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad\"}}"
                    : stream ? STREAM : MESSAGE).getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().add("Content-Type", stream && status.get() == 200 ? "text/event-stream" : "application/json");
            ex.sendResponseHeaders(status.get(), out.length);
            ex.getResponseBody().write(out);
            ex.close();
        });
        server.start();
        client = TrooperLlm.wrap(AnthropicOkHttpClient.builder()
                .apiKey("test-key")
                .baseUrl("http://127.0.0.1:" + server.getAddress().getPort())
                .maxRetries(0)
                .build());
    }

    @AfterEach
    void stop() {
        client.close();
        server.stop(0);
    }

    private static MessageCreateParams params() {
        return MessageCreateParams.builder()
                .model("claude-sonnet-4-5")
                .maxTokens(256)
                .system("You are terse.")
                .addUserMessage("Say hello")
                .build();
    }

    @Test
    void createRecordsModelTokensAndStopReason() {
        Message m = client.messages().create(params());
        assertThat(m.id()).isEqualTo("msg_1");

        SpanData s = only();
        assertThat(s.getName()).isEqualTo("chat claude-sonnet-4-5");
        assertThat(s.getKind()).isEqualTo(SpanKind.CLIENT);
        assertThat(str(s, "gen_ai.provider.name")).isEqualTo("anthropic");
        assertThat(str(s, "gen_ai.operation.name")).isEqualTo("chat");
        assertThat(str(s, "gen_ai.request.model")).isEqualTo("claude-sonnet-4-5");
        assertThat(str(s, "gen_ai.response.model")).isEqualTo("claude-sonnet-4-5-20250929");
        assertThat(str(s, "gen_ai.response.id")).isEqualTo("msg_1");
        assertThat(attr(s, LlmCall.FINISH_REASONS)).containsExactly("end_turn");
        assertThat(lng(s, "gen_ai.request.max_tokens")).isEqualTo(256L);
        assertThat(lng(s, "gen_ai.usage.input_tokens")).isEqualTo(12L);
        assertThat(lng(s, "gen_ai.usage.output_tokens")).isEqualTo(5L);
        assertThat(lng(s, "gen_ai.usage.cache_read.input_tokens")).isEqualTo(3L);
        assertThat(lng(s, "gen_ai.usage.cache_creation.input_tokens")).isEqualTo(0L);
        assertThat(str(s, "gen_ai.input.messages")).isNull();
        assertThat(requests).hasSize(1);
    }

    @Test
    void contentCaptureRecordsMessages() {
        TrooperLlm.setCaptureContent(true);
        client.messages().create(params());
        SpanData s = only();
        assertThat(str(s, "gen_ai.input.messages"))
                .isEqualTo("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"Say hello\"}]}]");
        assertThat(str(s, "gen_ai.system_instructions")).isEqualTo("[{\"type\":\"text\",\"content\":\"You are terse.\"}]");
        assertThat(str(s, "gen_ai.output.messages")).isEqualTo(
                "[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"Hello!\"}],\"finish_reason\":\"end_turn\"}]");
    }

    @Test
    void streamingRecordsUsageAndTimeToFirstToken() {
        TrooperLlm.setCaptureContent(true);
        try (StreamResponse<RawMessageStreamEvent> stream = client.messages().createStreaming(params())) {
            assertThat(stream.stream().count()).isEqualTo(7);
        }
        SpanData s = only();
        assertThat(str(s, "gen_ai.response.model")).isEqualTo("claude-haiku-4-5");
        assertThat(str(s, "gen_ai.response.id")).isEqualTo("msg_2");
        assertThat(lng(s, "gen_ai.usage.input_tokens")).isEqualTo(20L);
        assertThat(lng(s, "gen_ai.usage.output_tokens")).isEqualTo(7L);
        assertThat(lng(s, "gen_ai.usage.cache_read.input_tokens")).isEqualTo(4L);
        assertThat(attr(s, LlmCall.FINISH_REASONS)).containsExactly("max_tokens");
        assertThat(attr(s, LlmCall.TTFT_MS)).isNotNull().isPositive();
        assertThat(str(s, "gen_ai.output.messages")).contains("\"content\":\"Hello\"", "\"finish_reason\":\"max_tokens\"");
    }

    @Test
    void apiErrorsAreRecordedAndRethrownUnchanged() {
        status.set(400);
        assertThatThrownBy(() -> client.messages().create(params()))
                .isInstanceOf(com.anthropic.errors.BadRequestException.class);
        SpanData s = only();
        assertThat(s.getStatus().getStatusCode()).isEqualTo(StatusCode.ERROR);
        assertThat(str(s, "error.type")).isEqualTo("com.anthropic.errors.BadRequestException");
    }

    @Test
    void otherServicesAndOptionsKeepWorking() {
        AnthropicClient withOptions = client.withOptions(b -> b.maxRetries(0));
        withOptions.messages().create(params());
        assertThat(spans()).hasSize(1);
        assertThat(TrooperLlm.wrap(client)).isSameAs(client); // idempotent
        assertThat(client.models()).isNotNull(); // delegated
        assertThat(client).isEqualTo(client);
    }
}
