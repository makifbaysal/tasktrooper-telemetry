using System.Globalization;
using System.Reflection;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Options;

namespace TaskTrooper.Telemetry;

/// <summary>Fills <see cref="TaskTrooperOptions"/> from environment variables and the <c>TaskTrooper</c> config section.</summary>
internal sealed class TaskTrooperOptionsSetup : IConfigureOptions<TaskTrooperOptions>, IPostConfigureOptions<TaskTrooperOptions>
{
    private readonly IConfiguration? configuration;
    private readonly IHostEnvironment? environment;

    public TaskTrooperOptionsSetup(IServiceProvider services)
    {
        configuration = services.GetService(typeof(IConfiguration)) as IConfiguration;
        environment = services.GetService(typeof(IHostEnvironment)) as IHostEnvironment;
    }

    private string? Env(string key)
    {
        var v = configuration?[key];
        if (string.IsNullOrWhiteSpace(v)) v = System.Environment.GetEnvironmentVariable(key);
        return string.IsNullOrWhiteSpace(v) ? null : v.Trim();
    }

    public void Configure(TaskTrooperOptions o)
    {
        try
        {
            // 1) environment variables
            o.Dsn = Env("TROOPER_DSN") ?? o.Dsn;
            o.ServiceName = Env("TROOPER_SERVICE") ?? Env("OTEL_SERVICE_NAME") ?? o.ServiceName;
            o.ServiceVersion = Env("TROOPER_VERSION") ?? o.ServiceVersion;
            o.Environment = Env("TROOPER_ENVIRONMENT") ?? o.Environment;
            if (double.TryParse(Env("TROOPER_SAMPLE_RATE"), NumberStyles.Float, CultureInfo.InvariantCulture, out var rate)) o.SampleRate = rate;
            if (bool.TryParse(Env("TROOPER_ENABLED"), out var enabled)) o.Enabled = enabled;

            // 2) configuration section "TaskTrooper"
            var section = configuration?.GetSection(TaskTrooperOptions.SectionName);
            if (section is not null && section.Exists())
            {
                o.Dsn = section[nameof(o.Dsn)] ?? o.Dsn;
                o.ServiceName = section[nameof(o.ServiceName)] ?? o.ServiceName;
                o.ServiceVersion = section[nameof(o.ServiceVersion)] ?? o.ServiceVersion;
                o.Environment = section[nameof(o.Environment)] ?? o.Environment;
                if (double.TryParse(section[nameof(o.SampleRate)], NumberStyles.Float, CultureInfo.InvariantCulture, out var r)) o.SampleRate = r;
                if (bool.TryParse(section[nameof(o.Enabled)], out var e)) o.Enabled = e;
                if (bool.TryParse(section[nameof(o.EnableTraces)], out var t)) o.EnableTraces = t;
                if (bool.TryParse(section[nameof(o.EnableMetrics)], out var m)) o.EnableMetrics = m;
                if (bool.TryParse(section[nameof(o.EnableLogs)], out var l)) o.EnableLogs = l;
            }
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("options", e);
        }
    }

    // 3) code delegate runs between Configure and PostConfigure; defaults are filled last
    public void PostConfigure(string? name, TaskTrooperOptions o)
    {
        try
        {
            var entry = Assembly.GetEntryAssembly();
            o.ServiceName ??= environment?.ApplicationName ?? entry?.GetName().Name ?? "unknown_service:dotnet";
            o.ServiceVersion ??= entry?.GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion
                                 ?? entry?.GetName().Version?.ToString();
            o.Environment ??= environment?.EnvironmentName?.ToLowerInvariant();
            o.SampleRate = double.IsNaN(o.SampleRate) ? 1.0 : Math.Clamp(o.SampleRate, 0.0, 1.0);
        }
        catch (Exception e)
        {
            TrooperSelfDiagnostics.Error("options", e);
        }
    }
}
