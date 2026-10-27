using System.Diagnostics;
using System.Runtime.CompilerServices;
using Microsoft.Extensions.AI;
using Microsoft.Extensions.DependencyInjection;
using OpenTelemetry;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry.Tests;

public sealed class LlmTests : IDisposable
{
    private readonly List<Activity> exported = new();
    private ServiceProvider? sp;

    private TracerProvider Provider(Action<TaskTrooperOptions>? configure = null)
    {
        var services = new ServiceCollection();
        services.AddTaskTrooper(o =>
            {
                o.ServiceName = "ai-app";
                configure?.Invoke(o);
            })
            .OpenTelemetry.WithTracing(t => t.AddInMemoryExporter(exported));
        sp = services.BuildServiceProvider();
        return sp.GetRequiredService<TracerProvider>();
    }

    public void Dispose()
    {
        sp?.Dispose();
        TrooperLlm.CaptureContent = null;
        Environment.SetEnvironmentVariable(TrooperLlm.OtelCaptureContentEnv, null);
    }

    [Theory]
    [InlineData("Experimental.Microsoft.Extensions.AI")]
    [InlineData("OpenAI.ChatClient")]
    [InlineData("Azure.AI.Inference.ChatCompletionsClient")]
    [InlineData("Microsoft.SemanticKernel.Diagnostics")]
    public void GenAI_sources_are_listened_to_by_default(string sourceName)
    {
        _ = Provider();
        using var source = new ActivitySource(sourceName);
        using (var activity = source.StartActivity("chat gpt-4o", ActivityKind.Client))
        {
            Assert.NotNull(activity);
            activity!.SetTag("gen_ai.operation.name", "chat");
        }

        Assert.Single(exported, a => a.Source.Name == sourceName);
        Assert.True(AppContext.TryGetSwitch(TrooperLlm.OpenAIOpenTelemetrySwitch, out var on) && on);
        Assert.Contains(TrooperLlm.MicrosoftExtensionsAISource, TrooperLlm.GenAISources);
    }

    [Fact]
    public void GenAI_sources_can_be_turned_off()
    {
        _ = Provider(o => o.EnableLlm = false);
        using var source = new ActivitySource("OpenAI.EmbeddingClient");
        using (source.StartActivity("embeddings", ActivityKind.Client)) { }
        Assert.Empty(exported);
    }

    [Fact]
    public async Task Microsoft_Extensions_AI_chat_client_spans_are_exported_without_content_by_default()
    {
        _ = Provider();
        using var client = new ChatClientBuilder(new FakeChatClient())
            .UseOpenTelemetry(configure: c => c.EnableSensitiveData = false)
            .Build();

        var response = await client.GetResponseAsync("What is the capital of France?");
        Assert.Equal("Paris.", response.Text);

        var span = Assert.Single(exported, a => a.Source.Name == TrooperLlm.MicrosoftExtensionsAISource);
        Assert.Equal("chat", span.Tag("gen_ai.operation.name"));
        Assert.Equal("openai", span.Tag("gen_ai.provider.name"));
        Assert.Equal("gpt-4o-mini", span.Tag("gen_ai.request.model"));
        Assert.Equal("gpt-4o-mini-2024-07-18", span.Tag("gen_ai.response.model"));
        Assert.Equal(12L, Convert.ToInt64(span.Tag("gen_ai.usage.input_tokens")));
        Assert.Equal(3L, Convert.ToInt64(span.Tag("gen_ai.usage.output_tokens")));
        Assert.Null(span.Tag("gen_ai.input.messages"));
        Assert.Null(span.Tag("gen_ai.output.messages"));
    }

    [Fact]
    public async Task Capture_content_true_turns_on_message_recording()
    {
        _ = Provider(o => o.LlmCaptureContent = true);
        Assert.Equal("true", Environment.GetEnvironmentVariable(TrooperLlm.OtelCaptureContentEnv));
        // M.E.AI caches the env default per process (first use may have been another test), so opt in explicitly here
        using var client = new ChatClientBuilder(new FakeChatClient())
            .UseOpenTelemetry(configure: c => c.EnableSensitiveData = true)
            .Build();

        await client.GetResponseAsync("What is the capital of France?");

        var span = Assert.Single(exported, a => a.Source.Name == TrooperLlm.MicrosoftExtensionsAISource);
        Assert.Contains("capital of France", span.Tag("gen_ai.input.messages") as string);
        Assert.Contains("Paris.", span.Tag("gen_ai.output.messages") as string);
    }

    [Fact]
    public async Task Capture_content_false_strips_content_even_when_the_library_recorded_it()
    {
        _ = Provider(o => o.LlmCaptureContent = false);
        using var client = new ChatClientBuilder(new FakeChatClient())
            .UseOpenTelemetry(configure: c => c.EnableSensitiveData = true)
            .Build();

        await client.GetResponseAsync("secret prompt");

        var span = Assert.Single(exported, a => a.Source.Name == TrooperLlm.MicrosoftExtensionsAISource);
        Assert.Null(span.Tag("gen_ai.input.messages"));
        Assert.Null(span.Tag("gen_ai.output.messages"));
        Assert.Equal(12L, Convert.ToInt64(span.Tag("gen_ai.usage.input_tokens")));
    }

