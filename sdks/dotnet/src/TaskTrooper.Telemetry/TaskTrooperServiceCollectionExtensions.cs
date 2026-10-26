using OpenTelemetry;
using OpenTelemetry.Metrics;
using OpenTelemetry.Trace;
using TaskTrooper.Telemetry;

namespace Microsoft.Extensions.DependencyInjection;

public static class TaskTrooperServiceCollectionExtensions
{
    /// <summary>
    /// One-line setup: <c>builder.Services.AddTaskTrooper(o =&gt; { o.Dsn = ...; o.ServiceName = ...; })</c>.
    /// Reads <c>TROOPER_DSN</c> and the <c>TaskTrooper</c> configuration section when values are not set in code.
    /// </summary>
    public static ITaskTrooperBuilder AddTaskTrooper(this IServiceCollection services, Action<TaskTrooperOptions>? configure = null)
    {
        ArgumentNullException.ThrowIfNull(services);
        return services.AddOpenTelemetry().UseTaskTrooper(configure);
    }

    /// <summary>Adds ActivitySources (wildcards allowed) to the TaskTrooper tracer.</summary>
    public static ITaskTrooperBuilder AddSources(this ITaskTrooperBuilder builder, params string[] names)
    {
        builder.OpenTelemetry.WithTracing(t => t.AddSource(names));
        return builder;
    }

    /// <summary>Adds Meters (wildcards allowed) to the TaskTrooper meter provider.</summary>
    public static ITaskTrooperBuilder AddMeters(this ITaskTrooperBuilder builder, params string[] names)
    {
        builder.OpenTelemetry.WithMetrics(m => m.AddMeter(names));
        return builder;
    }
}
