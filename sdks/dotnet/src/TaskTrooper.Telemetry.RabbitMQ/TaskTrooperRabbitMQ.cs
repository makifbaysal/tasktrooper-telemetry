using System.Diagnostics;
using System.Globalization;
using System.Text;
using OpenTelemetry;
using OpenTelemetry.Trace;
using RabbitMQ.Client;
using RabbitMQ.Client.Events;

namespace TaskTrooper.Telemetry.RabbitMQ;

public sealed class TrooperRabbitMQOptions
{
    /// <summary>Consumer process name for <c>trooper.process</c>. Default: routing key, else exchange.</summary>
    public string? ProcessName { get; set; }
}

/// <summary>
/// RabbitMQ.Client 7 has built-in ActivitySources (<c>RabbitMQ.Client.Publisher</c>/<c>.Subscriber</c>); this processor adds
/// the TaskTrooper consumer conventions to the subscriber spans.
/// </summary>
internal sealed class TrooperRabbitMQProcessor(TrooperRabbitMQOptions options) : BaseProcessor<Activity>
{
    public override void OnStart(Activity activity)
    {
        try
        {
            if (activity.Kind != ActivityKind.Consumer || activity.Source.Name != TrooperRabbitMQ.SubscriberSource) return;
            if (activity.GetTagItem(TrooperAttributes.Process) is not null) return;
            var process = options.ProcessName
                          ?? NonEmpty(activity.GetTagItem("messaging.rabbitmq.destination.routing_key") as string)
                          ?? NonEmpty(activity.GetTagItem("messaging.destination.name") as string)
                          ?? "rabbitmq";
            TrooperSpan.SetProcess(activity, process);
            if (activity.GetTagItem(TrooperAttributes.ConsumerAttempt) is null) activity.SetTag(TrooperAttributes.ConsumerAttempt, 1);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("rabbitmq.processor", e);
        }
    }

    private static string? NonEmpty(string? s) => string.IsNullOrEmpty(s) ? null : s;
}

public static class TrooperRabbitMQ
{
    public const string PublisherSource = "RabbitMQ.Client.Publisher";
    public const string SubscriberSource = "RabbitMQ.Client.Subscriber";

    /// <summary>
    /// Call at the start of a delivery handler: sets <c>trooper.process</c> (queue), <c>trooper.consumer.attempt</c>
    /// (from <c>x-delivery-count</c>/<c>x-death</c>/redelivered flag) and <c>trooper.caller</c> (baggage <c>tt.svc</c>) on the
    /// current consumer span.
    /// </summary>
    public static void Enrich(BasicDeliverEventArgs delivery, string? queue = null, Activity? activity = null)
    {
        try
        {
            activity ??= Activity.Current;
            if (activity is null) return;
            if (!string.IsNullOrEmpty(queue)) TrooperSpan.SetProcess(activity, queue);
            activity.SetTag(TrooperAttributes.ConsumerAttempt, Attempt(delivery));
            var headers = delivery.BasicProperties?.Headers;
            if (headers is not null && headers.TryGetValue("baggage", out var raw))
            {
                var caller = TrooperPropagation.CallerFromBaggageHeader(AsString(raw));
                if (caller is not null) activity.SetTag(TrooperAttributes.Caller, caller);
            }
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("rabbitmq.enrich", e);
        }
    }

    /// <summary>1-based delivery attempt.</summary>
    public static int Attempt(BasicDeliverEventArgs delivery)
    {
        var headers = delivery.BasicProperties?.Headers;
        if (headers is not null)
        {
            // quorum queues: number of previous delivery attempts
            if (headers.TryGetValue("x-delivery-count", out var dc) && ToLong(dc) is { } count) return (int)count + 1;
            // dead-letter retry loops: sum of x-death counts
            if (headers.TryGetValue("x-death", out var death) && death is IEnumerable<object?> entries)
            {
                long total = 0;
                foreach (var entry in entries)
                    if (entry is IDictionary<string, object?> d && d.TryGetValue("count", out var c) && ToLong(c) is { } n) total += n;
                if (total > 0) return (int)total + 1;
            }
        }

        return delivery.Redelivered ? 2 : 1;
    }

    /// <summary>Flags the current consumer span as dead-lettered.</summary>
    public static void MarkDeadLettered(Activity? activity = null) => TrooperSpan.MarkDeadLettered(activity);

    internal static void InstallBaggageInjector()
    {
        // Default injector writes traceparent via DistributedContextPropagator; ours adds W3C baggage with tt.svc.
        RabbitMQActivitySource.ContextInjector = (activity, headers) =>
            TrooperPropagation.Inject(headers, static (h, k, v) => h[k] = v, activity);
    }

    private static long? ToLong(object? v) => v switch
    {
        long l => l,
        int i => i,
        short s => s,
        byte b => b,
        byte[] bytes when long.TryParse(Encoding.UTF8.GetString(bytes), NumberStyles.Integer, CultureInfo.InvariantCulture, out var p) => p,
        string str when long.TryParse(str, NumberStyles.Integer, CultureInfo.InvariantCulture, out var p) => p,
        _ => null,
    };

    private static string? AsString(object? v) => v switch
    {
        byte[] bytes => Encoding.UTF8.GetString(bytes),
        string s => s,
        _ => v?.ToString(),
    };
}

public static class TaskTrooperRabbitMQExtensions
{
    /// <summary>
    /// RabbitMQ.Client 7 tracing (publisher + subscriber spans) with <c>trooper.process</c>/<c>trooper.consumer.attempt</c>
    /// on consumer spans and <c>tt.svc</c> baggage on published messages.
    /// </summary>
    public static ITaskTrooperBuilder AddRabbitMQ(this ITaskTrooperBuilder builder, Action<TrooperRabbitMQOptions>? configure = null)
    {
        var options = new TrooperRabbitMQOptions();
        configure?.Invoke(options);
        TrooperRabbitMQ.InstallBaggageInjector();
        builder.OpenTelemetry.WithTracing(t => t
            .AddSource(TrooperRabbitMQ.PublisherSource, TrooperRabbitMQ.SubscriberSource)
            .AddProcessor(new TrooperRabbitMQProcessor(options)));
        return builder;
    }
}
