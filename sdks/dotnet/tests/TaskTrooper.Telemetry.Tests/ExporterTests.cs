using System.Diagnostics;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using OpenTelemetry;
using OpenTelemetry.Logs;
using OpenTelemetry.Metrics;
using OpenTelemetry.Resources;
using OpenTelemetry.Trace;

namespace TaskTrooper.Telemetry.Tests;

public class ExporterTests
{
    private static readonly ActivitySource Source = new("Test.Exporter");

    private static ServiceProvider Build(string dsn, Action<TaskTrooperOptions>? extra = null)
    {
        var services = new ServiceCollection();
        services.AddLogging(l => l.SetMinimumLevel(LogLevel.Information));
        services.AddTaskTrooper(o =>
        {
            o.Dsn = dsn;
            o.ServiceName = "orders-api";
            o.ServiceVersion = "1.2.3";
            o.Environment = "staging";
            o.RetryBaseDelay = TimeSpan.FromMilliseconds(20);
            extra?.Invoke(o);
        }).AddSources("Test.Exporter");
        var sp = services.BuildServiceProvider();
        _ = sp.GetRequiredService<TracerProvider>();
        _ = sp.GetRequiredService<MeterProvider>();
        return sp;
    }

    [Fact]
    public async Task Exports_traces_metrics_and_logs_over_otlp_http_with_key_header()
    {
        using var server = new CaptureServer();
        await using var sp = Build($"http://secret-key@127.0.0.1:{server.Port}/42");

        using (var a = Source.StartActivity("work", ActivityKind.Server)) a?.SetTag(TrooperAttributes.Route, "/work");
        sp.GetRequiredService<ILoggerFactory>().CreateLogger("test").LogWarning("hello {Who}", "trooper");
        sp.GetRequiredService<TracerProvider>().ForceFlush();
        sp.GetRequiredService<MeterProvider>().ForceFlush();
        sp.GetRequiredService<LoggerProvider>().ForceFlush();

        var traces = await server.WaitForAsync("/v1/traces");
        Assert.Equal("POST", traces.Method);
        Assert.Equal("secret-key", traces.Header("x-trooper-key"));
        Assert.Equal("application/x-protobuf", traces.Header("content-type"));
        Assert.True(traces.BodyContains("orders-api"));
        Assert.True(traces.BodyContains("trooper-dotnet"));
        Assert.True(traces.BodyContains("deployment.environment.name"));
        Assert.True(traces.BodyContains("trooper.sample_rate"));

        var logs = await server.WaitForAsync("/v1/logs");
        Assert.Equal("secret-key", logs.Header("x-trooper-key"));
        Assert.True(logs.BodyContains("hello trooper"));

        var metrics = await server.WaitForAsync("/v1/metrics");
        Assert.Equal("secret-key", metrics.Header("x-trooper-key"));
    }

    [Fact]
    public async Task Retries_on_503_and_drops_on_400()
    {
        var calls = 0;
        using var server = new CaptureServer(r => r.Path == "/v1/traces" && Interlocked.Increment(ref calls) == 1 ? 503 : 200);
        await using (var sp = Build($"http://k@127.0.0.1:{server.Port}/1"))
        {
            using (Source.StartActivity("work")) { }
            sp.GetRequiredService<TracerProvider>().ForceFlush();
            await server.WaitForAsync("/v1/traces", count: 2);
        }

        using var bad = new CaptureServer(r => r.Path == "/v1/traces" ? 400 : 200);
        await using (var sp = Build($"http://k@127.0.0.1:{bad.Port}/1"))
        {
            using (Source.StartActivity("work")) { }
            sp.GetRequiredService<TracerProvider>().ForceFlush();
            await bad.WaitForAsync("/v1/traces");
            await Task.Delay(300);
            Assert.Single(bad.ForPath("/v1/traces"));
        }
    }

    [Fact]
    public async Task Without_dsn_nothing_is_exported_and_host_works()
    {
        await using var sp = Build("not-a-dsn");
        using (var a = Source.StartActivity("work")) Assert.NotNull(a);
        Assert.True(sp.GetRequiredService<TracerProvider>().ForceFlush());
    }

    [Fact]
    public void Resource_has_service_and_distro_attributes()
    {
        using var sp = Build("http://k@127.0.0.1:1/1");
        var resource = sp.GetRequiredService<TracerProvider>().GetResource().Attributes.ToDictionary(kv => kv.Key, kv => kv.Value);
        Assert.Equal("orders-api", resource["service.name"]);
        Assert.Equal("1.2.3", resource["service.version"]);
        Assert.Equal("staging", resource["deployment.environment.name"]);
        Assert.Equal("trooper-dotnet", resource["telemetry.distro.name"]);
    }

    [Fact]
    public void Options_resolve_env_then_config_section_then_code()
    {
        static TaskTrooperOptions Resolve(Dictionary<string, string?> config, Action<TaskTrooperOptions>? code = null)
        {
            var services = new ServiceCollection();
            services.AddSingleton<IConfiguration>(new ConfigurationBuilder().AddInMemoryCollection(config).Build());
            services.AddTaskTrooper(code);
            using var sp = services.BuildServiceProvider();
            return sp.GetRequiredService<IOptions<TaskTrooperOptions>>().Value;
        }

        var env = new Dictionary<string, string?> { ["TROOPER_DSN"] = "https://env@h/1", ["TROOPER_SERVICE"] = "env-svc", ["TROOPER_SAMPLE_RATE"] = "0.25" };
        var o = Resolve(env);
        Assert.Equal("https://env@h/1", o.Dsn);
        Assert.Equal("env-svc", o.ServiceName);
        Assert.Equal(0.25, o.SampleRate);

        env["TaskTrooper:Dsn"] = "https://section@h/2";
        env["TaskTrooper:SampleRate"] = "7"; // clamped
        o = Resolve(env);
        Assert.Equal("https://section@h/2", o.Dsn);
        Assert.Equal(1.0, o.SampleRate);

        o = Resolve(env, x => x.Dsn = "https://code@h/3");
        Assert.Equal("https://code@h/3", o.Dsn);
        Assert.Equal("env-svc", o.ServiceName);
    }
}
