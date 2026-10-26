using System.Diagnostics;
using OpenTelemetry;
using OpenTelemetry.Context.Propagation;

namespace TaskTrooper.Telemetry;

/// <summary>
/// W3C trace-context + baggage propagation that always stamps <c>tt.svc=&lt;this service&gt;</c> on outgoing baggage,
/// so the receiving service can fill <c>trooper.caller</c>. Used by HttpClient (via the global OTel propagator)
/// and by the messaging helpers.
/// </summary>
public static class TrooperPropagation
{
    private static readonly TextMapPropagator W3C =
        new CompositeTextMapPropagator(new TextMapPropagator[] { new TraceContextPropagator(), new BaggagePropagator() });

    /// <summary>Service name stamped into outgoing baggage. Set when the TaskTrooper tracer provider is built.</summary>
    public static string? ServiceName { get; internal set; }

    /// <summary>Injects trace context of <paramref name="activity"/> (default: current) and baggage incl. <c>tt.svc</c>.</summary>
    public static void Inject<T>(T carrier, Action<T, string, string> setter, Activity? activity = null)
    {
        try
        {
            activity ??= Activity.Current;
            var baggage = Baggage.Current;
            if (!string.IsNullOrEmpty(ServiceName)) baggage = baggage.SetBaggage(TrooperAttributes.CallerBaggageKey, ServiceName);
            W3C.Inject(new PropagationContext(activity?.Context ?? default, baggage), carrier, setter);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("propagation.inject", e);
        }
    }

    /// <summary>Extracts trace context and baggage written by <see cref="Inject{T}"/> (or any W3C producer).</summary>
    public static PropagationContext Extract<T>(T carrier, Func<T, string, IEnumerable<string>?> getter)
    {
        try
        {
            return W3C.Extract(default, carrier, getter);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("propagation.extract", e);
            return default;
        }
    }

    /// <summary>Reads <c>tt.svc</c> from a raw W3C <c>baggage</c> header value.</summary>
    public static string? CallerFromBaggageHeader(string? header)
    {
        if (string.IsNullOrEmpty(header)) return null;
        foreach (var member in header.Split(','))
        {
            var kv = member.Split(';', 2)[0];
            var eq = kv.IndexOf('=');
            if (eq <= 0) continue;
            if (kv[..eq].Trim() != TrooperAttributes.CallerBaggageKey) continue;
            var value = Uri.UnescapeDataString(kv[(eq + 1)..].Trim());
            return value.Length == 0 ? null : value;
        }
        return null;
    }
}

/// <summary>Global OTel propagator wrapper: delegates to W3C trace-context + baggage and adds <c>tt.svc</c> on inject.</summary>
internal sealed class TrooperTextMapPropagator : TextMapPropagator
{
    private readonly TextMapPropagator inner =
        new CompositeTextMapPropagator(new TextMapPropagator[] { new TraceContextPropagator(), new BaggagePropagator() });

    public override ISet<string>? Fields => inner.Fields;

    public override PropagationContext Extract<T>(PropagationContext context, T carrier, Func<T, string, IEnumerable<string>?> getter)
        => inner.Extract(context, carrier, getter);

    public override void Inject<T>(PropagationContext context, T carrier, Action<T, string, string> setter)
    {
        var baggage = context.Baggage;
        var svc = TrooperPropagation.ServiceName;
        if (!string.IsNullOrEmpty(svc)) baggage = baggage.SetBaggage(TrooperAttributes.CallerBaggageKey, svc);
        inner.Inject(new PropagationContext(context.ActivityContext, baggage), carrier, setter);
    }
}
