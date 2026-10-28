using System.ComponentModel;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>Called by the core package (by name) when this package is referenced; not for application code.</summary>
[EditorBrowsable(EditorBrowsableState.Never)]
public static class TaskTrooperProfilingAutoRegistration
{
    public static void Register(IServiceCollection services) => TaskTrooperProfilingExtensions.Register(services, explicitCall: false);
}

internal sealed class ProfilingMarker
{
    public bool Explicit { get; set; }
}

/// <summary>Starts / stops the <see cref="TrooperProfiler"/> with the host.</summary>
internal sealed class TrooperProfilerHostedService(
    IOptions<TrooperProfilingOptions> profiling,
    IOptions<TaskTrooperOptions> trooper,
    ProfilingMarker marker,
    ILoggerFactory? loggerFactory = null) : IHostedService, IDisposable
{
    internal TrooperProfiler? Profiler { get; private set; }

    internal bool IsEnabled(TrooperProfilingOptions o) => o.Enabled ?? o.EnvEnabled ?? marker.Explicit;

    public Task StartAsync(CancellationToken cancellationToken)
    {
        try
        {
            var o = profiling.Value;
            if (!IsEnabled(o)) return Task.CompletedTask;
            var profiler = new TrooperProfiler(o, trooper.Value, loggerFactory?.CreateLogger("TaskTrooper.Telemetry.Profiling"));
            if (profiler.Start()) Profiler = profiler;
            else profiler.Dispose();
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }

        return Task.CompletedTask;
    }

    public Task StopAsync(CancellationToken cancellationToken)
    {
        try
        {
            Profiler?.Stop(TimeSpan.FromSeconds(5));
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }

        return Task.CompletedTask;
    }

    public void Dispose() => Profiler?.Dispose();
}
