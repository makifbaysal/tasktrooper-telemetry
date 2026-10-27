using System.Diagnostics;
using System.Text;

namespace TaskTrooper.Telemetry;

/// <summary>
/// LLM monitoring (ADR-023, contracts §25). <c>AddTaskTrooper()</c> listens to the GenAI ActivitySources of
/// <c>Microsoft.Extensions.AI</c> (<c>chatClient.AsBuilder().UseOpenTelemetry()</c>), the OpenAI .NET SDK, Azure AI
/// Inference and Semantic Kernel by default; <see cref="Start"/> records calls made with any other client.
/// Prompts / completions are recorded only when content capture is on (<c>TROOPER_LLM_CAPTURE_CONTENT=true</c>).
/// </summary>
public static class TrooperLlm
{
    /// <summary>ActivitySource of <see cref="Start"/> (covered by the default <c>TaskTrooper.Telemetry*</c> source).</summary>
    public const string ActivitySourceName = "TaskTrooper.Telemetry.Llm";

    /// <summary>Default name of the Microsoft.Extensions.AI <c>OpenTelemetryChatClient</c> ActivitySource.</summary>
    public const string MicrosoftExtensionsAISource = "Experimental.Microsoft.Extensions.AI";

    /// <summary>AppContext switch that makes the OpenAI .NET SDK emit its (experimental) OpenTelemetry activities.</summary>
    public const string OpenAIOpenTelemetrySwitch = "OpenAI.Experimental.EnableOpenTelemetry";

    /// <summary>Env var Microsoft.Extensions.AI reads as the default of <c>EnableSensitiveData</c>.</summary>
    public const string OtelCaptureContentEnv = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT";

    /// <summary>GenAI ActivitySources listened to by default (wildcards allowed).</summary>
    public static readonly IReadOnlyList<string> GenAISources = new[]
    {
        MicrosoftExtensionsAISource,   // M.E.AI OpenTelemetryChatClient / embedding generator (default source name)
        "Microsoft.Extensions.AI*",    // future non-experimental name
        "OpenAI.*",                    // OpenAI .NET SDK: OpenAI.ChatClient, …
        "Azure.AI.Inference*",
        "Microsoft.SemanticKernel*",
    };

    internal static readonly ActivitySource Source =
        new(ActivitySourceName, typeof(TrooperLlm).Assembly.GetName().Version?.ToString());

    /// <summary>
    /// Content capture: <c>true</c> records prompts / completions (and turns on M.E.AI's sensitive data default),
    /// <c>false</c> strips them from all GenAI spans even if a library recorded them, <c>null</c> (default) leaves each
    /// library's own opt-in alone (e.g. M.E.AI <c>EnableSensitiveData</c>, default off). Falls back to
    /// <c>TROOPER_LLM_CAPTURE_CONTENT</c>; set by <c>TaskTrooperOptions.LlmCaptureContent</c>.
    /// </summary>
    public static bool? CaptureContent { get; set; }

    internal static bool? EffectiveCaptureContent
    {
        get
        {
            if (CaptureContent is { } v) return v;
            try
            {
                return bool.TryParse(Environment.GetEnvironmentVariable("TROOPER_LLM_CAPTURE_CONTENT"), out var e) ? e : null;
            }
            catch
            {
                return null;
            }
        }
    }

    /// <summary>
    /// Starts a GenAI client span <c>"{operation} {model}"</c> (<c>gen_ai.operation.name</c>, <c>gen_ai.provider.name</c>,
    /// <c>gen_ai.request.model</c>). Never throws; returns a no-op call when nobody listens.
    /// </summary>
    /// <param name="provider"><c>openai</c>, <c>anthropic</c>, <c>aws.bedrock</c>, <c>azure.ai.openai</c>, <c>gcp.gemini</c> …</param>
    /// <param name="model">Requested model.</param>
    /// <param name="operation"><c>chat</c>, <c>text_completion</c>, <c>embeddings</c>, <c>execute_tool</c>, <c>invoke_agent</c> …</param>
    public static LlmCall Start(string? provider, string? model, string operation = "chat")
    {
        try
        {
            var op = string.IsNullOrWhiteSpace(operation) ? "chat" : operation;
            var activity = Source.StartActivity(string.IsNullOrWhiteSpace(model) ? op : $"{op} {model}", ActivityKind.Client);
            if (activity is not null)
            {
                activity.SetTag(GenAI.OperationName, op);
                if (!string.IsNullOrWhiteSpace(provider)) activity.SetTag(GenAI.ProviderName, provider.ToLowerInvariant());
                if (!string.IsNullOrWhiteSpace(model)) activity.SetTag(GenAI.RequestModel, model);
            }

            return new LlmCall(activity, EffectiveCaptureContent == true);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("llm", e);
            return new LlmCall(null, false);
        }
    }