    [Fact]
    public async Task Manual_helper_records_tokens_ttft_and_errors()
    {
        _ = Provider();
        using (var call = TrooperLlm.Start("Anthropic", "claude-sonnet-4-5"))
        {
            await Task.Delay(5);
            call.FirstToken().FirstToken()
                .MaxTokens(1024)
                .Prompt("never recorded without capture")
                .ResponseModel("claude-sonnet-4-5-20250929")
                .FinishReasons("end_turn");
            call.End(new LlmUsage(100, 20, CacheReadInputTokens: 80));
            call.End(new LlmUsage(1, 1)); // ignored
        }

        var failed = TrooperLlm.Start("openai", "gpt-4o", "embeddings");
        failed.Fail(new InvalidOperationException("429"));
        failed.FirstToken();

        var ok = Assert.Single(exported, a => a.DisplayName == "chat claude-sonnet-4-5");
        Assert.Equal(ActivityKind.Client, ok.Kind);
        Assert.Equal(TrooperLlm.ActivitySourceName, ok.Source.Name);
        Assert.Equal("anthropic", ok.Tag("gen_ai.provider.name"));
        Assert.Equal("chat", ok.Tag("gen_ai.operation.name"));
        Assert.Equal(100L, ok.Tag("gen_ai.usage.input_tokens"));
        Assert.Equal(20L, ok.Tag("gen_ai.usage.output_tokens"));
        Assert.Equal(80L, ok.Tag("gen_ai.usage.cache_read.input_tokens"));
        Assert.Equal(1024L, ok.Tag("gen_ai.request.max_tokens"));
        Assert.Equal(new[] { "end_turn" }, ok.Tag("gen_ai.response.finish_reasons") as string[]);
        Assert.True((double)ok.Tag("trooper.llm.ttft_ms")! >= 4.0);
        Assert.Null(ok.Tag("gen_ai.input.messages"));

        var err = Assert.Single(exported, a => a.DisplayName == "embeddings gpt-4o");
        Assert.Equal(ActivityStatusCode.Error, err.Status);
        Assert.Equal("System.InvalidOperationException", err.Tag("error.type"));
        Assert.Null(err.Tag("trooper.llm.ttft_ms"));
    }

    [Fact]
    public void Manual_helper_records_content_only_when_enabled_and_never_throws_without_listener()
    {
        // no provider: nobody listens → no-op call
        var noop = TrooperLlm.Start("openai", "gpt-4o");
        noop.FirstToken().Prompt("x").End();
        noop.Fail(new Exception("after end"));

        _ = Provider(o => o.LlmCaptureContent = true);
        using (var call = TrooperLlm.Start("openai", "gpt-4o"))
        {
            call.Prompt("say \"hi\"").Completion("hi");
        }

        var span = Assert.Single(exported);
        Assert.Equal("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"say \\\"hi\\\"\"}]}]", span.Tag("gen_ai.input.messages"));
        Assert.Equal("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"hi\"}]}]", span.Tag("gen_ai.output.messages"));
    }

    [Fact]
    public void Processor_maps_legacy_system_and_time_to_first_chunk()
    {
        _ = Provider();
        using var source = new ActivitySource("OpenAI.ChatClient");
        using (var activity = source.StartActivity("chat gpt-4o", ActivityKind.Client)!)
        {
            activity.SetTag("gen_ai.operation.name", "chat");
            activity.SetTag("gen_ai.system", "az.ai.openai");
            activity.SetTag("gen_ai.response.time_to_first_chunk", 0.25);
        }

        var span = Assert.Single(exported);
        Assert.Equal("azure.ai.openai", span.Tag("gen_ai.provider.name"));
        Assert.Equal(250.0, (double)span.Tag("trooper.llm.ttft_ms")!, 3);
        Assert.Equal("openai", TrooperGenAIProcessor.Provider(" OpenAI "));
    }

    [Fact]
    public void Options_read_capture_content_from_environment()
    {
        Environment.SetEnvironmentVariable("TROOPER_LLM_CAPTURE_CONTENT", "false");
        try
        {
            var services = new ServiceCollection();
            services.AddTaskTrooper();
            using var provider = services.BuildServiceProvider();
            var o = provider.GetRequiredService<Microsoft.Extensions.Options.IOptions<TaskTrooperOptions>>().Value;
            Assert.False(o.LlmCaptureContent);
            Assert.True(o.EnableLlm);
        }
        finally
        {
            Environment.SetEnvironmentVariable("TROOPER_LLM_CAPTURE_CONTENT", null);
        }
    }

    private sealed class FakeChatClient : IChatClient
    {
        private readonly ChatClientMetadata metadata = new("openai", new Uri("https://api.openai.com/v1"), "gpt-4o-mini");

        public Task<ChatResponse> GetResponseAsync(IEnumerable<ChatMessage> messages, ChatOptions? options = null, CancellationToken cancellationToken = default) =>
            Task.FromResult(new ChatResponse(new ChatMessage(ChatRole.Assistant, "Paris."))
            {
                ModelId = "gpt-4o-mini-2024-07-18",
                ResponseId = "chatcmpl-1",
                FinishReason = ChatFinishReason.Stop,
                Usage = new UsageDetails { InputTokenCount = 12, OutputTokenCount = 3, TotalTokenCount = 15 },
            });

        public async IAsyncEnumerable<ChatResponseUpdate> GetStreamingResponseAsync(IEnumerable<ChatMessage> messages, ChatOptions? options = null,
            [EnumeratorCancellation] CancellationToken cancellationToken = default)
        {
            await Task.Yield();
            yield return new ChatResponseUpdate(ChatRole.Assistant, "Paris.");
        }

        public object? GetService(Type serviceType, object? serviceKey = null) =>
            serviceType == typeof(ChatClientMetadata) ? metadata : null;

        public void Dispose()
        {
        }
    }
}
