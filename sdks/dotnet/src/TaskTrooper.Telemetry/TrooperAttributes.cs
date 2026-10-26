namespace TaskTrooper.Telemetry;

/// <summary>Attribute names of the TaskTrooper semantic conventions (see .ai/contracts.md §4).</summary>
public static class TrooperAttributes
{
    /// <summary>Normalized route template of a server span, e.g. <c>/v1/orders/:id</c>.</summary>
    public const string Route = "trooper.route";

    /// <summary>Route (or consumer process name) of the entry span; set on every span of the request.</summary>
    public const string EntryRoute = "trooper.entry.route";

    /// <summary>Consumer process name (defaults to the consumer group).</summary>
    public const string Process = "trooper.process";

    /// <summary>Calling service, read from W3C baggage <c>tt.svc</c>.</summary>
    public const string Caller = "trooper.caller";

    /// <summary>Delivery attempt of a consumed message (1 = first delivery).</summary>
    public const string ConsumerAttempt = "trooper.consumer.attempt";

    /// <summary>True when the message was sent to a dead-letter queue.</summary>
    public const string ConsumerDlq = "trooper.consumer.dlq";

    /// <summary>Sampling probability applied to an entry span (writer scales counts by 1/rate).</summary>
    public const string SampleRate = "trooper.sample_rate";

    /// <summary>W3C baggage key carrying the calling service name.</summary>
    public const string CallerBaggageKey = "tt.svc";

    /// <summary>Value of the <c>telemetry.distro.name</c> resource attribute.</summary>
    public const string DistroName = "trooper-dotnet";
}
