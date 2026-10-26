using OpenTelemetry.Resources;

namespace TaskTrooper.Telemetry;

/// <summary>service.name/version, deployment.environment.name and telemetry.distro.* from <see cref="TaskTrooperOptions"/>.</summary>
internal sealed class TrooperResourceDetector : IResourceDetector
{
    internal static readonly string DistroVersion =
        typeof(TrooperResourceDetector).Assembly.GetName().Version?.ToString(3) ?? "0.0.0";

    private readonly TaskTrooperOptions options;

    public TrooperResourceDetector(TaskTrooperOptions options) => this.options = options;

    public Resource Detect()
    {
        var attrs = new Dictionary<string, object>
        {
            ["telemetry.distro.name"] = TrooperAttributes.DistroName,
            ["telemetry.distro.version"] = DistroVersion,
        };
        if (!string.IsNullOrEmpty(options.ServiceName)) attrs["service.name"] = options.ServiceName;
        if (!string.IsNullOrEmpty(options.ServiceVersion)) attrs["service.version"] = options.ServiceVersion;
        if (!string.IsNullOrEmpty(options.Environment)) attrs["deployment.environment.name"] = options.Environment;
        foreach (var kv in options.ResourceAttributes) attrs[kv.Key] = kv.Value;
        return new Resource(attrs);
    }
}
