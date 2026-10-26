using System.Collections.Concurrent;
using System.Diagnostics;
using System.Net;
using System.Net.Sockets;

[assembly: CollectionBehavior(DisableTestParallelization = true)]

namespace TaskTrooper.Telemetry.Tests;

/// <summary>Minimal HTTP server capturing requests (OTLP endpoint / downstream service stand-in).</summary>
public sealed class CaptureServer : IDisposable
{
    private readonly HttpListener listener = new();
    private readonly CancellationTokenSource cts = new();
    private readonly Func<CapturedRequest, int> respond;

    public CaptureServer(Func<CapturedRequest, int>? respond = null)
    {
        this.respond = respond ?? (_ => 200);
        Port = FreePort();
        listener.Prefixes.Add($"http://127.0.0.1:{Port}/");
        listener.Start();
        _ = Task.Run(LoopAsync);
    }

    public int Port { get; }

    public string BaseUrl => $"http://127.0.0.1:{Port}";

    public ConcurrentQueue<CapturedRequest> Requests { get; } = new();

    public IEnumerable<CapturedRequest> ForPath(string path) => Requests.Where(r => r.Path == path);

    public async Task<CapturedRequest> WaitForAsync(string path, int count = 1, int timeoutMs = 10_000)
    {
        var sw = Stopwatch.StartNew();
        while (sw.ElapsedMilliseconds < timeoutMs)
        {
            var matches = ForPath(path).ToList();
            if (matches.Count >= count) return matches[count - 1];
            await Task.Delay(25);
        }

        throw new TimeoutException($"no request #{count} for {path}; got: {string.Join(", ", Requests.Select(r => r.Path))}");
    }

    private async Task LoopAsync()
    {
        while (!cts.IsCancellationRequested)
        {
            HttpListenerContext ctx;
            try
            {
                ctx = await listener.GetContextAsync();
            }
            catch
            {
                return;
            }

            using var ms = new MemoryStream();
            await ctx.Request.InputStream.CopyToAsync(ms);
            var headers = ctx.Request.Headers.AllKeys.Where(k => k is not null)
                .ToDictionary(k => k!.ToLowerInvariant(), k => ctx.Request.Headers[k]!);
            var req = new CapturedRequest(ctx.Request.HttpMethod, ctx.Request.Url!.AbsolutePath, headers, ms.ToArray());
            Requests.Enqueue(req);
            ctx.Response.StatusCode = respond(req);
            ctx.Response.Close();
        }
    }

    private static int FreePort()
    {
        var l = new TcpListener(IPAddress.Loopback, 0);
        l.Start();
        var port = ((IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        return port;
    }

    public void Dispose()
    {
        cts.Cancel();
        listener.Close();
    }
}

public sealed record CapturedRequest(string Method, string Path, IReadOnlyDictionary<string, string> Headers, byte[] Body)
{
    public string? Header(string name) => Headers.TryGetValue(name.ToLowerInvariant(), out var v) ? v : null;

    /// <summary>Protobuf stores strings as raw UTF-8, so a substring search is enough for assertions.</summary>
    public bool BodyContains(string text) =>
        System.Text.Encoding.UTF8.GetString(Body).Contains(text, StringComparison.Ordinal);
}

public static class ActivityExtensions
{
    public static object? Tag(this Activity a, string key) => a.GetTagItem(key);

    public static async Task<List<Activity>> WaitForAsync(this List<Activity> exported, Func<List<Activity>, bool> condition, int timeoutMs = 5000)
    {
        var sw = Stopwatch.StartNew();
        while (sw.ElapsedMilliseconds < timeoutMs)
        {
            List<Activity> snapshot;
            lock (exported) snapshot = exported.ToList();
            if (condition(snapshot)) return snapshot;
            await Task.Delay(20);
        }

        lock (exported) return exported.ToList();
    }
}
