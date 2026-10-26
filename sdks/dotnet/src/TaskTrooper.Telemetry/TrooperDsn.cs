using System.Diagnostics.CodeAnalysis;

namespace TaskTrooper.Telemetry;

/// <summary>
/// Parsed DSN: <c>https://&lt;key&gt;@&lt;host&gt;[:port][/base]/&lt;project_id&gt;</c>.
/// The ingest endpoint is the DSN without user-info and project segment.
/// </summary>
public sealed record TrooperDsn(Uri Endpoint, string Key, string ProjectId)
{
    /// <summary>Endpoint for one OTLP signal, e.g. <c>v1/traces</c>.</summary>
    public Uri SignalEndpoint(string signalPath) =>
        new(Endpoint.AbsoluteUri.TrimEnd('/') + "/" + signalPath.TrimStart('/'));

    public static bool TryParse(string? dsn, [NotNullWhen(true)] out TrooperDsn? result)
    {
        result = null;
        if (string.IsNullOrWhiteSpace(dsn)) return false;
        if (!Uri.TryCreate(dsn.Trim(), UriKind.Absolute, out var uri)) return false;
        if (uri.Scheme != Uri.UriSchemeHttp && uri.Scheme != Uri.UriSchemeHttps) return false;

        var key = Uri.UnescapeDataString(uri.UserInfo.Split(':')[0]);
        if (key.Length == 0) return false;

        var segments = uri.AbsolutePath.Split('/', StringSplitOptions.RemoveEmptyEntries);
        if (segments.Length == 0) return false;
        var project = segments[^1];
        var basePath = string.Join('/', segments[..^1]);

        var builder = new UriBuilder(uri.Scheme, uri.Host, uri.Port, basePath.Length == 0 ? "/" : "/" + basePath + "/");
        result = new TrooperDsn(builder.Uri, key, project);
        return true;
    }

    public static TrooperDsn Parse(string dsn) =>
        TryParse(dsn, out var r) ? r : throw new FormatException("Invalid TaskTrooper DSN. Expected https://<key>@<host>[:port]/<project_id>.");

    public override string ToString() => $"{Endpoint} (project {ProjectId})";
}