    /// <summary>GenAI JSON message array with one text message: <c>[{"role":…,"parts":[{"type":"text","content":…}]}]</c>.</summary>
    public static string Messages(string role, string text, string? finishReason = null)
    {
        var sb = new StringBuilder("[{\"role\":");
        Json(sb, role);
        sb.Append(",\"parts\":[{\"type\":\"text\",\"content\":");
        Json(sb, text);
        sb.Append("}]");
        if (finishReason is not null)
        {
            sb.Append(",\"finish_reason\":");
            Json(sb, finishReason);
        }

        return sb.Append("}]").ToString();
    }

    internal static void Json(StringBuilder sb, string? s)
    {
        sb.Append('"');
        foreach (var c in s ?? string.Empty)
        {
            switch (c)
            {
                case '"': sb.Append("\\\""); break;
                case '\\': sb.Append("\\\\"); break;
                case '\n': sb.Append("\\n"); break;
                case '\r': sb.Append("\\r"); break;
                case '\t': sb.Append("\\t"); break;
                default:
                    if (c < 0x20) sb.Append("\\u").Append(((int)c).ToString("x4"));
                    else sb.Append(c);
                    break;
            }
        }

        sb.Append('"');
    }

    /// <summary>OTel GenAI attribute names used by the SDK (contracts §25).</summary>
    public static class GenAI
    {
        public const string OperationName = "gen_ai.operation.name";
        public const string ProviderName = "gen_ai.provider.name";
        public const string System = "gen_ai.system";
        public const string RequestModel = "gen_ai.request.model";
        public const string ResponseModel = "gen_ai.response.model";
        public const string ResponseId = "gen_ai.response.id";
        public const string FinishReasons = "gen_ai.response.finish_reasons";
        public const string MaxTokens = "gen_ai.request.max_tokens";
        public const string Temperature = "gen_ai.request.temperature";
        public const string ConversationId = "gen_ai.conversation.id";
        public const string InputTokens = "gen_ai.usage.input_tokens";
        public const string OutputTokens = "gen_ai.usage.output_tokens";
        public const string CacheReadInputTokens = "gen_ai.usage.cache_read.input_tokens";
        public const string CacheCreationInputTokens = "gen_ai.usage.cache_creation.input_tokens";
        public const string InputMessages = "gen_ai.input.messages";
        public const string OutputMessages = "gen_ai.output.messages";
        public const string SystemInstructions = "gen_ai.system_instructions";
        public const string TimeToFirstChunk = "gen_ai.response.time_to_first_chunk";
        public const string TtftMs = "trooper.llm.ttft_ms";
    }
}

/// <summary>Token usage of one call; <c>null</c> = unknown.</summary>
public readonly record struct LlmUsage(long? InputTokens, long? OutputTokens, long? CacheReadInputTokens = null, long? CacheCreationInputTokens = null);

/// <summary>
/// One LLM call started by <see cref="TrooperLlm.Start"/>. Thread-safe, never throws, no-op after <see cref="End"/>.
/// <code>
/// using var call = TrooperLlm.Start("anthropic", "claude-sonnet-4-5");
/// await foreach (var chunk in stream) { call.FirstToken(); … }
/// call.End(new LlmUsage(inTokens, outTokens));
/// </code>
/// </summary>
public sealed class LlmCall : IDisposable
{
    private readonly long startTimestamp = Stopwatch.GetTimestamp();
    private int firstToken;
    private int ended;

    internal LlmCall(Activity? activity, bool captureContent)
    {
        Activity = activity;
        CaptureContent = captureContent && activity is not null;
    }

    /// <summary>The span (null when no listener samples it).</summary>
    public Activity? Activity { get; }

