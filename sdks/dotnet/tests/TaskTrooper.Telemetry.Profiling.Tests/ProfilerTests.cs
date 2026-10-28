using System.Diagnostics;
using System.Runtime.CompilerServices;
using System.Web;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Options;
using OpenTelemetry.Trace;
using TaskTrooper.Telemetry.Profiling;
using TaskTrooper.Telemetry.Tests;

namespace TaskTrooper.Telemetry.Profiling.Tests;

public static class Workload
{
    public static double Sink;

    [MethodImpl(MethodImplOptions.NoInlining)]
    public static void BurnCpu(TimeSpan duration)
    {
        var sw = Stopwatch.StartNew();
        var x = 0.0;
        while (sw.Elapsed < duration)
            for (var i = 1; i < 100_000; i++) x += Math.Sqrt(i) / i;
        Sink = x;
    }

    [MethodImpl(MethodImplOptions.NoInlining)]
    public static void Allocate(TimeSpan duration)
    {
        var sw = Stopwatch.StartNew();
        while (sw.Elapsed < duration)
        {
            var list = new List<byte[]>();
            for (var i = 0; i < 1_000; i++) list.Add(new byte[1_024]);
            GC.KeepAlive(list);
        }
    }

    [MethodImpl(MethodImplOptions.NoInlining)]
    public static void Contend(object gate, TimeSpan duration)
    {
        var sw = Stopwatch.StartNew();
        while (sw.Elapsed < duration)
        {
            lock (gate) Thread.Sleep(5);
        }
    }
}

public class ProfilerTests
{
    private static readonly ActivitySource Source = new("Test.Profiling");

    private static ServiceProvider Build(string dsn, Action<TrooperProfilingOptions>? profiling, IDictionary<string, string?>? config = null, bool explicitCall = true)
    {
        var services = new ServiceCollection();
        services.AddSingleton<IConfiguration>(new ConfigurationBuilder().AddInMemoryCollection(config ?? new Dictionary<string, string?>()).Build());
        services.AddLogging();
        services.AddTaskTrooper(o =>
        {
            o.Dsn = dsn;
            o.ServiceName = "profiled-api";
            o.ServiceVersion = "9.9.9";
            o.Environment = "test";
            o.EnableMetrics = false;
            o.EnableLogs = false;
        }).AddSources("Test.Profiling");
        if (explicitCall) services.AddTaskTrooperProfiling(profiling);
        else if (profiling is not null) services.Configure(profiling);
        return services.BuildServiceProvider();
    }

    private static TrooperProfilerHostedService Hosted(IServiceProvider sp) =>
        sp.GetServices<IHostedService>().OfType<TrooperProfilerHostedService>().Single();

