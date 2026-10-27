using Microsoft.Extensions.DependencyInjection.Extensions;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Options;
using OpenTelemetry;
using TaskTrooper.Telemetry;
using TaskTrooper.Telemetry.Profiling;

namespace Microsoft.Extensions.DependencyInjection;

public static class TaskTrooperProfilingExtensions
{
    /// <summary>
    /// Turns on continuous profiling (contracts §24): <c>builder.Services.AddTaskTrooperProfiling(o =&gt; o.Types.Add(ProfileType.Wall))</c>.
    /// Uses the DSN / service / environment / version of <c>AddTaskTrooper()</c> (added here when missing).
    /// <c>TROOPER_PROFILING=false</c> or <c>o.Enabled = false</c> turns it off.
    /// </summary>
    public static IServiceCollection AddTaskTrooperProfiling(this IServiceCollection services, Action<TrooperProfilingOptions>? configure = null)
    {
        ArgumentNullException.ThrowIfNull(services);
        if (!services.Any(d => d.ServiceType == typeof(TaskTrooperMarker))) services.AddTaskTrooper();
        Register(services, explicitCall: true);
        if (configure is not null) services.Configure(configure);
        return services;
    }

    /// <summary><c>services.AddTaskTrooper().AddProfiling()</c>.</summary>
    public static ITaskTrooperBuilder AddProfiling(this ITaskTrooperBuilder builder, Action<TrooperProfilingOptions>? configure = null)
    {
        ArgumentNullException.ThrowIfNull(builder);
        builder.Services.AddTaskTrooperProfiling(configure);
        return builder;
    }

    internal static void Register(IServiceCollection services, bool explicitCall)
    {
        var marker = services.FirstOrDefault(d => d.ServiceType == typeof(ProfilingMarker))?.ImplementationInstance as ProfilingMarker;
        if (marker is null)
        {
            marker = new ProfilingMarker();
            services.AddSingleton(marker);
            services.AddSingleton<IConfigureOptions<TrooperProfilingOptions>>(sp => new TrooperProfilingOptionsSetup(sp));
            services.TryAddEnumerable(ServiceDescriptor.Singleton<IHostedService, TrooperProfilerHostedService>());
        }

        marker.Explicit |= explicitCall;
    }
}
