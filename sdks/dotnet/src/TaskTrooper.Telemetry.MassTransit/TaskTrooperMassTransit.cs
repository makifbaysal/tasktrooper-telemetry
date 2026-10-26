using System.Diagnostics;
using OpenTelemetry;
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry.MassTransit;

public sealed class TrooperMassTransitOptions
{
    /// <summary>Consumer process name for <c>trooper.process</c>. Default: receive endpoint (queue) name.</summary>
    public string? ProcessName { get; set; }
}

/// <summary>
/// Maps MassTransit's built-in diagnostics (ActivitySource <c>MassTransit</c>) to TaskTrooper consumer conventions.
/// No MassTransit package dependency: works by attribute names, so it supports MassTransit 8 and 9.
/// </summary>
internal sealed class TrooperMassTransitProcessor(TrooperMassTransitOptions options) : BaseProcessor<Activity>
{
    internal const string SourceName = "MassTransit";

    public override void OnStart(Activity activity)
    {
        try
        {
            if (activity.Source.Name != SourceName || activity.Kind != ActivityKind.Consumer) return;
            if (activity.GetTagItem(TrooperAttributes.Process) is not null) return;
            // only the receive span (local root) is the entry span; consume/handler spans inherit entry route
            if (activity.Parent is not null) return;
            var process = options.ProcessName
                          ?? QueueName(activity.GetTagItem("messaging.destination.name") as string)
                          ?? QueueName(activity.GetTagItem("messaging.masstransit.input_address") as string)
                          ?? "masstransit";
            TrooperSpan.SetProcess(activity, process);
            activity.SetTag(TrooperAttributes.ConsumerAttempt, 1);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("masstransit.processor", e);
        }
    }

    internal static string? QueueName(string? value)
    {
        if (string.IsNullOrWhiteSpace(value)) return null;
        if (!Uri.TryCreate(value, UriKind.Absolute, out var uri)) return value;
        var last = uri.AbsolutePath.TrimEnd('/').Split('/').LastOrDefault();
        return string.IsNullOrEmpty(last) ? value : last;
    }
}

public static class TaskTrooperMassTransitExtensions
{
    /// <summary>MassTransit traces + metrics with <c>trooper.process</c> on receive (entry) spans.</summary>
    public static ITaskTrooperBuilder AddMassTransit(this ITaskTrooperBuilder builder, Action<TrooperMassTransitOptions>? configure = null)
    {
        var options = new TrooperMassTransitOptions();
        configure?.Invoke(options);
        builder.OpenTelemetry
            .WithTracing(t => t.AddSource(TrooperMassTransitProcessor.SourceName).AddProcessor(new TrooperMassTransitProcessor(options)))
            .WithMetrics(m => m.AddMeter(TrooperMassTransitProcessor.SourceName));
        return builder;
    }
}