    [Fact]
    public async Task Profiles_a_cpu_burning_request_and_attributes_it_to_its_route()
    {
        using var server = new CaptureServer();
        await using var sp = Build($"http://tt_key@127.0.0.1:{server.Port}/1", o =>
        {
            o.StartDelay = TimeSpan.Zero;
            o.Period = TimeSpan.FromSeconds(6);
            o.Duration = TimeSpan.FromSeconds(6);
            o.Types = new HashSet<ProfileType> { ProfileType.Cpu, ProfileType.Wall, ProfileType.AllocSpace, ProfileType.Lock };
        });
        _ = sp.GetRequiredService<TracerProvider>();
        var hosted = Hosted(sp);
        await hosted.StartAsync(default);
        Assert.NotNull(hosted.Profiler);
        Assert.True(hosted.Profiler!.IsRunning);
        await Task.Delay(1_500); // session + rundown up

        var gate = new object();
        var contenders = Enumerable.Range(0, 2).Select(_ => Task.Run(() => Workload.Contend(gate, TimeSpan.FromSeconds(2)))).ToArray();
        await ServeBurnRequest();
        Workload.Allocate(TimeSpan.FromSeconds(1));
        await Task.WhenAll(contenders);

        var cpu = await WaitForType(server, "cpu");
        await hosted.StopAsync(default);
        var q = HttpUtility.ParseQueryString(cpu.Query);
        Assert.Equal("nanoseconds", q["unit"]);
        Assert.Equal("profiled-api", q["service"]);
        Assert.Equal("test", q["env"]);
        Assert.Equal("9.9.9", q["version"]);
        Assert.Equal("dotnet", q["runtime"]);
        Assert.Equal("tt_key", cpu.Header("x-trooper-key"));
        Assert.True(long.Parse(q["end"]!) > long.Parse(q["start"]!));

        var folded = UploaderTests.Gunzip(cpu.Body);
        var section = Section(folded, "# trooper.route=/burn trooper.method=GET");
        var hot = section.Where(l => l.Contains("Workload.BurnCpu")).ToList();
        Assert.True(hot.Count > 0, "BurnCpu not under /burn:\n" + Head(folded));
        var hotNs = hot.Sum(l => long.Parse(l[(l.LastIndexOf(' ') + 1)..]));
        Assert.True(hotNs > 500_000_000, $"BurnCpu only {hotNs} ns");
        Assert.DoesNotContain("trooper-profiler", folded);
        Assert.All(folded.Split('\n', StringSplitOptions.RemoveEmptyEntries).Where(l => !l.StartsWith('#')),
            l => Assert.True(l.Split(' ')[0].Split(';').Length <= FoldedProfile.MaxDepth));

        var alloc = UploaderTests.Gunzip((await WaitForType(server, "alloc_space")).Body);
        Assert.Contains("Workload.Allocate", alloc);
        var lockProfile = UploaderTests.Gunzip((await WaitForType(server, "lock")).Body);
        Assert.Contains("Workload.Contend", lockProfile);
        var wall = UploaderTests.Gunzip((await WaitForType(server, "wall")).Body);
        Assert.Contains("Workload.BurnCpu", wall);
    }

    private static async Task ServeBurnRequest()
    {
        using var activity = Source.StartActivity("GET /burn", ActivityKind.Server);
        Assert.NotNull(activity);
        activity!.SetTag("http.request.method", "GET");
        TrooperSpan.SetRoute(activity, "/burn");
        await Task.Yield();
        await Task.Run(() => Workload.BurnCpu(TimeSpan.FromSeconds(2))); // continuation on a pool thread
    }

    private static async Task<CapturedRequest> WaitForType(CaptureServer server, string type)
    {
        var sw = Stopwatch.StartNew();
        while (sw.Elapsed < TimeSpan.FromSeconds(30))
        {
            var match = server.ForPath("/v1/profiles").FirstOrDefault(r => HttpUtility.ParseQueryString(r.Query)["type"] == type);
            if (match is not null) return match;
            await Task.Delay(100);
        }

        throw new TimeoutException($"no {type} profile; got: {string.Join(", ", server.Requests.Select(r => r.Query))}");
    }

    private static List<string> Section(string folded, string header)
    {
        var lines = new List<string>();
        var inside = false;
        foreach (var line in folded.Split('\n', StringSplitOptions.RemoveEmptyEntries))
        {
            if (line.StartsWith('#')) inside = line == header;
            else if (inside) lines.Add(line);
        }

        return lines;
    }

    private static string Head(string folded) => string.Join('\n', folded.Split('\n').Take(40));

    [Fact]
    public void Disabled_diagnostics_never_throw_and_do_not_start()
    {
        var previous = Environment.GetEnvironmentVariable("DOTNET_EnableDiagnostics");
        try
        {
            Environment.SetEnvironmentVariable("DOTNET_EnableDiagnostics", "0");
            Assert.False(TrooperProfiler.IsSupported(out var reason));
            Assert.Contains("DOTNET_EnableDiagnostics=0", reason);
            using var profiler = new TrooperProfiler(new TrooperProfilingOptions(), new TaskTrooperOptions { Dsn = "http://k@127.0.0.1:1/1" });
            Assert.False(profiler.Start());
            Assert.False(profiler.IsRunning);
        }
        finally
        {
            Environment.SetEnvironmentVariable("DOTNET_EnableDiagnostics", previous);
        }
    }

