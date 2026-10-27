using System.Diagnostics;
using System.Globalization;
using OpenTelemetry;

namespace TaskTrooper.Telemetry;

/// <summary>
/// Normalizes GenAI activities to the attribute names the writer reads (contracts §25) when they end:
/// <list type="bullet">
/// <item><c>gen_ai.system</c> (older M.E.AI / OpenAI SDK) → <c>gen_ai.provider.name</c> when missing, vendor values mapped
/// (<c>az.ai.openai</c> → <c>azure.ai.openai</c>, <c>az.ai.inference</c> → <c>azure.ai.inference</c> …).</item>
/// <item><c>gen_ai.response.time_to_first_chunk</c> (seconds, M.E.AI streaming) → <c>trooper.llm.ttft_ms</c>.</item>
/// <item>Content capture explicitly off (<see cref="TrooperLlm.CaptureContent"/> = false /
/// <c>TROOPER_LLM_CAPTURE_CONTENT=false</c>): prompt / completion tags are removed even if a library recorded them.</item>
/// </list>
/// Activities without <c>gen_ai.operation.name</c> / <c>gen_ai.system</c> are untouched. Never throws.
/// </summary>
public sealed class TrooperGenAIProcessor : BaseProcessor<Activity>
{
    private static readonly string[] ContentTags =
    {
        TrooperLlm.GenAI.InputMessages, TrooperLlm.GenAI.OutputMessages, TrooperLlm.GenAI.SystemInstructions,
        "gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.prompt", "gen_ai.completion",
    };

    private static readonly Dictionary<string, string> Providers = new(StringComparer.OrdinalIgnoreCase)
    {
        ["az.ai.openai"] = "azure.ai.openai",
        ["azure_openai"] = "azure.ai.openai",
        ["az.ai.inference"] = "azure.ai.inference",
        ["bedrock"] = "aws.bedrock",
        ["vertex_ai"] = "gcp.vertex_ai",
        ["gemini"] = "gcp.gemini",
        ["mistral"] = "mistral_ai",
    };

    public override void OnEnd(Activity activity)
    {
        try
        {
            var operation = activity.GetTagItem(TrooperLlm.GenAI.OperationName);
            var system = activity.GetTagItem(TrooperLlm.GenAI.System) as string;
            if (operation is null && system is null) return;

            if (activity.GetTagItem(TrooperLlm.GenAI.ProviderName) is null && !string.IsNullOrWhiteSpace(system))
                activity.SetTag(TrooperLlm.GenAI.ProviderName, Provider(system));

            if (activity.GetTagItem(TrooperLlm.GenAI.TtftMs) is null &&
                Seconds(activity.GetTagItem(TrooperLlm.GenAI.TimeToFirstChunk)) is { } ttft)
            {
                activity.SetTag(TrooperLlm.GenAI.TtftMs, ttft * 1000.0);
            }

            if (TrooperLlm.EffectiveCaptureContent == false) StripContent(activity);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("processor.genai", e);
        }
    }

    internal static string Provider(string system)
    {
        var s = system.Trim().ToLowerInvariant();
        return Providers.TryGetValue(s, out var mapped) ? mapped : s;
    }

    private static void StripContent(Activity activity)
    {
        foreach (var key in ContentTags)
        {
            if (activity.GetTagItem(key) is not null) activity.SetTag(key, null);
        }

        List<string>? indexed = null;
        foreach (var tag in activity.TagObjects)
        {
            if (tag.Key.StartsWith("gen_ai.prompt.", StringComparison.Ordinal) ||
                tag.Key.StartsWith("gen_ai.completion.", StringComparison.Ordinal))
            {
                (indexed ??= new List<string>()).Add(tag.Key);
            }
        }

        if (indexed is null) return;
        foreach (var key in indexed) activity.SetTag(key, null);
    }

    private static double? Seconds(object? value) => value switch
    {
        double d => d,
        float f => f,
        TimeSpan t => t.TotalSeconds,
        long l => l,
        int i => i,
        string s when double.TryParse(s, NumberStyles.Float, CultureInfo.InvariantCulture, out var d) => d,
        _ => null,
    };
}
