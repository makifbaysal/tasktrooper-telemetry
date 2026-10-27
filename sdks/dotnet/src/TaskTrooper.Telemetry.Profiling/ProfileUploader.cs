using System.IO.Compression;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using OpenTelemetry;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>Resource metadata sent as query parameters of <c>/v1/profiles</c>.</summary>
internal sealed record ProfileMetadata(string Service, string? Environment, string? Version, string? Host);

/// <summary>
/// <c>POST /v1/profiles?format=folded&amp;type=…&amp;unit=…&amp;service=…&amp;env=…&amp;version=…&amp;host=…&amp;runtime=dotnet&amp;start=…&amp;end=…</c>
/// (contracts §24), gzip body, <c>x-trooper-key</c>. A failed profile is dropped (no queue). After 429 / 5xx / network
/// errors further uploads are skipped for an exponential, jittered backoff 1 s → 60 s (<c>Retry-After</c> honoured);
/// other 4xx drop only that profile (contracts §2). Never throws.
/// </summary>
internal sealed class ProfileUploader : IDisposable
{
    internal static readonly TimeSpan MaxBackoff = TimeSpan.FromSeconds(60);

    private readonly Uri endpoint;
    private readonly string key;
    private readonly ProfileMetadata metadata;
    private readonly TimeSpan baseDelay;
    private readonly HttpClient http;
    private int failures;
    private DateTimeOffset blockedUntil;

    public ProfileUploader(TrooperDsn dsn, ProfileMetadata metadata, TimeSpan timeout, TimeSpan baseDelay, HttpMessageHandler? handler = null)
    {
        endpoint = dsn.SignalEndpoint("v1/profiles");
        key = dsn.Key;
        this.metadata = metadata;
        this.baseDelay = baseDelay;
        http = new HttpClient(handler ?? new SocketsHttpHandler { PooledConnectionLifetime = TimeSpan.FromMinutes(5) }, disposeHandler: handler is null)
        {
            Timeout = timeout,
        };
    }

    public enum Result { Sent, Dropped, SkippedBackoff, Empty }

    internal TimeSpan BackoffRemaining
    {
        get
        {
            lock (this)
            {
                var left = blockedUntil - DateTimeOffset.UtcNow;
                return left > TimeSpan.Zero ? left : TimeSpan.Zero;
            }
        }
    }

    public Result Upload(FoldedProfile? profile, long startMs, long endMs)
    {
        if (profile is null || profile.IsEmpty) return Result.Empty;
        lock (this)
        {
            if (DateTimeOffset.UtcNow < blockedUntil) return Result.SkippedBackoff;
            try
            {
                using var request = new HttpRequestMessage(HttpMethod.Post, Url(endpoint, metadata, profile.Type, startMs, endMs));
                request.Headers.TryAddWithoutValidation("x-trooper-key", key);
                request.Content = new ByteArrayContent(Gzip(profile.Render()));
                request.Content.Headers.ContentType = new MediaTypeHeaderValue("text/plain") { CharSet = "utf-8" };
                request.Content.Headers.ContentEncoding.Add("gzip");

                HttpResponseMessage response;
                using (SuppressInstrumentationScope.Begin()) // no HttpClient span for our own upload
                {
                    response = http.Send(request);
                }

                using (response)
                {
                    var status = (int)response.StatusCode;
                    if (status is >= 200 and < 300)
                    {
                        failures = 0;
                        return Result.Sent;
                    }

                    if (response.StatusCode == HttpStatusCode.TooManyRequests || status >= 500) Backoff(response.Headers.RetryAfter);
                    TrooperSelfDiagnostics.Error("exporter.profiles");
                    return Result.Dropped;
                }
            }
            catch (Exception e)
            {
                Backoff(null); // network error / timeout
                TrooperSelfDiagnostics.Error("exporter.profiles", e);
                return Result.Dropped;
            }
        }
    }

    private void Backoff(RetryConditionHeaderValue? retryAfter)
    {
        failures++;
        var cap = TimeSpan.FromTicks(Math.Min(MaxBackoff.Ticks, (long)(baseDelay.Ticks * Math.Pow(2, Math.Min(16, failures - 1)))));
        var delay = cap / 2 + cap / 2 * Random.Shared.NextDouble(); // equal jitter
        var hinted = retryAfter?.Delta ?? (retryAfter?.Date is { } date ? date - DateTimeOffset.UtcNow : null);
        if (hinted is { } h && h > delay) delay = h < MaxBackoff ? h : MaxBackoff;
        blockedUntil = DateTimeOffset.UtcNow + delay;
    }

    /// <summary>Upload URL with metadata query parameters; empty values are omitted.</summary>
    internal static Uri Url(Uri endpoint, ProfileMetadata m, ProfileType type, long startMs, long endMs)
    {
        var q = new (string Key, string? Value)[]
        {
            ("format", "folded"),
            ("type", type.WireName()),
            ("unit", type.Unit()),
            ("service", m.Service),
            ("env", m.Environment),
            ("version", m.Version),
            ("host", m.Host),
            ("runtime", "dotnet"),
            ("start", startMs.ToString(System.Globalization.CultureInfo.InvariantCulture)),
            ("end", endMs.ToString(System.Globalization.CultureInfo.InvariantCulture)),
        };
        var sb = new StringBuilder(endpoint.AbsoluteUri);
        var sep = '?';
        foreach (var (k, v) in q)
        {
            if (string.IsNullOrWhiteSpace(v)) continue;
            sb.Append(sep).Append(k).Append('=').Append(Uri.EscapeDataString(v));
            sep = '&';
        }

        return new Uri(sb.ToString());
    }

    internal static byte[] Gzip(string text)
    {
        using var ms = new MemoryStream(Math.Max(64, text.Length / 4));
        using (var gz = new GZipStream(ms, CompressionLevel.Fastest, leaveOpen: true))
        {
            var bytes = Encoding.UTF8.GetBytes(text);
            gz.Write(bytes, 0, bytes.Length);
        }

        return ms.ToArray();
    }

    public void Dispose() => http.Dispose();
}
