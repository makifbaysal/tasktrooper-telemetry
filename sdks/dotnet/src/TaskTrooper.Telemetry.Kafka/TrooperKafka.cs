using System.Diagnostics;
using System.Globalization;
using System.Text;
using Confluent.Kafka;
using OpenTelemetry;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry.Kafka;

/// <summary>
/// Confluent.Kafka helpers. Consumer side creates the entry span (<c>kind=consumer</c>) with the TaskTrooper conventions
/// (<c>trooper.process</c>, <c>trooper.consumer.attempt</c>, <c>trooper.caller</c>, OTel messaging attributes) and continues
/// the producer's trace from the message headers. Producer side injects <c>traceparent</c> + <c>baggage</c> (incl. <c>tt.svc</c>).
/// </summary>
public static class TrooperKafka
{
    public const string ActivitySourceName = "TaskTrooper.Telemetry.Kafka";

    /// <summary>Optional header carrying the delivery attempt (1-based) when re-publishing to retry topics.</summary>
    public const string AttemptHeader = "tt-attempt";

    internal static readonly ActivitySource Source =
        new(ActivitySourceName, typeof(TrooperKafka).Assembly.GetName().Version?.ToString(3));

    /// <summary>
    /// Starts the consumer entry span for <paramref name="result"/>. Dispose it when processing ends.
    /// <paramref name="processName"/> defaults to <paramref name="groupId"/>; <paramref name="attempt"/> defaults to the
    /// <c>tt-attempt</c> header or 1.
    /// </summary>
    public static Activity? StartConsume<TKey, TValue>(ConsumeResult<TKey, TValue> result, string groupId, string? processName = null, int? attempt = null)
    {
        try
        {
            var headers = result.Message?.Headers;
            var ctx = headers is null ? default : TrooperPropagation.Extract(headers, GetHeader);
            var process = string.IsNullOrWhiteSpace(processName) ? groupId : processName;
            var tags = new ActivityTagsCollection
            {
                ["messaging.system"] = "kafka",
                ["messaging.operation.type"] = "process",
                ["messaging.operation.name"] = "process",
                ["messaging.destination.name"] = result.Topic,
                ["messaging.consumer.group.name"] = groupId,
                ["messaging.destination.partition.id"] = result.Partition.Value.ToString(CultureInfo.InvariantCulture),
                ["messaging.kafka.offset"] = result.Offset.Value,
                [TrooperAttributes.Process] = process,
                [TrooperAttributes.EntryRoute] = process,
                [TrooperAttributes.ConsumerAttempt] = attempt ?? AttemptFromHeaders(headers) ?? 1,
            };
            var caller = ctx.Baggage.GetBaggage(TrooperAttributes.CallerBaggageKey);
            if (!string.IsNullOrEmpty(caller)) tags[TrooperAttributes.Caller] = caller;

            return Source.StartActivity(ActivityKind.Consumer, ctx.ActivityContext, tags, name: $"process {result.Topic}");
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("kafka.consume", e);
            return null;
        }
    }

    /// <summary>
    /// Runs <paramref name="handler"/> inside the consumer entry span. Exceptions are recorded on the span and rethrown.
    /// </summary>
    public static async Task ProcessAsync<TKey, TValue>(
        this ConsumeResult<TKey, TValue> result,
        string groupId,
        Func<ConsumeResult<TKey, TValue>, Task> handler,
        string? processName = null,
        int? attempt = null)
    {
        using var activity = StartConsume(result, groupId, processName, attempt);
        try
        {
            await handler(result).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            RecordError(activity, e);
            throw;
        }
    }

    /// <summary>Flags the current consumer span as dead-lettered (<c>trooper.consumer.dlq=true</c>).</summary>
    public static void MarkDeadLettered(Activity? activity = null) => TrooperSpan.MarkDeadLettered(activity);

    /// <summary>Writes <c>traceparent</c>, <c>tracestate</c> and <c>baggage</c> (with <c>tt.svc</c>) into Kafka headers.</summary>
    public static void Inject(Headers headers, Activity? activity = null) =>
        TrooperPropagation.Inject(headers, SetHeader, activity);

    /// <summary>Produces with a producer span and propagated trace context.</summary>
    public static async Task<DeliveryResult<TKey, TValue>> ProduceTracedAsync<TKey, TValue>(
        this IProducer<TKey, TValue> producer, string topic, Message<TKey, TValue> message, CancellationToken cancellationToken = default)
    {
        Activity? activity = null;
        try
        {
            activity = Source.StartActivity(ActivityKind.Producer, default(ActivityContext), new ActivityTagsCollection
            {
                ["messaging.system"] = "kafka",
                ["messaging.operation.type"] = "send",
                ["messaging.operation.name"] = "send",
                ["messaging.destination.name"] = topic,
            }, name: $"send {topic}");
            message.Headers ??= new Headers();
            Inject(message.Headers, activity);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("kafka.produce", e);
        }

        using (activity)
        {
            try
            {
                var result = await producer.ProduceAsync(topic, message, cancellationToken).ConfigureAwait(false);
                activity?.SetTag("messaging.destination.partition.id", result.Partition.Value.ToString(CultureInfo.InvariantCulture));
                activity?.SetTag("messaging.kafka.offset", result.Offset.Value);
                return result;
            }
            catch (Exception e)
            {
                RecordError(activity, e);
                throw;
            }
        }
    }

    internal static void RecordError(Activity? activity, Exception e)
    {
        if (activity is null) return;
        try
        {
            activity.SetStatus(ActivityStatusCode.Error, e.Message);
            activity.AddException(e);
        }
        catch (Exception inner)
        {
            TrooperSelfDiagnostics.Error("kafka.error", inner);
        }
    }

    private static int? AttemptFromHeaders(Headers? headers)
    {
        if (headers is null || !headers.TryGetLastBytes(AttemptHeader, out var bytes) || bytes is null) return null;
        return int.TryParse(Encoding.UTF8.GetString(bytes), NumberStyles.Integer, CultureInfo.InvariantCulture, out var n) && n > 0 ? n : null;
    }

    private static IEnumerable<string>? GetHeader(Headers headers, string key) =>
        headers.TryGetLastBytes(key, out var bytes) && bytes is not null ? new[] { Encoding.UTF8.GetString(bytes) } : null;

    private static void SetHeader(Headers headers, string key, string value)
    {
        headers.Remove(key);
        headers.Add(key, Encoding.UTF8.GetBytes(value));
    }
}
