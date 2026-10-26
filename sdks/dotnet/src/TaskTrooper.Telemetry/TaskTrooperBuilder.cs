using Microsoft.Extensions.DependencyInjection;
using OpenTelemetry;

namespace TaskTrooper.Telemetry;

/// <summary>Returned by <c>AddTaskTrooper()</c>/<c>UseTaskTrooper()</c>; integration packages hang their extensions here.</summary>
public interface ITaskTrooperBuilder
{
    IServiceCollection Services { get; }

    IOpenTelemetryBuilder OpenTelemetry { get; }
}

internal sealed class TaskTrooperBuilder(IOpenTelemetryBuilder openTelemetry) : ITaskTrooperBuilder
{
    public IServiceCollection Services => OpenTelemetry.Services;

    public IOpenTelemetryBuilder OpenTelemetry { get; } = openTelemetry;
}
