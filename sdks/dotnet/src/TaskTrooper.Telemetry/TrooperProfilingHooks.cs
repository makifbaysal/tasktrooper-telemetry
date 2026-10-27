using System.Diagnostics;
using Microsoft.Extensions.DependencyInjection;

namespace TaskTrooper.Telemetry;

/// <summary>
/// Seam between the core distribution and the optional <c>TaskTrooper.Telemetry.Profiling</c> package (contracts §24).
/// Core has no compile-time dependency on the profiler: <see cref="TrooperSpanProcessor"/> reports entry spans through
/// these delegates (null unless a profiler runs) and <see cref="TryAutoRegister"/> probes for the package by name.
/// </summary>
internal static class TrooperProfilingHooks
{
    internal const string AutoRegistrationType =
        "TaskTrooper.Telemetry.Profiling.TaskTrooperProfilingAutoRegistration, TaskTrooper.Telemetry.Profiling";

    /// <summary>An entry span (local root SERVER / CONSUMER) started; runs synchronously inside <c>Activity.Start</c>.</summary>
    internal static volatile Action<Activity>? EntryStarted;

    /// <summary>An entry span ended; its route is final.</summary>
    internal static volatile Action<Activity>? EntryStopped;

    /// <summary>
    /// When the profiling package is referenced, lets it register its hosted service (enabled only by
    /// <c>TROOPER_PROFILING=true</c> or an explicit <c>AddTaskTrooperProfiling()</c>). Never throws.
    /// </summary>
    internal static void TryAutoRegister(IServiceCollection services)
    {
        try
        {
            var type = Type.GetType(AutoRegistrationType, throwOnError: false);
            type?.GetMethod("Register", new[] { typeof(IServiceCollection) })?.Invoke(null, new object[] { services });
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }
    }
}