    [Fact]
    public void Missing_ipc_endpoint_stops_the_profiler_quietly()
    {
        // a PID without a diagnostics socket behaves like a runtime started with DOTNET_EnableDiagnostics=0
        var options = new TrooperProfilingOptions { StartDelay = TimeSpan.Zero, Period = TimeSpan.FromSeconds(1), ProcessId = 999_999 };
        using var profiler = new TrooperProfiler(options, new TaskTrooperOptions { Dsn = "http://k@127.0.0.1:1/1" });
        Assert.True(profiler.Start());
        var sw = Stopwatch.StartNew();
        while (profiler.IsRunning && sw.Elapsed < TimeSpan.FromSeconds(20)) Thread.Sleep(50);
        Assert.False(profiler.IsRunning);
        Assert.Equal(0, profiler.Windows);
        Assert.False(ProfileRoutes.Enabled);

        // the process-wide slot is released: another profiler may start
        using var again = new TrooperProfiler(options, new TaskTrooperOptions { Dsn = "http://k@127.0.0.1:1/1" });
        Assert.True(again.Start());
        again.Stop();
    }

    [Fact]
    public void No_dsn_or_disabled_sdk_does_not_start()
    {
        using var noDsn = new TrooperProfiler(new TrooperProfilingOptions(), new TaskTrooperOptions());
        Assert.False(noDsn.Start());
        using var off = new TrooperProfiler(new TrooperProfilingOptions(), new TaskTrooperOptions { Dsn = "http://k@h/1", Enabled = false });
        Assert.False(off.Start());
    }

    [Fact]
    public async Task Referenced_package_is_registered_by_AddTaskTrooper_and_enabled_by_TROOPER_PROFILING()
    {
        await using (var sp = Build("http://k@127.0.0.1:1/1", null, explicitCall: false))
        {
            var hosted = Hosted(sp); // auto-registered by the core package
            Assert.False(hosted.IsEnabled(sp.GetRequiredService<IOptions<TrooperProfilingOptions>>().Value));
        }

        var config = new Dictionary<string, string?>
        {
            ["TROOPER_PROFILING"] = "true",
            ["TROOPER_PROFILING_TYPES"] = "cpu,wall",
            ["TROOPER_PROFILING_PERIOD"] = "30",
            ["TROOPER_PROFILING_CPU_DURATION"] = "5",
        };
        await using (var sp = Build("http://k@127.0.0.1:1/1", null, config, explicitCall: false))
        {
            var o = sp.GetRequiredService<IOptions<TrooperProfilingOptions>>().Value;
            Assert.True(Hosted(sp).IsEnabled(o));
            Assert.Equal(new[] { ProfileType.Cpu, ProfileType.Wall }, o.Types.OrderBy(t => t));
            Assert.Equal(TimeSpan.FromSeconds(30), o.Period);
            Assert.Equal(TimeSpan.FromSeconds(5), o.EffectiveDuration);
        }
    }

    [Fact]
    public async Task Explicit_call_enables_profiling_unless_turned_off()
    {
        await using (var sp = Build("http://k@127.0.0.1:1/1", null))
        {
            var o = sp.GetRequiredService<IOptions<TrooperProfilingOptions>>().Value;
            Assert.True(Hosted(sp).IsEnabled(o));
            Assert.Equal(ProfileTypes.Defaults.OrderBy(t => t), o.Types.OrderBy(t => t));
            Assert.Single(sp.GetServices<IHostedService>().OfType<TrooperProfilerHostedService>());
        }

        await using (var sp = Build("http://k@127.0.0.1:1/1", null, new Dictionary<string, string?> { ["TROOPER_PROFILING"] = "false" }))
            Assert.False(Hosted(sp).IsEnabled(sp.GetRequiredService<IOptions<TrooperProfilingOptions>>().Value));

        await using (var sp = Build("http://k@127.0.0.1:1/1", o => o.Enabled = false))
            Assert.False(Hosted(sp).IsEnabled(sp.GetRequiredService<IOptions<TrooperProfilingOptions>>().Value));
    }
}
