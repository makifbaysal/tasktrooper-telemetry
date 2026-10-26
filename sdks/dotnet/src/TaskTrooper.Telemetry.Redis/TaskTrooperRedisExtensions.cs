using OpenTelemetry;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry;

public static class TaskTrooperRedisExtensions
{
    /// <summary>
    /// StackExchange.Redis command spans. Uses the <c>IConnectionMultiplexer</c> registered in DI
    /// (or call <c>AddRedisInstrumentation(connection)</c> on the tracer for manually created connections).
    /// </summary>
    public static ITaskTrooperBuilder AddRedis(this ITaskTrooperBuilder builder)
    {
        builder.OpenTelemetry.WithTracing(t => t.AddRedisInstrumentation());
        return builder;
    }
}
