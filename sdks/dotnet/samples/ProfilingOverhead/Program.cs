// Rough overhead check of TaskTrooper.Telemetry.Profiling (not a benchmark suite).
//   dotnet run -c Release -- [seconds per phase=60] [workers=cores/2]
// Phases: baseline → default duty cycle (10 s window every 60 s; use ≥ 60 s phases) → baseline → continuous window → baseline (noise check).
// Each "request" is a server Activity with a route, CPU work, some allocation, an async hop and a shared lock.
// Profiles go to a local fake ingest endpoint; CPU time per request and throughput are compared with the baselines.
using System.Diagnostics;
using System.IO.Compression;
using System.Net;
using System.Net.Sockets;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Options;
using OpenTelemetry.Trace;
using TaskTrooper.Telemetry;
using TaskTrooper.Telemetry.Profiling;

var seconds = args.Length > 0 ? int.Parse(args[0]) : 60;
var workers = args.Length > 1 ? int.Parse(args[1]) : Math.Max(1, Environment.ProcessorCount / 2);
var work = args.Length > 2 ? int.Parse(args[2]) : 2_000;
var source = new ActivitySource("Overhead");
var gate = new object();
long sink = 0;

var port = FreePort();
var listener = new HttpListener();
listener.Prefixes.Add($"http://127.0.0.1:{port}/");
listener.Start();
var received = new List<(string Query, string Body)>();
_ = Task.Run(async () =>
{
    while (true)
    {
        var ctx = await listener.GetContextAsync();
        using var gz = new GZipStream(ctx.Request.InputStream, CompressionMode.Decompress);
        var body = await new StreamReader(gz).ReadToEndAsync();
        lock (received) received.Add((ctx.Request.Url!.Query, body));
        ctx.Response.StatusCode = 200;
        ctx.Response.Close();
    }
});

var services = new ServiceCollection();
services.AddLogging();
services.AddTaskTrooper(o =>
{
    o.Dsn = $"http://k@127.0.0.1:{port}/1";
    o.ServiceName = "overhead";
    o.EnableMetrics = false;
    o.EnableLogs = false;
    o.EnableTraces = false; // keep span export out of the measurement; spans are still created and processed
}).AddSources("Overhead");
using var sp = services.BuildServiceProvider();
_ = sp.GetRequiredService<TracerProvider>();
var trooper = sp.GetRequiredService<IOptions<TaskTrooperOptions>>().Value;

Console.WriteLine($"cores={Environment.ProcessorCount} workers={workers} phase={seconds}s work={work}");
Run("warmup", Math.Min(5, seconds));
var baselines = new List<(double CpuPerReq, double Rps)> { Run("baseline", seconds) };

using (var p = new TrooperProfiler(new TrooperProfilingOptions { StartDelay = TimeSpan.Zero, Period = TimeSpan.FromSeconds(60), Duration = TimeSpan.FromSeconds(10) }, trooper))
{
    p.Start();
    var duty = Run("default 10s/60s", seconds);
    p.Stop();
    baselines.Add(Run("baseline", seconds));
    Report("default 10s/60s", duty);
}

using (var p = new TrooperProfiler(new TrooperProfilingOptions { StartDelay = TimeSpan.Zero, Period = TimeSpan.FromSeconds(seconds), Duration = TimeSpan.FromSeconds(seconds) }, trooper))
{
    p.Start();
    var cont = Run("continuous", seconds);
    p.Stop();
    baselines.Add(Run("baseline", seconds));
    Report("continuous", cont);
}

Console.WriteLine($"baseline spread: cpu/request {Spread(baselines.Select(b => b.CpuPerReq))}  throughput {Spread(baselines.Select(b => b.Rps))}");
lock (received)
{
    Console.WriteLine($"profiles uploaded: {received.Count}");
    foreach (var cpu in received.Where(r => r.Query.Contains("type=cpu")))
    {
        long total = 0, routed = 0;
        var inRoute = false;
        foreach (var line in cpu.Body.Split('\n', StringSplitOptions.RemoveEmptyEntries))
        {
            if (line.StartsWith('#')) { inRoute = line.Length > 2; continue; }
            var v = long.Parse(line[(line.LastIndexOf(' ') + 1)..]);
            total += v;
            if (inRoute) routed += v;
        }

        Console.WriteLine($"cpu profile: {total / 1e9:F1} s CPU, {routed * 100.0 / Math.Max(1, total):F0}% under a route; " +
                          string.Join(", ", cpu.Body.Split('\n').Where(l => l.StartsWith('#')).Distinct()));
    }
}

return;

(double CpuPerReq, double Rps) Run(string name, int secs)
{
    var clock = Stopwatch.StartNew();
    long done = 0;
    var cpu0 = Process.GetCurrentProcess().TotalProcessorTime;
    var tasks = Enumerable.Range(0, workers).Select(_ => Task.Run(async () =>
    {
        while (clock.Elapsed.TotalSeconds < secs)
        {
            await Request();
            Interlocked.Increment(ref done);
        }
    })).ToArray();
    Task.WaitAll(tasks);
    var cpu = (Process.GetCurrentProcess().TotalProcessorTime - cpu0).TotalMilliseconds;
    var r = (cpu / done, done / clock.Elapsed.TotalSeconds);
    Console.WriteLine($"{name,-14} requests/s={r.Item2,8:F0}  cpu ms/request={r.Item1:F4}");
    return r;
}

void Report(string name, (double CpuPerReq, double Rps) x)
{
    var b = (CpuPerReq: baselines.Average(v => v.CpuPerReq), Rps: baselines.Average(v => v.Rps));
    Console.WriteLine($"  → {name}: cpu/request {(x.CpuPerReq / b.CpuPerReq - 1) * 100:+0.0;-0.0}%  throughput {(x.Rps / b.Rps - 1) * 100:+0.0;-0.0}%");
}

static string Spread(IEnumerable<double> values)
{
    var v = values.ToList();
    return $"±{(v.Max() - v.Min()) / v.Average() * 50:F1}%";
}

async Task Request()
{
    using var a = source.StartActivity("GET /work/:id", ActivityKind.Server);
    a?.SetTag("http.request.method", "GET");
    TrooperSpan.SetRoute(a, "/work/:id");
    Compute(work);
    await Task.Yield();
    var buf = new List<byte[]>();
    for (var i = 0; i < 16; i++) buf.Add(new byte[512]);
    lock (gate) Interlocked.Add(ref sink, buf.Count);
    Compute(work);
}

static void Compute(int iterations)
{
    double x = 0;
    for (var i = 1; i < iterations * 100; i++) x += Math.Sqrt(i);
    if (x < 0) Console.WriteLine();
}

static int FreePort()
{
    var l = new TcpListener(IPAddress.Loopback, 0);
    l.Start();
    var p = ((IPEndPoint)l.LocalEndpoint).Port;
    l.Stop();
    return p;
}
