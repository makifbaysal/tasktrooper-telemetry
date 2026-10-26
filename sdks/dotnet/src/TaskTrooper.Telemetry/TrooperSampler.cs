using System.Diagnostics;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry;

/// <summary>
/// Parent-based trace-id-ratio sampler. Entry spans (server/consumer kind whose parent is remote or absent)
/// get <c>trooper.sample_rate</c> so the writer can scale RED counts by 1/rate.
/// </summary>
public sealed class TrooperSampler : Sampler
{
    private readonly Sampler inner;
    private readonly KeyValuePair<string, object>[] rateAttribute;

    public TrooperSampler(double sampleRate)
    {
        SampleRate = double.IsNaN(sampleRate) ? 1.0 : Math.Clamp(sampleRate, 0.0, 1.0);
        Sampler root = SampleRate >= 1.0 ? new AlwaysOnSampler() : SampleRate <= 0.0 ? new AlwaysOffSampler() : new TraceIdRatioBasedSampler(SampleRate);
        inner = new ParentBasedSampler(root);
        rateAttribute = new[] { new KeyValuePair<string, object>(TrooperAttributes.SampleRate, SampleRate) };
        Description = $"TrooperSampler{{{SampleRate}}}";
    }

    public double SampleRate { get; }

    public override SamplingResult ShouldSample(in SamplingParameters p)
    {
        var result = inner.ShouldSample(p);
        if (result.Decision != SamplingDecision.RecordAndSample || !IsEntry(p)) return result;
        var attrs = result.Attributes is null ? rateAttribute : result.Attributes.Concat(rateAttribute);
        return new SamplingResult(result.Decision, attrs, result.TraceStateString);
    }

    private static bool IsEntry(in SamplingParameters p) =>
        (p.Kind == ActivityKind.Server || p.Kind == ActivityKind.Consumer) &&
        (p.ParentContext.TraceId == default || p.ParentContext.IsRemote);
}
