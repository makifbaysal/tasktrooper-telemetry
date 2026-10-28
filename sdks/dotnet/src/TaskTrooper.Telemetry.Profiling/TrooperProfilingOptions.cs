using System.Globalization;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.Options;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>
/// Continuous profiler settings (contracts §24). Resolution: environment / configuration keys → code delegate.
/// Upload target, service, environment and version come from <see cref="TaskTrooperOptions"/>.
/// </summary>
public sealed class TrooperProfilingOptions
{
    /// <summary>
    /// On / off. Env <c>TROOPER_PROFILING</c>. Unset: on after <c>AddTaskTrooperProfiling()</c>, off when the package is
    /// only referenced (then <c>TROOPER_PROFILING=true</c> turns it on).
    /// </summary>
    public bool? Enabled { get; set; }

    /// <summary>Profile types. Env <c>TROOPER_PROFILING_TYPES</c> (<c>cpu,wall,alloc_space,lock</c>). Default <c>cpu,alloc_space,lock</c>.</summary>
    public ISet<ProfileType> Types { get; set; } = new HashSet<ProfileType>(ProfileTypes.Defaults);

    /// <summary>A profiling window starts every period. Env <c>TROOPER_PROFILING_PERIOD</c> (seconds). Default 60 s.</summary>
    public TimeSpan Period { get; set; } = TimeSpan.FromSeconds(60);

    /// <summary>
    /// Length of each EventPipe window (≤ period; equal to period = continuous). Env <c>TROOPER_PROFILING_CPU_DURATION</c>
    /// (seconds). Default 10 s — EventPipe samples every thread at 1 kHz, so the window is duty-cycled to stay within the
    /// overhead budget (&lt; 2 % CPU).
    /// </summary>
    public TimeSpan Duration { get; set; } = TimeSpan.FromSeconds(10);

    /// <summary>Delay before the first window (keeps profiling out of application start-up). Default 5 s.</summary>
    public TimeSpan StartDelay { get; set; } = TimeSpan.FromSeconds(5);

    /// <summary><c>host</c> query parameter. Default: machine name.</summary>
    public string? Host { get; set; }

    /// <summary>EventPipe buffer size; events are dropped (never blocking the app) when it is full. Default 64 MB.</summary>
    public int BufferSizeMb { get; set; } = 64;

    // test hooks
    internal int ProcessId { get; set; } = Environment.ProcessId;
    internal TimeSpan UploadRetryBaseDelay { get; set; } = TimeSpan.FromSeconds(1);
    internal HttpMessageHandler? UploadHandler { get; set; }

    /// <summary>Value of <c>TROOPER_PROFILING</c> (env / configuration), applied when <see cref="Enabled"/> is unset.</summary>
    internal bool? EnvEnabled { get; set; }

    internal TimeSpan EffectiveDuration => Duration <= TimeSpan.Zero || Duration > Period ? Period : Duration;
}

/// <summary>Reads <c>TROOPER_PROFILING*</c> from configuration keys / environment variables.</summary>
internal sealed class TrooperProfilingOptionsSetup(IServiceProvider services) : IConfigureOptions<TrooperProfilingOptions>
{
    public void Configure(TrooperProfilingOptions o)
    {
        try
        {
            var configuration = services.GetService(typeof(IConfiguration)) as IConfiguration;
            string? Env(string key)
            {
                var v = configuration?[key];
                if (string.IsNullOrWhiteSpace(v)) v = Environment.GetEnvironmentVariable(key);
                return string.IsNullOrWhiteSpace(v) ? null : v.Trim();
            }

            if (bool.TryParse(Env("TROOPER_PROFILING"), out var enabled)) o.EnvEnabled = enabled;
            if (ProfileTypes.ParseList(Env("TROOPER_PROFILING_TYPES")) is { } types) o.Types = types;
            if (double.TryParse(Env("TROOPER_PROFILING_PERIOD"), NumberStyles.Float, CultureInfo.InvariantCulture, out var p) && p > 0)
                o.Period = TimeSpan.FromSeconds(p);
            if (double.TryParse(Env("TROOPER_PROFILING_CPU_DURATION"), NumberStyles.Float, CultureInfo.InvariantCulture, out var d) && d > 0)
                o.Duration = TimeSpan.FromSeconds(d);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("profiler", e);
        }
    }
}