    /// <summary>Whether prompts / completions are recorded for this call.</summary>
    public bool CaptureContent { get; }

    public LlmCall MaxTokens(long value) => Tag(TrooperLlm.GenAI.MaxTokens, value);

    public LlmCall Temperature(double value) => Tag(TrooperLlm.GenAI.Temperature, value);

    public LlmCall ConversationId(string? id) => Tag(TrooperLlm.GenAI.ConversationId, id);

    public LlmCall ResponseModel(string? model) => Tag(TrooperLlm.GenAI.ResponseModel, string.IsNullOrWhiteSpace(model) ? null : model);

    public LlmCall ResponseId(string? id) => Tag(TrooperLlm.GenAI.ResponseId, string.IsNullOrWhiteSpace(id) ? null : id);

    public LlmCall FinishReasons(params string[] reasons) =>
        reasons is { Length: > 0 } ? Tag(TrooperLlm.GenAI.FinishReasons, reasons) : this;

    /// <summary>Marks the first streamed token / chunk: records <c>trooper.llm.ttft_ms</c> once.</summary>
    public LlmCall FirstToken()
    {
        if (Interlocked.Exchange(ref firstToken, 1) == 0)
            Tag(TrooperLlm.GenAI.TtftMs, Stopwatch.GetElapsedTime(startTimestamp).TotalMilliseconds);
        return this;
    }

    /// <summary><c>gen_ai.input.messages</c> (GenAI JSON) — only with content capture.</summary>
    public LlmCall InputMessages(string? json) => CaptureContent ? Tag(TrooperLlm.GenAI.InputMessages, json) : this;

    /// <summary><c>gen_ai.output.messages</c> (GenAI JSON) — only with content capture.</summary>
    public LlmCall OutputMessages(string? json) => CaptureContent ? Tag(TrooperLlm.GenAI.OutputMessages, json) : this;

    /// <summary><c>gen_ai.system_instructions</c> (GenAI JSON parts) — only with content capture.</summary>
    public LlmCall SystemInstructions(string? json) => CaptureContent ? Tag(TrooperLlm.GenAI.SystemInstructions, json) : this;

    /// <summary>One user text message as <c>gen_ai.input.messages</c> — only with content capture.</summary>
    public LlmCall Prompt(string? text) => CaptureContent && text is not null ? InputMessages(TrooperLlm.Messages("user", text)) : this;

    /// <summary>One assistant text message as <c>gen_ai.output.messages</c> — only with content capture.</summary>
    public LlmCall Completion(string? text) => CaptureContent && text is not null ? OutputMessages(TrooperLlm.Messages("assistant", text)) : this;

    /// <summary>Records usage and ends the span.</summary>
    public void End(LlmUsage? usage = null)
    {
        if (Interlocked.Exchange(ref ended, 1) == 1) return;
        try
        {
            if (Activity is null) return;
            if (usage is { } u)
            {
                if (u.InputTokens is { } i) Activity.SetTag(TrooperLlm.GenAI.InputTokens, i);
                if (u.OutputTokens is { } o) Activity.SetTag(TrooperLlm.GenAI.OutputTokens, o);
                if (u.CacheReadInputTokens is { } r) Activity.SetTag(TrooperLlm.GenAI.CacheReadInputTokens, r);
                if (u.CacheCreationInputTokens is { } c) Activity.SetTag(TrooperLlm.GenAI.CacheCreationInputTokens, c);
            }

            Activity.Stop();
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("llm", e);
        }
    }

    /// <summary>Records the error and ends the span.</summary>
    public void Fail(Exception? error)
    {
        if (Volatile.Read(ref ended) == 1) return;
        try
        {
            if (Activity is not null)
            {
                Activity.SetStatus(ActivityStatusCode.Error, error?.Message);
                if (error is not null)
                {
                    Activity.SetTag("error.type", error.GetType().FullName);
                    Activity.AddException(error);
                }
            }
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("llm", e);
        }

        End();
    }

    public void Dispose() => End();

    private LlmCall Tag(string key, object? value)
    {
        if (Volatile.Read(ref ended) == 1 || Activity is null || value is null) return this;
        try
        {
            Activity.SetTag(key, value);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("llm", e);
        }

        return this;
    }
}
