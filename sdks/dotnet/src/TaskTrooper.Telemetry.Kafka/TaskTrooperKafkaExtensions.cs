using OpenTelemetry;
using OpenTelemetry.Trace;
using TaskTrooper.Telemetry.Kafka;

namespace TaskTrooper.Telemetry;

public static class TaskTrooperKafkaExtensions
{
    /// <summary>
    /// Enables the Kafka helper spans. (The core already listens to <c>TaskTrooper.Telemetry*</c>, so this is only needed
    /// when wiring OpenTelemetry without <c>UseTaskTrooper</c>.)
    /// </summary>
    public static ITaskTrooperBuilder AddKafka(this ITaskTrooperBuilder builder)
    {
        builder.OpenTelemetry.WithTracing(t => t.AddSource(TrooperKafka.ActivitySourceName));
        return builder;
    }

    /// <summary>Listens to the TaskTrooper Kafka helper spans on a plain OpenTelemetry tracer.</summary>
    public static TracerProviderBuilder AddTaskTrooperKafka(this TracerProviderBuilder builder) =>
        builder.AddSource(TrooperKafka.ActivitySourceName);
}
